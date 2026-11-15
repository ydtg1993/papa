package sitedecl

import (
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v4/core"
)

func TestStageBreakerReadOnlyCoverageDeclaration(t *testing.T) {
	for _, coverage := range []string{"", "完整读取与解析；无副作用", "  ", strings.Repeat("x", 1025)} {
		plan, err := planStage(core.Site{Key: "s"}, StageSpec{Fetcher: namedFetcher{"detail"}, WorkerCount: 1, QueueSize: 8, BreakerReadOnlyCoverage: coverage}, map[string]string{})
		invalid := coverage == "  " || len(coverage) > 1024
		if (err != nil) != invalid {
			t.Fatalf("validation mismatch length=%d err=%v", len(coverage), err)
		}
		if !invalid && plan.Config.BreakerReadOnlyCoverage != coverage {
			t.Fatal("coverage not forwarded")
		}
	}
}

func TestStageRepeatLimitDeclaration(t *testing.T) {
	for _, limit := range []int{0, 30, -1} {
		plan, err := planStage(core.Site{Key: "s"}, StageSpec{Fetcher: namedFetcher{"detail"}, WorkerCount: 1, QueueSize: 8, RepeatLimit: limit}, map[string]string{})
		if limit < 0 {
			if err == nil {
				t.Fatal("negative RepeatLimit accepted")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if plan.Config.RepeatLimit != limit {
			t.Fatalf("limit not forwarded: %+v", plan.Config)
		}
	}
}
