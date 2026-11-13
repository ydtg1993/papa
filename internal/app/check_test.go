package app

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/pkg/loggers"
)

func TestRegisterSitesRejectsInheritedBreakerBeforePublishingBatch(t *testing.T) {
	for _, tc := range []struct {
		name     string
		defaults config.BreakerConfig
		want     string
	}{
		{"threshold", config.BreakerConfig{Enabled: true}, "threshold 必须 > 0"},
		{"policy", config.BreakerConfig{Policy: config.BreakerPolicy{Capacity: -1}}, "capacity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{}
			cfg.Crawler.DrainInterval = time.Hour
			cfg.Crawler.Breaker = tc.defaults
			e := noDBEngine(t, cfg)
			t.Cleanup(func() { e.Stop(time.Millisecond) })
			a := &App{Config: cfg, Engine: e}
			// A valid first site must not be published before the later inherited
			// breaker fails, including when both sites have stages.
			good := SiteSpec{Key: "good", Breaker: &BreakerSpec{Enabled: true, Threshold: 9, Policy: &config.BreakerPolicy{}},
				Stages: []StageSpec{{Fetcher: namedFetcher{name: "catalog"}, WorkerCount: 1, QueueSize: 8}}}
			bad := SiteSpec{Key: "bad", Stages: []StageSpec{{Fetcher: namedFetcher{name: "catalog"}, WorkerCount: 1, QueueSize: 8}}}
			var panicked any
			func() {
				defer func() { panicked = recover() }()
				a.RegisterSites(good, bad)
			}()
			if panicked == nil || !strings.Contains(fmt.Sprint(panicked), tc.want) {
				t.Fatalf("expected inherited breaker validation panic, got %v", panicked)
			}
			for _, key := range []string{"good", "bad"} {
				if _, ok := a.Site(key); ok {
					t.Errorf("App published %q from an invalid batch", key)
				}
				if _, ok := e.Site(key); ok {
					t.Errorf("Engine published %q from an invalid batch", key)
				}
			}
			if len(e.GetStageStats()) != 0 || len(e.BreakerStatuses()) != 0 {
				t.Fatalf("invalid batch published stages or breakers: stages=%v breakers=%v", e.GetStageStats(), e.BreakerStatuses())
			}
		})
	}
}

// 声明有问题时**一个字节都不落地**：原来是一边注册一边炸，前几个站已经进引擎了。
func TestRegisterSitesAppliesNothingWhenDeclarationIsBad(t *testing.T) {
	cfg := &config.Config{}
	cfg.Crawler.DrainInterval = time.Hour
	e := noDBEngine(t, cfg)
	t.Cleanup(func() { e.Stop(time.Millisecond) })
	a := &App{Config: cfg, Engine: e, Logger: &loggers.LoggerSet{Engine: quietLogger()}}

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
