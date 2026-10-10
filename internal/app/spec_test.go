package app

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/engine"
	"github.com/ydtg1993/papa/v3/pkg/loggers"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// quietLogger 吞掉输出的 logger：注册那条链会打几条 Info/Warn，测试输出里不需要它们。
func quietLogger() *logrus.Logger {
	l := logrus.New()
	l.SetOutput(io.Discard)
	return l
}

// namedFetcher 一个阶段名可配的最小 fetcher（注册那条链要多个不同名的阶段）。
type namedFetcher struct{ name string }

func (f namedFetcher) GetStage() string { return f.name }

func (f namedFetcher) FetchHandler(context.Context, *engine.Task, *engine.Engine) error { return nil }

// noDBEngine 造一个**连不上库**的真引擎：阶段注册（AddStage / SetSite / SetSiteBreaker）不碰库，
// 够把 RegisterSites 这条链测到底。NewEngine 里那次 loadActiveTasks 会失败并记日志（吞掉，不致命）。
func noDBEngine(t *testing.T, cfg *config.Config) *engine.Engine {
	t.Helper()
	db, err := gorm.Open(mysql.New(mysql.Config{
		DSN:                       "root@tcp(127.0.0.1:9)/none?charset=utf8mb4&parseTime=True",
		SkipInitializeWithVersion: true,
	}), &gorm.Config{DisableAutomaticPing: true, Logger: gormlogger.Discard})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	return engine.NewEngine(db, cfg, &loggers.LoggerSet{
		Engine: quietLogger(), DB: quietLogger(), Monitor: quietLogger(), Sys: quietLogger(),
	})
}

// RegisterSites 这条链把声明落到三个地方：引擎的阶段表（含 AutoStart 策略）、站点快照、按站的熔断阈值。
// 参数解析与校验的纯函数部分在 app_extra_test.go 里；这里测的是**接线**。
func TestRegisterSitesWiresStagesSitesAndBreakers(t *testing.T) {
	cfg := &config.Config{}
	cfg.Crawler.DrainInterval = time.Hour
	cfg.Crawler.Breaker = config.BreakerConfig{Enabled: true, Window: 5 * time.Minute, Threshold: 50}

	e := noDBEngine(t, cfg)
	// Stop 关掉引擎 ctx（delayDispatcher 那条协程随之退出）
	t.Cleanup(func() { e.Stop(time.Millisecond) })
	a := &App{
		Config: cfg,
		Engine: e,
		Logger: &loggers.LoggerSet{Engine: quietLogger()},
		sites:  map[string]core.Site{},
	}

	entry := &entryStageFetcher{} // 阶段 review：实现了 SubmitEntries（入口任务）
	noAuto := false               // 站点 a 显式关掉自动轮询（站点 b 不写 = 自动）
	a.RegisterSites(
		SiteSpec{
			Key: "a", BaseURL: "https://a.example/", AutoRepeat: &noAuto,
			Headers:            map[string]string{"User-Agent": "ua-a"},
			RestrictedKeywords: []string{"安全验证"},
			Breaker:            &BreakerSpec{Enabled: true, Threshold: 7, Window: "2m"},
			Stages: []StageSpec{{
				Fetcher: entry, WorkerCount: 2, QueueSize: 16,
				Delay: "5m", Retry: RetrySpec{MaxAttempts: 2, Backoff: "3s"},
				AutoStart: true,
			}},
		},
		SiteSpec{
			Key:    "b",
			Stages: []StageSpec{{Fetcher: namedFetcher{name: "detail"}, WorkerCount: 1, QueueSize: 8}},
		},
	)

	// ① 站点快照：Key/BaseURL/站点级头/受限页文案/是否自动轮询
	got, ok := a.Site("a")
	if !ok || got.BaseURL != "https://a.example/" || got.Headers["User-Agent"] != "ua-a" {
		t.Fatalf("App.Site(a) = %+v", got)
	}
	if got.AutoRepeat {
		t.Fatalf("声明里显式写了 AutoRepeat: false，快照应当是关的：%+v", got)
	}
	if b, ok := a.Site("b"); !ok || !b.AutoRepeat {
		t.Fatalf("没写 AutoRepeat 的站点应当按自动处理：%+v", b)
	}
	if len(got.RestrictedKeywords) != 1 || got.RestrictedKeywords[0] != "安全验证" {
		t.Fatalf("站点受限页文案没带上：%+v", got)
	}
	if _, ok := a.Site("ghost"); ok {
		t.Fatal("没声明过的站点不该有快照")
	}
	// 引擎侧也拿得到（handler 用 engine.Site(task.Site)）
	if viaEngine, ok := e.Site("a"); !ok || viaEngine.BaseURL != got.BaseURL {
		t.Fatalf("engine.Site(a) = %+v, %v", viaEngine, ok)
	}

	// ② 按站的熔断阈值：a 用自己的 7/2m，b 没单配 → 落在默认 scope 的那把（50/5m）
	sts := e.BreakerStatuses()
	if sts["a"].Threshold != 7 || sts["a"].Window != 2*time.Minute {
		t.Fatalf("站点 a 的熔断阈值没生效：%+v", sts["a"])
	}
	if sts[""].Threshold != 50 || sts[""].Window != 5*time.Minute {
		t.Fatalf("默认 scope 应当用 crawler.breaker：%+v", sts[""])
	}

	// ③ 阶段注册了、AutoStart=true 的入口真的被调（两趟遍历 + 策略在声明层这一整套）
	e.ApplyRegisterStage()
	if !entry.called {
		t.Fatal("AutoStart=true 的阶段应当在启动时投入口任务")
	}
	// 站点声明（BaseURL / Headers）也一并交给入口回调 —— fetcher 不必自己存副本。
	// 少了这一条，站点值就只能在 fetcher 里再写一份常量，改了会漂。
	if entry.site.BaseURL != "https://a.example/" || entry.site.Headers["User-Agent"] != "ua-a" {
		t.Fatalf("入口回调拿到的站点快照不对：%+v", entry.site)
	}
}

// BreakerSpec 合到 crawler.breaker 上的语义：**Enabled 不看默认值**（写了就由它说了算），
// 阈值/窗口没给才落回默认。
func TestBreakerSpecToConfig(t *testing.T) {
	def := config.BreakerConfig{Enabled: true, Window: 5 * time.Minute, Threshold: 50}

	// 全不给：阈值窗口用默认，但 Enabled 是 false（站点自己说了算 —— 于是"全局开着、某站关掉"能表达）
	got, err := BreakerSpec{}.toConfig(def)
	if err != nil {
		t.Fatal(err)
	}
	if got.Enabled || got.Threshold != 50 || got.Window != 5*time.Minute {
		t.Fatalf("空 BreakerSpec 应当只继承阈值与窗口：%+v", got)
	}

	// 给了阈值与窗口：覆盖
	got, err = BreakerSpec{Enabled: true, Threshold: 7, Window: "2m"}.toConfig(def)
	if err != nil || !got.Enabled || got.Threshold != 7 || got.Window != 2*time.Minute {
		t.Fatalf("覆盖没生效：%+v %v", got, err)
	}

	// 默认里 Window 是零值：走 WindowOrDefault（否则 time.NewTicker(0) 会在闸门里 panic）
	got, err = BreakerSpec{Enabled: true}.toConfig(config.BreakerConfig{Threshold: 3})
	if err != nil || got.Window <= 0 {
		t.Fatalf("窗口应当有默认值：%+v %v", got, err)
	}
	if got.Threshold != 3 {
		t.Fatalf("阈值应当继承默认：%+v", got)
	}

	// 窗口写错：报错（不是在运行期崩）
	if _, err := (BreakerSpec{Enabled: true, Window: "两分钟"}).toConfig(def); err == nil {
		t.Fatal("窗口解析失败应当报错")
	}
}

// 站点级队列参数：不写 = 原样用全局那份；写了逐字段覆盖；**用不上的键直接报错**（不静默忽略）。
func TestSiteQueueSpecToConfig(t *testing.T) {
	defErr := config.ErrorQueueConfig{Enabled: true, WorkerCount: 3, Interval: 4 * time.Hour, MaxRetry: 5, BatchSize: 100}
	defRec := config.RecoverQueueConfig{Enabled: true, WorkerCount: 2, BatchSize: 50}
	defRep := config.RepeatQueueConfig{Enabled: true, WorkerCount: 1, Interval: 2 * time.Hour, BatchSize: 200}

	// 不写 = 全局那份（原样返回，不改一个字段）
	bare := SiteSpec{Key: "a"}
	if got, err := bare.errorQueueConfig(defErr); err != nil || got != defErr {
		t.Fatalf("不写错误队列参数时应原样用全局：%+v / %v", got, err)
	}
	if got, err := bare.recoverQueueConfig(defRec); err != nil || got != defRec {
		t.Fatalf("不写启动恢复参数时应原样用全局：%+v / %v", got, err)
	}
	if got, err := bare.repeatQueueConfig(defRep); err != nil || got != defRep {
		t.Fatalf("不写轮询队列参数时应原样用全局：%+v / %v", got, err)
	}

	// 写了 = 逐字段覆盖（没提到的键仍取全局那份）
	off := false
	full := SiteSpec{Key: "a", ErrorQueue: &QueueSpec{
		Enabled: &off, WorkerCount: 9, Interval: "30m", MaxRetry: 1, BatchSize: 10,
	}}
	got, err := full.errorQueueConfig(defErr)
	if err != nil {
		t.Fatalf("errorQueueConfig = %v", err)
	}
	want := config.ErrorQueueConfig{Enabled: false, WorkerCount: 9, Interval: 30 * time.Minute, MaxRetry: 1, BatchSize: 10}
	if got != want {
		t.Fatalf("覆盖结果 = %+v, want %+v", got, want)
	}
	partial := SiteSpec{Key: "a", RepeatQueue: &QueueSpec{WorkerCount: 4, Interval: "45m"}}
	if got, err := partial.repeatQueueConfig(defRep); err != nil ||
		got.WorkerCount != 4 || got.Interval != 45*time.Minute || got.BatchSize != defRep.BatchSize || !got.Enabled {
		t.Fatalf("只写两项时其余应取全局：%+v / %v", got, err)
	}

	// 用不上的键 / 解析不了的时长：启动就报
	bad := []struct {
		name string
		run  func() error
	}{
		{"错误队列的 interval 解析不了", func() error {
			_, err := SiteSpec{ErrorQueue: &QueueSpec{Interval: "nonsense"}}.errorQueueConfig(defErr)
			return err
		}},
		{"轮询队列的 interval 解析不了", func() error {
			_, err := SiteSpec{RepeatQueue: &QueueSpec{Interval: "nonsense"}}.repeatQueueConfig(defRep)
			return err
		}},
		{"recover_queue 没有 interval", func() error {
			_, err := SiteSpec{RecoverQueue: &QueueSpec{Interval: "1h"}}.recoverQueueConfig(defRec)
			return err
		}},
		{"recover_queue 没有 max_retry", func() error {
			_, err := SiteSpec{RecoverQueue: &QueueSpec{MaxRetry: 3}}.recoverQueueConfig(defRec)
			return err
		}},
		{"站点级 repeat 的开关是 AutoRepeat", func() error {
			on := true
			_, err := SiteSpec{RepeatQueue: &QueueSpec{Enabled: &on}}.repeatQueueConfig(defRep)
			return err
		}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.run(); err == nil {
				t.Fatal("应当报错")
			} else if !strings.Contains(err.Error(), "Interval") && !strings.Contains(err.Error(), "MaxRetry") &&
				!strings.Contains(err.Error(), "AutoRepeat") {
				t.Fatalf("报错要说清是哪一项：%v", err)
			}
		})
	}
}
