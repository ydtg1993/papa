package sitedecl

import (
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/engine"
	"github.com/ydtg1993/papa/v4/pkg/middleware/filedown"
	"github.com/ydtg1993/papa/v4/pkg/middleware/m3u8"
)

func TestPlanSitesOwnsDeclarationAndDownloadSnapshots(t *testing.T) {
	dir, workers := "original", 2
	sharedM3U8 := &m3u8.Overlay{OutputDir: &dir}
	sharedFile := &filedown.Overlay{MaxConcurrent: &workers}
	fetcher := &entryStageFetcher{}
	spec := SiteSpec{
		Key: "example", Headers: map[string]string{"X-Test": "original"},
		RestrictedKeywords: []string{"original"}, M3U8: sharedM3U8, FileDown: sharedFile,
		Stages: []StageSpec{{Fetcher: fetcher, WorkerCount: 1, QueueSize: 8,
			M3U8: sharedM3U8, FileDown: sharedFile}},
	}
	plans, err := PlanSites(nil, []SiteSpec{spec})
	if err != nil {
		t.Fatal(err)
	}
	p := plans[0]
	spec.Headers["X-Test"], spec.RestrictedKeywords[0] = "input", "input"
	dir, workers = "input", 99
	if p.Site.Headers["X-Test"] != "original" || p.Site.RestrictedKeywords[0] != "original" {
		t.Errorf("plan site aliases the declaration: %+v", p.Site)
	}
	if *p.Downloads.M3U8.OutputDir != "original" || *p.Downloads.FileDown.MaxConcurrent != 2 ||
		*p.Stages[0].Config.M3U8.OutputDir != "original" || *p.Stages[0].Config.FileDown.MaxConcurrent != 2 {
		t.Fatal("site/stage download plans alias caller-owned scalar pointers")
	}
	*p.Downloads.M3U8.OutputDir, *p.Downloads.FileDown.MaxConcurrent = "plan-site", 3
	if *p.Stages[0].Config.M3U8.OutputDir != "original" || *p.Stages[0].Config.FileDown.MaxConcurrent != 2 {
		t.Fatal("site and stage plans share download snapshots")
	}
	if p.Stages[0].Fetcher != fetcher {
		t.Fatal("planning must retain the original Fetcher object")
	}
}

type snapshotEntryFetcher struct {
	namedFetcher
	seen []core.Site
}

func (f *snapshotEntryFetcher) SubmitEntries(_ *engine.Engine, site core.Site) {
	f.seen = append(f.seen, site)
}

func TestEntryCallbacksReceiveFreshIndependentSnapshots(t *testing.T) {
	first := &snapshotEntryFetcher{namedFetcher: namedFetcher{name: "first"}}
	second := &snapshotEntryFetcher{namedFetcher: namedFetcher{name: "second"}}
	spec := SiteSpec{Key: "example", Headers: map[string]string{"X-Test": "original"},
		RestrictedKeywords: []string{"original"}, Stages: []StageSpec{
			{Fetcher: first, WorkerCount: 1, QueueSize: 8, Entry: true},
			{Fetcher: second, WorkerCount: 1, QueueSize: 8, Entry: true},
		}}
	plans, err := PlanSites(nil, []SiteSpec{spec})
	if err != nil {
		t.Fatal(err)
	}
	p := plans[0]
	spec.Headers["X-Test"], spec.RestrictedKeywords[0] = "input", "input"
	p.Site.Headers["X-Test"], p.Site.RestrictedKeywords[0] = "plan", "plan"
	p.Stages[0].Submit(nil)
	if got := first.seen[0]; got.Headers["X-Test"] != "original" || got.RestrictedKeywords[0] != "original" {
		t.Fatalf("callback aliases its declaration or published plan: %+v", got)
	}
	first.seen[0].Headers["X-Test"], first.seen[0].RestrictedKeywords[0] = "callback", "callback"
	p.Stages[0].Submit(nil)
	p.Stages[1].Submit(nil)
	for _, got := range []core.Site{first.seen[1], second.seen[0]} {
		if got.Headers["X-Test"] != "original" || got.RestrictedKeywords[0] != "original" {
			t.Fatalf("callback mutation escaped into another invocation: %+v", got)
		}
	}
	if p.Site.Headers["X-Test"] != "plan" || p.Site.RestrictedKeywords[0] != "plan" ||
		spec.Headers["X-Test"] != "input" || spec.RestrictedKeywords[0] != "input" {
		t.Fatal("callback mutation escaped into plan or input")
	}
}

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
		{"阶段名重复", base, map[string]string{core.StageKey("a", "review"): "站点 a"}, "重复声明"},
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
	plan, err := planStage(SiteSpec{Key: "example"}.snapshot(), StageSpec{
		Fetcher: stubStageFetcher{}, WorkerCount: 1, QueueSize: 8,
		Delay: "10s-30s", Retry: RetrySpec{MaxAttempts: 2, Backoff: "30s"},
	}, owner)
	if err != nil {
		t.Fatalf("合法声明不该报错：%v", err)
	}
	if plan.Stage != "review" || plan.Config.Site != "example" {
		t.Fatalf("计划不对：%+v", plan)
	}
	if plan.Config.Delay.Min != 10*time.Second || plan.Config.Delay.Max != 30*time.Second {
		t.Fatalf("Delay 没解析进计划：%+v", plan.Config.Delay)
	}
	if plan.Config.Backoff != 30*time.Second || plan.Config.MaxAttempts != 2 {
		t.Fatalf("Retry 没解析进计划：%+v", plan.Config)
	}
	if owner[core.StageKey("example", "review")] == "" {
		t.Fatal("阶段名应登记到 owner（同站重名靠它查）")
	}
	// 未给参数时的默认：不延迟 + 退避 1s + 尝试 3 次
	plan2, err := planStage(SiteSpec{}.snapshot(), StageSpec{Fetcher: stubStageFetcher{}, WorkerCount: 1, QueueSize: 1}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if plan2.Config.Backoff != time.Second || plan2.Config.MaxAttempts != 3 || plan2.Config.Delay.Min != 0 {
		t.Fatalf("默认值不对：%+v", plan2.Config)
	}
}

// 阶段名按站点作用域：两个站可以各有自己的 catalog，各自一份计划（各自一个池子）。
// 同站内重名仍然报错 —— 别把"跨站允许"读成"随便重名"。
func TestPlanSitesScopesStageNameBySite(t *testing.T) {
	same := func() StageSpec {
		return StageSpec{Fetcher: stageNamed{"catalog"}, WorkerCount: 1, QueueSize: 8}
	}
	plans, err := planSites(nil, []SiteSpec{
		{Key: "example", Stages: []StageSpec{same()}},
		{Key: "example2", Stages: []StageSpec{same()}},
	})
	if err != nil {
		t.Fatalf("两个站同名阶段应当合法：%v", err)
	}
	if len(plans) != 2 || len(plans[0].Stages) != 1 || len(plans[1].Stages) != 1 {
		t.Fatalf("两份声明各应落地一个阶段：%+v", plans)
	}
	if plans[0].Stages[0].Stage != "catalog" || plans[0].Stages[0].Config.Site != "example" {
		t.Fatalf("第一个站落地的阶段不对：%+v", plans[0].Stages[0])
	}
	if plans[1].Stages[0].Stage != "catalog" || plans[1].Stages[0].Config.Site != "example2" {
		t.Fatalf("第二个站落地的阶段不对：%+v", plans[1].Stages[0])
	}

	// 同一个站里声明两次同名 → 还是要报
	if _, err := planSites(nil, []SiteSpec{
		{Key: "example", Stages: []StageSpec{same(), same()}},
	}); err == nil || !strings.Contains(err.Error(), "重复声明") {
		t.Fatalf("同站内重名应当报错，实得：%v", err)
	}

	// 未命名的声明直接拒掉（不再有"未归属"那一档）：它会落进默认 scope，而新任务一律要带站点
	if _, err := planSites(nil, []SiteSpec{{Stages: []StageSpec{same()}}}); err == nil ||
		!strings.Contains(err.Error(), "缺 Key") {
		t.Fatalf("未命名的站点声明应当报错，实得：%v", err)
	}
}

// Entry 是 bool（有没有入口任务）× 有没有实现入口，四个格子各有各的说法：
//   - 写了 Entry 却没人实现入口 → WARN（配置与实现对不上）
//   - 没写 Entry 却实现了入口 → WARN 且**回调不接** —— false 是默认值，这是最常见的那种手误，
//     所以在日志里直接把话写全（写 Entry: true 就投）
//   - 写了 Entry 且有入口 → Info，回调留着，启动那一趟投
//   - 没写且没入口 → 什么都不打（各走各的正常路）
//
// "启动先别跑"不在这张表里：那是站点级的 StartPaused（见 TestSiteSpecSnapshotCarriesStartPaused）。
func TestPlanStageEntryPolicy(t *testing.T) {
	cases := []struct {
		name      string
		entry     bool
		withEntry bool
		wantSub   bool
		wantNote  string
		wantWarn  bool
	}{
		{"有 Entry + 有入口", true, true, true, "有入口任务", false},
		{"有 Entry + 没入口", true, false, false, "没有实现 SubmitEntries", true},
		{"没写 Entry + 有入口", false, true, false, "没写 Entry", true},
		{"没写 Entry + 没入口", false, false, false, "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var f engine.Fetcher = stubStageFetcher{}
			if c.withEntry {
				f = &entryStageFetcher{}
			}
			plan, err := planStage(SiteSpec{Key: "a"}.snapshot(), StageSpec{
				Fetcher: f, WorkerCount: 1, QueueSize: 8, Entry: c.entry,
			}, map[string]string{})
			if err != nil {
				t.Fatalf("planStage = %v", err)
			}
			if got := plan.Submit != nil; got != c.wantSub {
				t.Fatalf("接不接入口回调 = %v，want %v", got, c.wantSub)
			}
			// 声明要原样落进引擎侧的 StageConfig（漏抄一处，引擎就会按零值 = 没有入口处理）
			if plan.Config.Entry != c.entry {
				t.Fatalf("Config.Entry = %v，want %v", plan.Config.Entry, c.entry)
			}
			if c.wantNote != "" && !strings.Contains(plan.Note, c.wantNote) {
				t.Fatalf("日志应含 %q，实得 %q", c.wantNote, plan.Note)
			}
			// 阶段名只在本站内唯一：日志必须带站点，否则两个站的 catalog 会打成两行一样的话。
			if c.wantNote != "" && !strings.Contains(plan.Note, "站点 a /") {
				t.Fatalf("日志应带站点前缀，实得 %q", plan.Note)
			}
			if c.wantNote == "" && plan.Note != "" {
				t.Fatalf("不该打日志，实得 %q", plan.Note)
			}
			if plan.Warn != c.wantWarn {
				t.Fatalf("warn = %v，want %v", plan.Warn, c.wantWarn)
			}
		})
	}
}

// StartPaused 是**站点级**的：它得原样过 snapshot() 到引擎，引擎在 ApplyRegisterStage 里读它下闸。
// 漏抄一处的表现最难查 —— 声明写了、启动日志里一个字都没有、任务照跑。
func TestSiteSpecSnapshotCarriesStartPaused(t *testing.T) {
	snap := SiteSpec{
		Key: "a", BaseURL: "https://a.example/",
		Headers:            map[string]string{"User-Agent": "ua-a"},
		RestrictedKeywords: []string{"安全验证"},
		StartPaused:        true,
	}.snapshot()
	if !snap.StartPaused {
		t.Fatal("StartPaused 没进快照 —— 引擎就永远不知道该闸住这一站")
	}
	// 其余字段也逐个对一遍：这条用例是"快照照声明走"的守卫，漏哪一个都是同一个 bug
	if snap.Key != "a" || snap.BaseURL != "https://a.example/" ||
		snap.Headers["User-Agent"] != "ua-a" || len(snap.RestrictedKeywords) != 1 {
		t.Fatalf("快照漏了字段：%+v", snap)
	}
	if off := (SiteSpec{Key: "a"}).snapshot(); off.StartPaused {
		t.Fatal("没写 StartPaused 的站不该被闸住（零值必须是「照跑」）")
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
