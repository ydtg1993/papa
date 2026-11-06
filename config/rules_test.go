package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRulesConfigIsOptIn(t *testing.T) {
	for _, body := range []string{"", "rules:\n  enabled: false\n", "rules:\n  enabled: false\n  sample_dir: ''\n"} {
		cfg := loadString(t, baseConfig+body)
		if cfg.Rules.Enabled || cfg.Rules.SampleDir != "" {
			t.Fatalf("disabled rules must stay optional: %+v", cfg.Rules)
		}
	}
}

func TestRulesConfigRequiresSampleDirWhenEnabled(t *testing.T) {
	for _, suffix := range []string{"", "  sample_dir: ''\n", "  sample_dir: '   '\n"} {
		msg := panicOn(t, baseConfig+"rules:\n  enabled: true\n"+suffix)
		if !strings.Contains(msg, "rules.sample_dir") {
			t.Fatalf("missing sample directory must name its key: %s", msg)
		}
	}
}

func TestRulesConfigLoadDoesNotCreateSampleDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "long-term-samples")
	cfg := loadString(t, baseConfig+"rules:\n  enabled: true\n  sample_dir: '"+dir+"'\n")
	if !cfg.Rules.Enabled || cfg.Rules.SampleDir != dir {
		t.Fatalf("rules config = %+v", cfg.Rules)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("config validation must not create sample storage: %v", err)
	}
}

func TestRulesConfigRejectsUnknownKey(t *testing.T) {
	msg := panicOn(t, baseConfig+"rules:\n  sample_path: ./rule-samples\n")
	if !strings.Contains(msg, "rules.sample_path") {
		t.Fatalf("unknown rule config key must be reported: %s", msg)
	}
}
