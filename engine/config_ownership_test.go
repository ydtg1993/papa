package engine

import (
	"sync"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/middleware/filedown"
	"github.com/ydtg1993/papa/v4/pkg/middleware/m3u8"
)

func TestDownloadRegistrationOwnsOverlayValues(t *testing.T) {
	for _, scope := range []string{"site", "stage"} {
		t.Run(scope, func(t *testing.T) {
			e := stageDepEngine(t)
			e.InitDownloaders()
			dir, concurrent := "registered", 7
			m := &m3u8.Overlay{OutputDir: &dir, MaxConcurrent: &concurrent}
			f := &filedown.Overlay{OutputDir: &dir, MaxConcurrent: &concurrent}
			cfg := stageDepCfg
			if scope == "site" {
				e.SetSiteDownloads(cfg.Site, SiteDownloads{M3U8: m, FileDown: f})
			} else {
				cfg.M3U8, cfg.FileDown = m, f
			}
			if err := e.AddStage("detail", cfg, nil, nil); err != nil {
				t.Fatal(err)
			}
			// Callers can reuse both the overlay and its pointer-valued inputs
			// after registration without silently rewriting engine-owned state.
			dir, concurrent = "mutated", 99
			replacement := "replaced"
			m.OutputDir, f.OutputDir = &replacement, &replacement
			e.resolveStageDownloads()
			mc := e.M3U8ForStage(cfg.Site, "detail").Config()
			fc := e.FiledownForStage(cfg.Site, "detail").Config()
			if mc.OutputDir != "registered" || fc.OutputDir != "registered" || mc.MaxConcurrent != 7 || fc.MaxConcurrent != 7 {
				t.Fatalf("registration retained caller-owned overlays: m3u8=(%s,%d) file=(%s,%d)", mc.OutputDir, mc.MaxConcurrent, fc.OutputDir, fc.MaxConcurrent)
			}
		})
	}
}

func TestInstallGovernanceOwnsDelaySnapshot(t *testing.T) {
	e := stageDepEngine(t)
	delays := map[string]time.Duration{"stub:detail": 2 * time.Second}
	e.InstallGovernance(nil, delays)
	delays["stub:detail"] = time.Minute
	if got := e.taskDelay(&Task{Site: "stub", Stage: "detail"}, time.Second); got != 2*time.Second {
		t.Fatalf("installed delay changed through caller map: %v", got)
	}
	// Explicit replacement remains supported, including restoring fallback.
	e.InstallGovernance(nil, delays)
	if got := e.taskDelay(&Task{Site: "stub", Stage: "detail"}, time.Second); got != time.Minute {
		t.Fatalf("explicit reinstall was ignored: %v", got)
	}
	e.InstallGovernance(nil, nil)
	if got := e.taskDelay(&Task{Site: "stub", Stage: "detail"}, time.Second); got != time.Second {
		t.Fatalf("fallback not restored: %v", got)
	}
}

func TestDownloadRegistrationCanReplaceAndClearOverlays(t *testing.T) {
	for _, scope := range []string{"site", "stage"} {
		t.Run(scope, func(t *testing.T) {
			e := stageDepEngine(t)
			e.InitDownloaders()
			register := func(ov SiteDownloads) {
				cfg := stageDepCfg
				if scope == "site" {
					e.SetSiteDownloads(cfg.Site, ov)
				} else {
					cfg.M3U8, cfg.FileDown = ov.M3U8, ov.FileDown
				}
				if err := e.AddStage("detail", cfg, nil, nil); err != nil {
					t.Fatal(err)
				}
			}
			retries, resume := 7, true
			ov := SiteDownloads{
				M3U8:     &m3u8.Overlay{MaxRetries: &retries, EnableResume: &resume},
				FileDown: &filedown.Overlay{MaxRetries: &retries, EnableResume: &resume},
			}
			register(ov)
			// Explicitly replace the declaration before startup; false and zero
			// are overrides rather than the absent/nil fallback.
			retries, resume = 0, false
			register(ov)
			e.resolveStageDownloads()
			m, f := e.M3U8ForStage("stub", "detail").Config(), e.FiledownForStage("stub", "detail").Config()
			if m.MaxRetries != 0 || f.MaxRetries != 0 || m.EnableResume || f.EnableResume {
				t.Fatal("replacement lost explicit zero/false override")
			}
			register(SiteDownloads{})
			e.resolveStageDownloads()
			if e.M3U8ForStage("stub", "detail") != e.GetM3U8() || e.FiledownForStage("stub", "detail") != e.GetFiledown() {
				t.Fatal("clearing declaration did not restore base fallback")
			}
		})
	}
}

func TestInstalledConfigurationDoesNotRaceCallerReuse(t *testing.T) {
	e := stageDepEngine(t)
	e.InitDownloaders()
	dir := "registered"
	m := &m3u8.Overlay{OutputDir: &dir}
	e.SetSiteDownloads("stub", SiteDownloads{M3U8: m})
	delays := map[string]time.Duration{"stub:detail": time.Second}
	e.InstallGovernance(nil, delays)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			dir = "reused"
			delays["stub:detail"] = time.Duration(i)
		}
	}()
	for i := 0; i < 100; i++ {
		e.resolveStageDownloads()
		if got := e.M3U8ForSite("stub").Config().OutputDir; got != "registered" {
			t.Errorf("registered directory changed: %s", got)
		}
		if got := e.taskDelay(&Task{Site: "stub", Stage: "detail"}, 0); got != time.Second {
			t.Errorf("installed delay changed: %v", got)
		}
	}
	wg.Wait()
}
