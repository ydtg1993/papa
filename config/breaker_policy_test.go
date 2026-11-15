package config

import (
	"strings"
	"testing"
)

func TestExecutionPolicyYAMLRejected(t *testing.T) {
	for _, body := range []string{"crawler:\n  breaker:\n    enabled: false\n", "browser:\n  pool_size: 3\n", "browser:\n  direct_pool_size: 0\n", "browser:\n  max_idle_time: 5m\n"} {
		msg := panicOn(t, baseConfig+body)
		if !strings.Contains(msg, "execution_policy") {
			t.Fatal(msg)
		}
	}
}
