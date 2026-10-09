package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v2/config"
	"github.com/ydtg1993/papa/v2/core"
	"github.com/ydtg1993/papa/v2/engine"
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

/* ---------- 阶段声明的校验与策略（planStage，纯函数） ---------- */

// 声明有问题**启动即失败**（与配置校验层同取向），报错要点到具体那个阶段。
func TestPlanStageRejects(t *testing.T) {
	base := func() StageSpec {
		return StageSpec{Fetcher: stubStageFetcher{}, WorkerCount: 1, QueueSize: 8}
	}
	cases := []struct {
		name  string
		spec  func() StageSpec
		owner map[string]string
		want  string
	}{
		{"fetcher 为 nil", func() StageSpec { s := base(); s.Fetcher = nil; return s }, nil, "nil"},
		{"worker_count 非正", func() StageSpec { s := base(); s.WorkerCount = 0; return s }, nil, "WorkerCount"},
		{"queue_size 非正", func() StageSpec { s := base(); s.QueueSize = 0; return s }, nil, "QueueSize"},
		{"delay 写错", func() StageSpec { s := base(); s.Delay = "五分钟"; return s }, nil, "Delay"},
		{"backoff 写错", func() StageSpec { s := base(); s.Retry.Backoff = "很快"; return s }, nil, "Backoff"},
		{"阶段名重复", base, map[string]string{"review": "站点 a"}, "重复声明"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			owner := c.owner
			if owner == nil {
				owner = map[string]string{}
			}
			if _, err := planStage(SiteSpec{Key: "a"}.snapshot(), c.spec(), owner); err == nil {
				t.Fatal("应当报错")
			} else if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("报错应含 %q，实得：%v", c.want, err)
			}
		})
	}
}

// 合法声明：算出来的计划带上阶段名、站点归属与解析好的参数。
func TestPlanStageAcceptsAndFillsPlan(t *testing.T) {
	owner := map[string]string{}
	plan, err := planStage(SiteSpec{Key: "huangguo"}.snapshot(), StageSpec{
		Fetcher: stubStageFetcher{}, WorkerCount: 1, QueueSize: 8,
		Delay: "10s-30s", Retry: RetrySpec{MaxAttempts: 2, Backoff: "30s"},
	}, owner)
	if err != nil {
		t.Fatalf("合法声明不该报错：%v", err)
	}
	if plan.stage != "review" || plan.cfg.Site != "huangguo" {
		t.Fatalf("计划不对：%+v", plan)
	}
	if plan.cfg.Delay.Min != 10*time.Second || plan.cfg.Delay.Max != 30*time.Second {
		t.Fatalf("Delay 没解析进计划：%+v", plan.cfg.Delay)
	}
	if plan.cfg.Backoff != 30*time.Second || plan.cfg.MaxAttempts != 2 {
		t.Fatalf("Retry 没解析进计划：%+v", plan.cfg)
	}
	if owner["review"] == "" {
		t.Fatal("阶段名应登记到 owner（重复声明靠它查）")
	}
	// 未给参数时的默认：不延迟 + 退避 1s + 尝试 3 次
	plan2, err := planStage(SiteSpec{}.snapshot(), StageSpec{Fetcher: stubStageFetcher{}, WorkerCount: 1, QueueSize: 1}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if plan2.cfg.Backoff != time.Second || plan2.cfg.MaxAttempts != 3 || plan2.cfg.Delay.Min != 0 {
		t.Fatalf("默认值不对：%+v", plan2.cfg)
	}
}

// AutoStart 决定**要不要把入口回调接上**，并给出该打的那句话：
//   - 开了却没人实现入口 → WARN（配置与实现对不上）
//   - 实现了入口却没开   → Info（"是关的、不是漏的"），且回调不接
//   - 既没开也没实现     → 什么都不打（绝大多数阶段）
func TestPlanStageEntryPolicy(t *testing.T) {
	cases := []struct {
		name      string
		autoStart bool
		withEntry bool
		wantSub   bool
		wantNote  string
		wantWarn  bool
	}{
		{"开了 AutoStart 且有入口", true, true, true, "", false},
		{"开了 AutoStart 但没入口", true, false, false, "AutoStart=true", true},
		{"有入口但没开 AutoStart", false, true, false, "AutoStart=false", false},
		{"既没开也没入口", false, false, false, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var f engine.Fetcher = stubStageFetcher{}
			if c.withEntry {
				f = &entryStageFetcher{}
			}
			plan, err := planStage(SiteSpec{Key: "a"}.snapshot(), StageSpec{
				Fetcher: f, WorkerCount: 1, QueueSize: 8, AutoStart: c.autoStart,
			}, map[string]string{})
			if err != nil {
				t.Fatalf("planStage = %v", err)
			}
			if got := plan.sub != nil; got != c.wantSub {
				t.Fatalf("接不接入口回调 = %v，want %v", got, c.wantSub)
			}
			if c.wantNote != "" && !strings.Contains(plan.note, c.wantNote) {
				t.Fatalf("日志应含 %q，实得 %q", c.wantNote, plan.note)
			}
			if c.wantNote == "" && plan.note != "" {
				t.Fatalf("不该打日志，实得 %q", plan.note)
			}
			if plan.warn != c.wantWarn {
				t.Fatalf("warn = %v，want %v", plan.warn, c.wantWarn)
			}
		})
	}
}

// 阶段间隔与退避的解析：与 YAML 同一套写法。
func TestParseDelayAndBackoff(t *testing.T) {
	if r, err := parseDelay(""); err != nil || r.Min != 0 {
		t.Fatalf("空 Delay 应当是零值区间：%+v %v", r, err)
	}
	if r, err := parseDelay("10s-30s"); err != nil || r.Min != 10*time.Second || r.Max != 30*time.Second {
		t.Fatalf("区间解析: %+v %v", r, err)
	}
	if d, err := parseBackoff(""); err != nil || d != time.Second {
		t.Fatalf("空 Backoff 应当默认 1s：%v %v", d, err)
	}
	if _, err := parseBackoff("0s"); err == nil {
		t.Fatal("0 退避应当报错（退避为 0 等于不退避，写出来多半是笔误）")
	}
}

// stubStageFetcher 一个最小 fetcher：阶段名 "review"，handler 什么都不做。
type stubStageFetcher struct{}

func (stubStageFetcher) GetStage() string { return "review" }

func (stubStageFetcher) FetchHandler(_ context.Context, _ *engine.Task, _ *engine.Engine) error {
	return nil
}

// entryStageFetcher 实现可选接口 EntrySubmitter。
// site 记下框架交过来的那份站点声明快照（BaseURL / Entries 该在这儿，而不是 fetcher 自己存）。
type entryStageFetcher struct {
	called bool
	site   core.Site
}

func (f *entryStageFetcher) GetStage() string { return "review" }

func (f *entryStageFetcher) FetchHandler(_ context.Context, _ *engine.Task, _ *engine.Engine) error {
	return nil
}

func (f *entryStageFetcher) SubmitEntries(_ *engine.Engine, site core.Site) {
	f.called = true
	f.site = site
}

/* ---------- 站点登记表（站点文件自己 init 登记） ---------- */

// 站点文件在 init 里登记，框架收集；`Sites()` 给副本、顺序稳定（= 文件名字典序）。
func TestRegisterSiteCollectsInOrder(t *testing.T) {
	resetRegisteredSites()
	t.Cleanup(resetRegisteredSites)

	RegisterSite(SiteSpec{Key: "b", BaseURL: "https://b.example"})
	RegisterSite(SiteSpec{Key: "a", BaseURL: "https://a.example"})

	got := Sites()
	if len(got) != 2 || got[0].Key != "b" || got[1].Key != "a" {
		t.Fatalf("登记顺序应当保持，实得 %+v", got)
	}
	// 返回副本：改它不该影响登记表
	got[0].Key = "tampered"
	if again := Sites(); again[0].Key != "b" {
		t.Fatalf("Sites() 应返回副本，实得 %+v", again)
	}
}

// 同一个站点键登记两次（多半是两个文件写了同一个 Key）→ 注册时报错并点名。
func TestValidateSitesRejectsDuplicateKey(t *testing.T) {
	sites := []SiteSpec{{Key: "a"}, {Key: "b"}, {Key: "a"}}
	err := validateSites(sites)
	if err == nil {
		t.Fatal("重复的站点键应当报错")
	}
	if !strings.Contains(err.Error(), `"a"`) || !strings.Contains(err.Error(), "第 3 个") {
		t.Fatalf("报错要点名是哪个键、第几个：%v", err)
	}
	// 空键（未归属）可以有多个
	if err := validateSites([]SiteSpec{{}, {Key: "a"}, {}}); err != nil {
		t.Fatalf("空键不该算重复：%v", err)
	}
}
