package config

import (
	"strings"
	"testing"
)

func TestQualityConfigOptInAndIndependent(t *testing.T) {
	for _, body := range []string{"", "quality:\n  enabled: false\n"} {
		cfg := loadString(t, baseConfig+body)
		if cfg.Quality.Enabled {
			t.Fatal("quality must default off")
		}
	}
	cfg := loadString(t, baseConfig+"quality:\n  enabled: true\n")
	if !cfg.Quality.Enabled || cfg.Rules.Enabled || cfg.Agent.Enabled {
		t.Fatal("quality requires neither rule nor agent feature")
	}
	message := panicOn(t, baseConfig+"quality:\n  api_key: secret\n")
	if !strings.Contains(message, "quality.api_key") {
		t.Fatalf("unknown key accepted: %s", message)
	}
}
