package config

import (
	"strings"
	"testing"
)

func TestAgentConfigOptInAndRulesDependency(t *testing.T) {
	for _, body := range []string{"", "agent:\n  enabled: false\n"} {
		cfg := loadString(t, baseConfig+body)
		if cfg.Agent.Enabled {
			t.Fatal("agent must default off")
		}
	}
	message := panicOn(t, baseConfig+"agent:\n  enabled: true\n")
	if !strings.Contains(message, "agent.enabled") || !strings.Contains(message, "rules.enabled") {
		t.Fatalf("dependency diagnostic: %s", message)
	}
	cfg := loadString(t, baseConfig+"rules:\n  enabled: true\n  sample_dir: './samples'\nagent:\n  enabled: true\n")
	if !cfg.Agent.Enabled || !cfg.Rules.Enabled {
		t.Fatal("enabled configuration not retained")
	}
	message = panicOn(t, baseConfig+"agent:\n  api_key: secret\n")
	if !strings.Contains(message, "agent.api_key") {
		t.Fatalf("unknown key accepted: %s", message)
	}
}
