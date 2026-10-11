package app

import (
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/pkg/loggers"
)

// CheckSites：不起库、不建池、不改状态，把启动时会 panic 的问题**一次收集全**（而不是报第一条就停）。
// 这条是给"升级后确认声明写对了"用的 —— 起服务要连库，这一步不用。
func TestCheckSitesCollectsAllProblems(t *testing.T) {
	on := true
	errQ, recQ, repQ := queueSpecs(&on)

	// 站点 b 的错误队列：时长写成中文了
	badErr := *errQ
	badErr.Interval = "4 小时"

	err := CheckSites(nil,
		// 站点 a：三队列一条没写；阶段名与 b 撞车
		SiteSpec{Key: "a", Stages: []StageSpec{
			{Fetcher: namedFetcher{name: "same"}, WorkerCount: 1, QueueSize: 8},
		}},
		// 站点 b：队列时长写错；两个阶段里一个撞名（报重复）、另一个名字没撞但 Delay 写法错
		SiteSpec{Key: "b", ErrorQueue: &badErr, RecoverQueue: recQ, RepeatQueue: repQ, Stages: []StageSpec{
			{Fetcher: namedFetcher{name: "same"}, WorkerCount: 1, QueueSize: 8, Delay: "5 分钟"},
			{Fetcher: namedFetcher{name: "b_detail"}, WorkerCount: 1, QueueSize: 8, Delay: "5 分钟"},
		}},
	)
	if err == nil {
		t.Fatal("一堆问题应当报错")
	}
	msg := err.Error()
	for _, want := range []string{
		`站点 "a" 的错误队列`,             // 三队列漏写
		"ErrorQueue.Interval 无法解析", // 时长写错
		"重复声明",                     // 阶段名 a / b 都是 same
		"Delay",                    // 延迟写法
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误汇总里应当有 %q（一次报全，不是报第一条就停）：\n%s", want, msg)
		}
	}
}

// 合法声明 → nil；cfg 传 nil（没有全局默认）也要能通过。
func TestCheckSitesAcceptsGoodDeclarations(t *testing.T) {
	on := true
	errQ, recQ, repQ := queueSpecs(&on)
	site := SiteSpec{
		Key: "a", ErrorQueue: errQ, RecoverQueue: recQ, RepeatQueue: repQ,
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
}

// 站点熔断阈值那条规则（enabled=true 时 threshold 必须 > 0）与 breaker.New 共用一份判据：
// 阈值只在全局默认里时，传了 cfg 就该放行；不传 cfg 就该报出来 —— 而不是等到真起引擎才炸。
func TestCheckSitesChecksSiteBreakerThreshold(t *testing.T) {
	on := true
	errQ, recQ, repQ := queueSpecs(&on)
	site := SiteSpec{
		Key: "a", ErrorQueue: errQ, RecoverQueue: recQ, RepeatQueue: repQ,
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
	on := true
	errQ, recQ, repQ := queueSpecs(&on)
	cfg := &config.Config{}
	cfg.Crawler.DrainInterval = time.Hour
	e := noDBEngine(t, cfg)
	t.Cleanup(func() { e.Stop(time.Millisecond) })
	a := &App{Config: cfg, Engine: e, Logger: &loggers.LoggerSet{Engine: quietLogger()}, sites: map[string]core.Site{}}

	good := SiteSpec{Key: "good", ErrorQueue: errQ, RecoverQueue: recQ, RepeatQueue: repQ,
		Stages: []StageSpec{{Fetcher: namedFetcher{name: "good_catalog"}, WorkerCount: 1, QueueSize: 8}}}
	bad := SiteSpec{Key: "bad"} // 三队列漏写 → 启动即失败

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
