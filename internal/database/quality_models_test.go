package database

import (
	"github.com/ydtg1993/papa/v3/config"
	"testing"
)

func TestQualityFrameworkModelsIndependent(t *testing.T) {
	cfg := &config.Config{}
	base := FrameworkModels(cfg)
	cfg.Quality.Enabled = true
	all := FrameworkModels(cfg)
	if len(all) != len(base)+3 {
		t.Fatalf("quality must add exactly three tables: %+v", all)
	}
	want := []string{"crawler_rule_observations", "crawler_rule_quality_reports", "crawler_rule_reanalysis_requests"}
	for i, m := range all[len(base):] {
		if m.Name != want[i] || m.Value.(interface{ TableName() string }).TableName() != want[i] {
			t.Fatalf("quality model %d: %+v", i, m)
		}
	}
}
