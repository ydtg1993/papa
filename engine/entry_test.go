package engine

import (
	"context"
	"sync"
	"testing"

	"github.com/ydtg1993/papa/v3/core"
)

// entryFetcher 一个实现了 SubmitEntries 的假 fetcher：记下每次被投，以及拿到的是哪个站的快照。
//
// 不复用 recordingFetcher：给那个加上 SubmitEntries 会让**所有**用它的阶段都变成"有入口"，
// 把别的用例的语义一起改了。
type entryFetcher struct {
	stage string

	mu    sync.Mutex
	calls int
	sites []string
}

func (f *entryFetcher) GetStage() string { return f.stage }

func (f *entryFetcher) FetchHandler(context.Context, *Task, *Engine) error { return nil }

func (f *entryFetcher) SubmitEntries(_ *Engine, site core.Site) {
	f.mu.Lock()
	f.calls++
	f.sites = append(f.sites, site.Key)
	f.mu.Unlock()
}

func (f *entryFetcher) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// entryScope 入口回调拿到的站点快照的站点键（按调用先后）。
// 顺手把"回调确实带着本站声明"这条钉住：快照是照声明压出来的唯一通道（SiteSpec.snapshot）。
func (f *entryFetcher) entryScope() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sites...)
}

// 声明了 Entry 的阶段：启动那一趟投一轮；没写 Entry 的（声明层会把回调整块丢掉 → 这里传 nil）一次都不投。
//
// 这条替代了原来的三态（auto/manual/off）用例：manual 没了 —— "启动先别跑"不是入口的事，
// 是站点级的 SiteSpec.StartPaused（见 TestStartPausedBlocksStartup…）。
func TestStageEntryAtStartup(t *testing.T) {
	e := newTestEngine(t, newFakeTaskDB())
	with, without := &entryFetcher{stage: "with"}, &entryFetcher{stage: "without"}
	cfg := StageConfig{Site: "a", MaxAttempts: 1, WorkerCount: 1, QueueSize: 8}
	cfgEntry := cfg
	cfgEntry.Entry = true
	e.AddStage("with", cfgEntry, with, func(eng *Engine) { with.SubmitEntries(eng, core.Site{Key: "a"}) })
	e.AddStage("without", cfg, without, nil)

	e.ApplyRegisterStage()

	if with.callCount() != 1 {
		t.Fatalf("有入口的阶段应当在启动那一趟投一次，实得 %d", with.callCount())
	}
	if without.callCount() != 0 {
		t.Fatalf("没写 Entry 的阶段一次都不该投，实得 %d", without.callCount())
	}
	if got := with.entryScope(); len(got) != 1 || got[0] != "a" {
		t.Fatalf("入口回调该拿到本站的声明快照，实得 %v", got)
	}
}

// 声明写了 Entry、fetcher 却没实现 SubmitEntries（回调是 nil）：启动照常，不 panic。
// 这种情况由声明层打 WARN 说清（见 internal/app 的 planStage），引擎侧只是"没有入口可投"。
func TestEntryWithoutSubmitterIsHarmless(t *testing.T) {
	e := newTestEngine(t, newFakeTaskDB())
	cfg := StageConfig{Site: "a", Entry: true, MaxAttempts: 1, WorkerCount: 1, QueueSize: 8}
	e.AddStage("catalog", cfg, &recordingFetcher{stage: "catalog"}, nil)

	e.ApplyRegisterStage() // 不 panic 就算过

	if pool := e.pools.get(core.StageKey("a", "catalog")); pool != nil {
		t.Fatal("没有入口回调的阶段不该被建起来（池子是懒建的）")
	}
}
