package app

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/v2/config"
	"github.com/ydtg1993/papa/v2/engine"
	"github.com/ydtg1993/papa/v2/internal/database"
	"github.com/ydtg1993/papa/v2/pkg/loggers"
)

/* ---------- 初始化选项 ---------- */

func TestOptionsSetFields(t *testing.T) {
	var a App
	if err := WithConfigPath("configs/x.yaml")(&a); err != nil {
		t.Fatalf("WithConfigPath = %v", err)
	}
	if a.configPath != "configs/x.yaml" {
		t.Fatalf("configPath = %q", a.configPath)
	}

	if err := WithModels(&struct{}{}, &struct{}{})(&a); err != nil {
		t.Fatalf("WithModels = %v", err)
	}
	if len(a.extraModels) != 2 {
		t.Fatalf("WithModels 应追加两个模型，实得 %d", len(a.extraModels))
	}

	// UseModels 与 WithModels 等价，区别只是能在 New 之后调用（脚手架就是那样登记的）
	a.UseModels(&struct{}{})
	if len(a.extraModels) != 3 {
		t.Fatalf("UseModels 应追加到同一份清单，实得 %d", len(a.extraModels))
	}
}

/* ---------- 白名单解析 ---------- */

// 文件里每行一个 IP/CIDR，空行与 # 注释忽略。
func TestReadWhitelistFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "whitelist")
	content := "# 注释\n10.0.0.1\n\n  192.168.1.0/24  \n# 又一行注释\n::1\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	got := readWhitelistFile(path)
	want := []string{"10.0.0.1", "192.168.1.0/24", "::1"}
	if len(got) != len(want) {
		t.Fatalf("readWhitelistFile = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 项 = %q, want %q（顺序保持文件序，值要 trim）", i, got[i], want[i])
		}
	}

	// 文件不存在 → nil（调用方据此回退内联配置）
	if readWhitelistFile(filepath.Join(dir, "nope")) != nil {
		t.Fatal("文件不存在应返回 nil")
	}
}

// **已知的 fail-open**（见 问题分析.md「空 whitelist 文件 = 放开」）：
// 文件存在但解析后为空时，返回的是**非 nil 空切片**，于是被当成"已配置"采用 → ipAllowed 恒 true。
//
// 这条钉的是现状，不是"应该如此"。要改成回退内联 whitelist 时，请连这条一起改。
func TestReadWhitelistFileEmptyMeansAllowAll(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty")
	if err := os.WriteFile(path, []byte("# 只有注释\n\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	got := readWhitelistFile(path)
	if got == nil {
		t.Fatal("现状：空文件返回非 nil 空切片（这正是 fail-open 的成因）")
	}
	if len(got) != 0 {
		t.Fatalf("应解析出 0 条，实得 %v", got)
	}

	// 经过 resolveWhitelist 之后，内联 whitelist 就被这份"空"顶掉了
	a := &App{}
	resolved := a.resolveWhitelist(config.ServerConfig{
		Whitelist:     []string{"10.0.0.1"},
		WhitelistFile: path,
	})
	if len(resolved) != 0 {
		t.Fatalf("现状：空文件会顶掉内联白名单，实得 %v", resolved)
	}
}

// 文件优先于内联；文件不存在时回退内联。
func TestResolveWhitelistPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "whitelist")
	if err := os.WriteFile(path, []byte("172.16.0.1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	a := &App{}
	cfg := config.ServerConfig{Whitelist: []string{"10.0.0.1"}, WhitelistFile: path}
	if got := a.resolveWhitelist(cfg); len(got) != 1 || got[0] != "172.16.0.1" {
		t.Fatalf("文件应优先，实得 %v", got)
	}

	cfg.WhitelistFile = filepath.Join(dir, "missing")
	if got := a.resolveWhitelist(cfg); len(got) != 1 || got[0] != "10.0.0.1" {
		t.Fatalf("文件读不到时应回退内联，实得 %v", got)
	}

	cfg = config.ServerConfig{}
	if got := a.resolveWhitelist(cfg); len(got) != 0 {
		t.Fatalf("两边都没有时返回空，实得 %v", got)
	}
}

/* ---------- 页面 key 校验 ---------- */

// key 会进 DOM id 与 URL，所以限制成字母/数字/下划线/连字符。
func TestValidPageKey(t *testing.T) {
	ok := []string{"review", "review-2", "review_v2", "A1", "0"}
	for _, k := range ok {
		if !validPageKey(k) {
			t.Errorf("validPageKey(%q) = false, want true", k)
		}
	}
	bad := []string{"", "re view", "re/view", "re.view", "re?x", "中文", "a\nb", "re#"}
	for _, k := range bad {
		if validPageKey(k) {
			t.Errorf("validPageKey(%q) = true, want false", k)
		}
	}
}

/* ---------- 注入脚本与样式 ---------- */

func TestUseScriptAndCSSIgnoreBlank(t *testing.T) {
	a := &App{}
	a.UseScript("")
	a.UseScript("   \n\t ")
	a.UseCSS("")
	a.UseCSS("\n")

	if len(a.extraJS) != 0 || len(a.extraCSS) != 0 {
		t.Fatalf("空白内容不该被记下来：js=%v css=%v", a.extraJS, a.extraCSS)
	}

	a.UseScript("window.x = 1;")
	a.UseCSS(".x { color: red; }")
	if len(a.extraJS) != 1 || len(a.extraCSS) != 1 {
		t.Fatalf("正常内容应被追加：js=%v css=%v", a.extraJS, a.extraCSS)
	}
}

// 读不到文件直接 panic：这是部署问题（路径写错 / 文件没打进去），
// 静默跳过会让"样式怎么没生效"变成一个查不动的问题。
func TestUseScriptFileAndCSSFile(t *testing.T) {
	dir := t.TempDir()
	js := filepath.Join(dir, "custom.js")
	css := filepath.Join(dir, "custom.css")
	if err := os.WriteFile(js, []byte("window.injected = 1;"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(css, []byte(".injected{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	a := &App{}
	a.UseScriptFile(js)
	a.UseCSSFile(css)

	if len(a.extraJS) != 1 || !strings.Contains(a.extraJS[0], "injected") {
		t.Fatalf("JS 没读进来：%v", a.extraJS)
	}
	if len(a.extraCSS) != 1 || !strings.Contains(a.extraCSS[0], ".injected") {
		t.Fatalf("CSS 没读进来：%v", a.extraCSS)
	}

	for _, c := range []struct {
		name string
		use  func()
	}{
		{"JS 文件不存在", func() { (&App{}).UseScriptFile(filepath.Join(dir, "nope.js")) }},
		{"CSS 文件不存在", func() { (&App{}).UseCSSFile(filepath.Join(dir, "nope.css")) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("读不到文件应当 panic（部署问题不该静默）")
				}
			}()
			c.use()
		})
	}
}

// UseRouter 的 nil 回调与路由组累加已在 router_test.go 里覆盖。

func TestUseRouterAndTablesAccumulate(t *testing.T) {
	a := &App{}
	a.UseRouter(func(*Router) {})
	a.UseRouter(func(*Router) {})
	if len(a.routers) != 2 {
		t.Fatalf("routers = %d, want 2", len(a.routers))
	}
}

/* ---------- RegisterStage ---------- */

// 声明与配置不一致时**启动即失败**，而不是等到某条任务提交进来才发现。
func TestRegisterStagePanicsOnBadConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  map[string]config.StageConfig
	}{
		{"阶段没在配置里声明", map[string]config.StageConfig{}},
		{"worker_count 非正", map[string]config.StageConfig{"review": {WorkerCount: 0, QueueSize: 4}}},
		{"queue_size 非正", map[string]config.StageConfig{"review": {WorkerCount: 1, QueueSize: 0}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := &App{Config: &config.Config{Crawler: config.CrawlerConfig{Stages: c.cfg}}}
			defer func() {
				if recover() == nil {
					t.Fatal("应当在注册时 panic")
				}
			}()
			a.RegisterStage(stubStageFetcher{}, nil)
		})
	}
}

// 校验全部发生在碰 Engine 之前 —— 所以一个还没初始化的 App 也能测出这些 panic。
// 反过来也说明：合法的阶段会往下走到 a.Engine.AddStage，需要真引擎，这里不构造。

/* ---------- RegisterCronJob + schedule ---------- */

func TestRegisterCronJobAccumulates(t *testing.T) {
	a := &App{}
	a.RegisterCronJob("a", "0 3 * * * *", func() {})
	a.RegisterCronJob("b", "@every 1h", func() {})
	if len(a.customJobs) != 2 {
		t.Fatalf("customJobs = %d, want 2", len(a.customJobs))
	}
	if a.customJobs[0].name != "a" || a.customJobs[0].schedule != "0 3 * * * *" {
		t.Fatalf("第一条任务 = %+v", a.customJobs[0])
	}
}

// 没有自定义任务时 schedule 直接返回：不建调度器（建了会去 LoadLocation）。
func TestScheduleNoopWithoutJobs(t *testing.T) {
	a := &App{}
	a.schedule(t.Context()) // 不该 panic、不该起协程
}

// 有时区与任务时把调度器起起来，ctx 取消后停掉。
// Engine 传 nil —— NewScheduler 只存指针不解引用，这条正好也钉住了这一点。
func TestScheduleStartsAndStops(t *testing.T) {
	a := &App{
		Config: &config.Config{Scheduler: config.SchedulerConfig{Timezone: "UTC"}},
		Logger: &loggers.LoggerSet{Scheduler: loggersStub()},
	}
	fired := make(chan struct{}, 4)
	a.RegisterCronJob("tick", "@every 1s", func() { fired <- struct{}{} })

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.schedule(ctx)

	// 先等它真的跑一次再取消 —— 取消早了调度器会被 Stop 掉，任务根本没机会触发
	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("注册的定时任务没有跑起来")
	}

	cancel()
	time.Sleep(50 * time.Millisecond) // 让 Stop 那一支走完
}

// 非法的 cron 表达式只记日志，不 panic —— 一个写错的任务不该把整个应用拦在启动阶段。
func TestScheduleLogsBadSpecAndKeepsGoing(t *testing.T) {
	// 用能抓输出的 logger：这条用例的名字就承诺了"把这条坏 spec 记下来"，
	// 而原来的 loggerStub 是 io.Discard —— 断言不了任何东西，只有"不该 panic"。
	var buf bytes.Buffer
	log := logrus.New()
	log.SetOutput(&buf)
	log.SetLevel(logrus.ErrorLevel)

	a := &App{
		Config: &config.Config{Scheduler: config.SchedulerConfig{Timezone: "UTC"}},
		Logger: &loggers.LoggerSet{Scheduler: log},
	}
	a.RegisterCronJob("bad", "这不是 cron", func() {})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a.schedule(ctx) // 不该 panic，也不该因为一条坏 spec 就整体不跑

	if got := buf.String(); !strings.Contains(got, "failed to add custom job bad") {
		t.Fatalf("应当把这条坏 spec 连同任务名记下来，实得日志：%q", got)
	}
}

/* ---------- NewApp 的失败路径 ---------- */

// 配置读不到时报的是配置的错，而不是继续往下连库。
func TestNewAppRejectsMissingConfig(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("PAPA_CONFIG", "")

	_, err := NewApp(WithConfigPath(filepath.Join(t.TempDir(), "nope.yaml")))
	if err == nil {
		t.Fatal("配置不存在应当报错")
	}
	if !strings.Contains(err.Error(), "load config") {
		t.Fatalf("错误信息 = %v", err)
	}
}

// 配置能读、但库连不上时报的是数据库的错 —— 两层错误要能分得清。
func TestNewAppReportsDatabaseFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// 127.0.0.1:1 会立刻拒绝连接，不会挂住
	cfg := "db:\n  driver: mysql\n  dsn: \"u:p@tcp(127.0.0.1:1)/x\"\n  max_open_conns: 1\n  max_idle_conns: 1\nlog:\n  dir: " + filepath.ToSlash(filepath.Join(dir, "logs")) + "\n"
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := NewApp(WithConfigPath(path))
	if err == nil {
		t.Fatal("连不上库应当报错")
	}
	if !strings.Contains(err.Error(), "connect to database") {
		t.Fatalf("错误信息 = %v", err)
	}
}

// 不支持的驱动同样在建连之前就被拒。
func TestNewAppRejectsUnsupportedDriver(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("log:\n  dir: ./logs\ndb:\n  driver: postgres\n  dsn: x\n  max_idle_conns: 10\n  max_open_conns: 100\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := NewApp(WithConfigPath(path))
	if err == nil || !strings.Contains(err.Error(), "connect to database") {
		t.Fatalf("err = %v", err)
	}
}

/* ---------- Migrate 的模型清单 ---------- */

// Migrate 走的是 database.FrameworkModels（按开关）+ 业务登记的模型，
// 这里只钉"业务模型确实被算进去"这一条（真跑 AutoMigrate 需要库）。
func TestMigrateModelListIncludesRegisteredModels(t *testing.T) {
	type bizModel struct{ ID uint }

	cfg := &config.Config{}
	framework := database.FrameworkModels(cfg)
	if len(framework) != 2 {
		t.Fatalf("开关全关时框架自带两张表，实得 %d", len(framework))
	}

	cfg.Server.OperationLog = true
	cfg.Crawler.Trace.Enabled = true
	framework = database.FrameworkModels(cfg)
	if len(framework) != 4 {
		t.Fatalf("两个开关都开时应是 4 张表，实得 %d", len(framework))
	}

	// App 侧的登记清单（extraModels）由 Migrate 传给 database.Migrate；
	// 这里断言 App 确实记住了它，不需要真库。
	a := &App{Config: cfg}
	a.UseModels(&bizModel{})
	if len(a.extraModels) != 1 {
		t.Fatalf("业务模型没被登记：%v", a.extraModels)
	}
}

/* ---------- 桩 ---------- */

type stubStageFetcher struct{}

func (stubStageFetcher) GetStage() string { return "review" }

func (stubStageFetcher) FetchHandler(_ context.Context, _ *engine.Task, _ *engine.Engine) error {
	return nil
}

func loggersStub() *logrus.Logger {
	l := logrus.New()
	l.SetOutput(io.Discard)
	return l
}
