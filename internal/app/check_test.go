package app

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/engine"
	"github.com/ydtg1993/papa/v3/pkg/loggers"
	"github.com/ydtg1993/papa/v3/pkg/middleware/filedown"
	"github.com/ydtg1993/papa/v3/pkg/middleware/m3u8"
)

// CheckSites：不起库、不建池、不改状态，把启动时会 panic 的问题**一次收集全**（而不是报第一条就停）。
// 这条是给"升级后确认声明写对了"用的 —— 起服务要连库，这一步不用。
func TestCheckSitesCollectsAllProblems(t *testing.T) {
	err := CheckSites(nil,
		// 站点 a：与 b 用同一个阶段名 —— 这是**合法**的（阶段名按站点作用域）
		SiteSpec{Key: "a", Stages: []StageSpec{
			{Fetcher: namedFetcher{name: "same"}, WorkerCount: 1, QueueSize: 8},
		}},
		// 站点 b：站内自己把 same 声明了两次（报重复）、另一个名字没撞但 Delay 写法错
		SiteSpec{Key: "b", Stages: []StageSpec{
			{Fetcher: namedFetcher{name: "same"}, WorkerCount: 1, QueueSize: 8},
			{Fetcher: namedFetcher{name: "same"}, WorkerCount: 1, QueueSize: 8},
			{Fetcher: namedFetcher{name: "b_detail"}, WorkerCount: 1, QueueSize: 8, Delay: "5 分钟"},
		}},
	)
	if err == nil {
		t.Fatal("一堆问题应当报错")
	}
	msg := err.Error()
	for _, want := range []string{
		"重复声明",  // 站点 b 里 same 声明了两次
		"Delay", // 延迟写法
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误汇总里应当有 %q（一次报全，不是报第一条就停）：\n%s", want, msg)
		}
	}
}

// 合法声明 → nil；cfg 传 nil（没有全局默认）也要能通过。
func TestCheckSitesAcceptsGoodDeclarations(t *testing.T) {
	site := SiteSpec{
		Key: "a",
		Stages: []StageSpec{{
			Fetcher: namedFetcher{name: "a_catalog"}, WorkerCount: 1, QueueSize: 8,
			Delay: "5m", Retry: RetrySpec{MaxAttempts: 2, Backoff: "30s"},
		}},
	}
	if err := CheckSites(&config.Config{}, site); err != nil {
		t.Fatalf("合法声明不该报错：%v", err)
	}
	if err := CheckSites(nil, site); err != nil {
		t.Fatalf("cfg 传 nil（没有全局默认）也该通过：%v", err)
	}

	// 阶段名按站点作用域：两个站都叫 a_catalog 也要通过（这是 `papa check` 直接跑的路径）
	other := SiteSpec{
		Key: "b",
		Stages: []StageSpec{{
			Fetcher: namedFetcher{name: "a_catalog"}, WorkerCount: 1, QueueSize: 8,
		}},
	}
	if err := CheckSites(&config.Config{}, site, other); err != nil {
		t.Fatalf("两个站同名阶段不该报错：%v", err)
	}
}

// 站点熔断阈值那条规则（enabled=true 时 threshold 必须 > 0）与 breaker.New 共用一份判据：
// 阈值只在全局默认里时，传了 cfg 就该放行；不传 cfg 就该报出来 —— 而不是等到真起引擎才炸。
func TestCheckSitesChecksSiteBreakerThreshold(t *testing.T) {
	site := SiteSpec{
		Key:     "a",
		Breaker: &BreakerSpec{Enabled: true}, // 阈值/窗口都留给默认那份
		Stages:  []StageSpec{{Fetcher: namedFetcher{name: "a_x"}, WorkerCount: 1, QueueSize: 8}},
	}

	cfg := &config.Config{}
	cfg.Crawler.Breaker = config.BreakerConfig{Enabled: true, Window: 5 * time.Minute, Threshold: 50}
	if err := CheckSites(cfg, site); err != nil {
		t.Fatalf("阈值取全局默认时不该报错：%v", err)
	}

	err := CheckSites(nil, site)
	if err == nil || !strings.Contains(err.Error(), "threshold 必须 > 0") {
		t.Fatalf("没有全局阈值时应当报 threshold：%v", err)
	}
}

// 声明有问题时**一个字节都不落地**：原来是一边注册一边炸，前几个站已经进引擎了。
func TestRegisterSitesAppliesNothingWhenDeclarationIsBad(t *testing.T) {
	cfg := &config.Config{}
	cfg.Crawler.DrainInterval = time.Hour
	e := noDBEngine(t, cfg)
	t.Cleanup(func() { e.Stop(time.Millisecond) })
	a := &App{Config: cfg, Engine: e, Logger: &loggers.LoggerSet{Engine: quietLogger()}, sites: map[string]core.Site{}}

	good := SiteSpec{Key: "good",
		Stages: []StageSpec{{Fetcher: namedFetcher{name: "good_catalog"}, WorkerCount: 1, QueueSize: 8}}}
	bad := SiteSpec{Key: "bad",
		Stages: []StageSpec{{Fetcher: namedFetcher{name: "bad_catalog"}, WorkerCount: 0, QueueSize: 8}}} // WorkerCount=0 → 启动即失败

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("有问题的声明应当 panic")
			}
		}()
		a.RegisterSites(good, bad)
	}()

	if _, ok := e.Site("good"); ok {
		t.Error("同批里有非法声明时，合法的那个也不该被注册（要么全落地、要么一个都不落）")
	}
	if _, ok := e.GetStageStats()["good_catalog"]; ok {
		t.Error("阶段同样不该被注册")
	}
}

// 下载器覆盖的自查输出：只列**改过**的项，站点层与阶段层分开说 —— 四层覆盖 + 随手造实例之后，
// "到底哪份生效"只能靠它一眼看清（`papa check` 会把这些行打出来）。
func TestDescribeDownloadOverlaysListsOnlyOverrides(t *testing.T) {
	dir := "./downloads/example"
	concurrent := 3
	mode := m3u8.OutputSegments
	lines := DescribeDownloadOverlays(SiteSpec{
		Key:      "example",
		BaseURL:  "https://example.com/",
		M3U8:     &m3u8.Overlay{OutputDir: &dir},
		FileDown: &filedown.Overlay{MaxConcurrent: &concurrent},
		Stages: []StageSpec{
			{Fetcher: stageNamed{"catalog"}},                                     // 一句没覆盖 → 不出现在结果里
			{Fetcher: stageNamed{"episode"}, M3U8: &m3u8.Overlay{Output: &mode}}, // 阶段层覆盖 → 单独一行
		},
	})
	joined := strings.Join(lines, " | ")
	for _, want := range []string{
		"站点 example: m3u8{output_dir=./downloads/example} filedown{max_concurrent=3}",
		"阶段 example:episode: m3u8{output=segments}",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("缺少 %q ｜ 实得：%s", want, joined)
		}
	}
	if strings.Contains(joined, "catalog") {
		t.Errorf("没覆盖的阶段不该出现：%s", joined)
	}
	if got := DescribeDownloadOverlays(SiteSpec{Key: "plain"}); len(got) != 0 {
		t.Errorf("一句没覆盖的站点不该产出任何行：%v", got)
	}
}

// stageNamed 一个只有阶段名的假 fetcher（自查输出只用到 GetStage）。
type stageNamed struct{ name string }

func (s stageNamed) GetStage() string { return s.name }

func (s stageNamed) FetchHandler(context.Context, *engine.Task, *engine.Engine) error { return nil }
