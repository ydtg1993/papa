package sitedecl

import (
	"context"

	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/engine"
)

// namedFetcher 一个阶段名可配的最小 fetcher（注册那条链要多个不同名的阶段）。
type namedFetcher struct{ name string }

func (f namedFetcher) GetStage() string { return f.name }

func (f namedFetcher) FetchHandler(context.Context, *engine.Task, *engine.Engine) error { return nil }

// stageNamed 一个只有阶段名的假 fetcher（自查输出只用到 GetStage）。
type stageNamed struct{ name string }

func (s stageNamed) GetStage() string { return s.name }

func (s stageNamed) FetchHandler(context.Context, *engine.Task, *engine.Engine) error { return nil }

// stubStageFetcher 一个最小 fetcher：阶段名 "review"，handler 什么都不做。
type stubStageFetcher struct{}

func (stubStageFetcher) GetStage() string { return "review" }

func (stubStageFetcher) FetchHandler(_ context.Context, _ *engine.Task, _ *engine.Engine) error {
	return nil
}

// entryStageFetcher 实现可选接口 EntrySubmitter。
// site 记下框架交过来的那份站点声明快照（BaseURL / Headers 该在这儿，而不是 fetcher 自己存）。
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
