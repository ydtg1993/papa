package app

import (
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/pkg/loggers"
)

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
