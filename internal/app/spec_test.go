package app

import (
	"context"
	"fmt"
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
	on := true
	errA, recA, repA := queueSpecs(&on) // 三条队列都得声明（漏了 RegisterSites 直接 panic）
	errB, recB, repB := queueSpecs(&on)
	a.RegisterSites(
		SiteSpec{
			Key: "a", BaseURL: "https://a.example/", AutoRepeat: &noAuto,
			ErrorQueue: errA, RecoverQueue: recA, RepeatQueue: repA,
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
			Key:        "b",
			ErrorQueue: errB, RecoverQueue: recB, RepeatQueue: repB,
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

// queueSpecs 三条队列都开着的最小声明：RegisterSites 要求**三条都得写**（见 SiteSpec 的注释）。
func queueSpecs(on *bool) (*ErrorQueueSpec, *RecoverQueueSpec, *RepeatQueueSpec) {
	return &ErrorQueueSpec{Enabled: on, WorkerCount: 1, Interval: "4h", MaxRetry: 3, BatchSize: 100},
		&RecoverQueueSpec{Enabled: on, WorkerCount: 1, BatchSize: 100},
		&RepeatQueueSpec{Enabled: on, WorkerCount: 1, Interval: "2h", BatchSize: 100}
}

// 三队列声明：写全了原样解析成引擎侧的生效值；关掉只写 Enabled: &false 一行，解析出来是零值（= 不跑）。
func TestSiteQueueSpecsResolveToEngineValues(t *testing.T) {
	on, off := true, false

	site := SiteSpec{Key: "a",
		ErrorQueue:   &ErrorQueueSpec{Enabled: &on, WorkerCount: 3, Interval: "30m", MaxRetry: 5, BatchSize: 100},
		RecoverQueue: &RecoverQueueSpec{Enabled: &on, WorkerCount: 2, BatchSize: 50},
		RepeatQueue:  &RepeatQueueSpec{Enabled: &on, WorkerCount: 1, Interval: "2h", BatchSize: 200},
	}
	wantError := config.ErrorQueueConfig{Enabled: true, WorkerCount: 3, Interval: 30 * time.Minute, MaxRetry: 5, BatchSize: 100}
	if got, err := site.errorQueueConfig(); err != nil || got != wantError {
		t.Fatalf("errorQueueConfig = %+v / %v, want %+v", got, err, wantError)
	}
	wantRecover := config.RecoverQueueConfig{Enabled: true, WorkerCount: 2, BatchSize: 50}
	if got, err := site.recoverQueueConfig(); err != nil || got != wantRecover {
		t.Fatalf("recoverQueueConfig = %+v / %v, want %+v", got, err, wantRecover)
	}
	wantRepeat := config.RepeatQueueConfig{Enabled: true, WorkerCount: 1, Interval: 2 * time.Hour, BatchSize: 200}
	if got, err := site.repeatQueueConfig(); err != nil || got != wantRepeat {
		t.Fatalf("repeatQueueConfig = %+v / %v, want %+v", got, err, wantRepeat)
	}

	// 关着的队列不看参数：其余字段一个都不写也照样解析（引擎那边 = 这一站不跑这条队列）
	closed := SiteSpec{Key: "a",
		ErrorQueue:   &ErrorQueueSpec{Enabled: &off},
		RecoverQueue: &RecoverQueueSpec{Enabled: &off},
		RepeatQueue:  &RepeatQueueSpec{Enabled: &off},
	}
	if got, err := closed.errorQueueConfig(); err != nil || got != (config.ErrorQueueConfig{}) {
		t.Fatalf("关着的错误队列应当是零值：%+v / %v", got, err)
	}
	if got, err := closed.recoverQueueConfig(); err != nil || got != (config.RecoverQueueConfig{}) {
		t.Fatalf("关着的启动恢复应当是零值：%+v / %v", got, err)
	}
	if got, err := closed.repeatQueueConfig(); err != nil || got != (config.RepeatQueueConfig{}) {
		t.Fatalf("关着的轮询队列应当是零值：%+v / %v", got, err)
	}
}

// 漏写 / 缺项 / 越界一律报错（RegisterSites 会 panic 点名），且必须说清是哪条队列的哪一项。
func TestSiteQueueSpecsRejectIncompleteDeclarations(t *testing.T) {
	on := true
	full := func() *ErrorQueueSpec {
		return &ErrorQueueSpec{Enabled: &on, WorkerCount: 1, Interval: "4h", MaxRetry: 3, BatchSize: 100}
	}
	bad := []struct {
		name string
		want string
		run  func() error
	}{
		{"漏了 ErrorQueue 整条", "ErrorQueue", func() error {
			_, err := SiteSpec{}.errorQueueConfig()
			return err
		}},
		{"漏了 RecoverQueue 整条", "RecoverQueue", func() error {
			_, err := SiteSpec{}.recoverQueueConfig()
			return err
		}},
		{"漏了 RepeatQueue 整条", "RepeatQueue", func() error {
			_, err := SiteSpec{}.repeatQueueConfig()
			return err
		}},
		{"Enabled 漏写", "Enabled", func() error {
			q := full()
			q.Enabled = nil
			_, err := SiteSpec{ErrorQueue: q}.errorQueueConfig()
			return err
		}},
		{"WorkerCount 为 0", "WorkerCount", func() error {
			q := full()
			q.WorkerCount = 0
			_, err := SiteSpec{ErrorQueue: q}.errorQueueConfig()
			return err
		}},
		{"BatchSize 为 0", "BatchSize", func() error {
			q := full()
			q.BatchSize = 0
			_, err := SiteSpec{ErrorQueue: q}.errorQueueConfig()
			return err
		}},
		{"MaxRetry 为负", "MaxRetry", func() error {
			q := full()
			q.MaxRetry = -1
			_, err := SiteSpec{ErrorQueue: q}.errorQueueConfig()
			return err
		}},
		{"Interval 漏写", "Interval", func() error {
			q := full()
			q.Interval = ""
			_, err := SiteSpec{ErrorQueue: q}.errorQueueConfig()
			return err
		}},
		{"Interval 解析不了", "Interval", func() error {
			q := full()
			q.Interval = "nonsense"
			_, err := SiteSpec{ErrorQueue: q}.errorQueueConfig()
			return err
		}},
		{"Interval 为负", "Interval", func() error {
			q := full()
			q.Interval = "-1h"
			_, err := SiteSpec{ErrorQueue: q}.errorQueueConfig()
			return err
		}},
		{"recover 的 WorkerCount 为 0", "WorkerCount", func() error {
			_, err := SiteSpec{RecoverQueue: &RecoverQueueSpec{Enabled: &on, BatchSize: 100}}.recoverQueueConfig()
			return err
		}},
		{"repeat 的 Interval 漏写", "Interval", func() error {
			_, err := SiteSpec{RepeatQueue: &RepeatQueueSpec{Enabled: &on, WorkerCount: 1, BatchSize: 100}}.repeatQueueConfig()
			return err
		}},
		{"repeat 的 Interval 为 0", "Interval", func() error {
			_, err := SiteSpec{RepeatQueue: &RepeatQueueSpec{Enabled: &on, WorkerCount: 1, Interval: "0", BatchSize: 100}}.repeatQueueConfig()
			return err
		}},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if err == nil {
				t.Fatal("应当报错")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("报错要说清是哪一项（%s）：%v", tc.want, err)
			}
		})
	}
}

// 三条队列是**注册时的硬要求**：漏一条、开了缺项、或给未归属（Key 为空）的声明配队列，RegisterSites 直接 panic。
func TestRegisterSitesRequiresQueueDeclarations(t *testing.T) {
	on := true
	all := func() (*ErrorQueueSpec, *RecoverQueueSpec, *RepeatQueueSpec) { return queueSpecs(&on) }
	cases := []struct {
		name string
		want string // panic 信息里必须点名到哪儿
		site func() SiteSpec
	}{
		{"漏了 ErrorQueue", "ErrorQueue", func() SiteSpec {
			_, rec, rep := all()
			return SiteSpec{Key: "a", RecoverQueue: rec, RepeatQueue: rep}
		}},
		{"漏了 RecoverQueue", "RecoverQueue", func() SiteSpec {
			errQ, _, rep := all()
			return SiteSpec{Key: "a", ErrorQueue: errQ, RepeatQueue: rep}
		}},
		{"漏了 RepeatQueue", "RepeatQueue", func() SiteSpec {
			errQ, rec, _ := all()
			return SiteSpec{Key: "a", ErrorQueue: errQ, RecoverQueue: rec}
		}},
		{"开了却缺参数", "WorkerCount", func() SiteSpec {
			errQ, rec, rep := all()
			errQ.WorkerCount = 0
			return SiteSpec{Key: "a", ErrorQueue: errQ, RecoverQueue: rec, RepeatQueue: rep}
		}},
		{"未归属（Key 为空）却配了队列", "未归属", func() SiteSpec {
			errQ, rec, rep := all()
			return SiteSpec{ErrorQueue: errQ, RecoverQueue: rec, RepeatQueue: rep}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Crawler.DrainInterval = time.Hour
			e := noDBEngine(t, cfg)
			t.Cleanup(func() { e.Stop(time.Millisecond) })
			a := &App{
				Config: cfg, Engine: e,
				Logger: &loggers.LoggerSet{Engine: quietLogger()},
				sites:  map[string]core.Site{},
			}
			defer func() {
				r := recover()
				if r == nil {
					t.Fatalf("%s：应当 panic", tc.name)
				}
				if !strings.Contains(fmt.Sprint(r), tc.want) {
					t.Fatalf("panic 信息里缺 %q：%v", tc.want, r)
				}
			}()
			a.RegisterSites(tc.site())
		})
	}
}
