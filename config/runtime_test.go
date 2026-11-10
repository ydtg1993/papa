package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRuntimeYAMLExplicitFalseZeroAndDuration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	raw := `log:
  dir: ./logs
db:
  max_idle_conns: 2
  max_open_conns: 10
runtime:
  defaults:
    request:
      enabled: true
      manual_delay: 2s
      autothrottle:
        enabled: true
  sites:
    fixture:
      request:
        enabled: false
        requests_per_second: 0
        manual_delay: 0s
        autothrottle:
          enabled: false
      stages:
        detail:
          max_concurrent: 2
          delay: 500ms
  resources:
    enabled: false
    task_max: 6
  proxy_health:
    enabled: false
    initial_cooldown: 15s
`
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	p := c.Runtime.Sites["fixture"]
	if p.Request.Enabled == nil || *p.Request.Enabled || p.Request.RequestsPerSecond == nil || *p.Request.RequestsPerSecond != 0 || p.Request.ManualDelay == nil || *p.Request.ManualDelay != 0 {
		t.Fatal("explicit false/zero lost")
	}
	if p.Request.MaxInflight != nil {
		t.Fatal("unspecified field became explicit")
	}
	if *p.Stages["detail"].Delay != 500*time.Millisecond || *c.Runtime.ProxyHealth.InitialCooldown != 15*time.Second {
		t.Fatal("duration pointer decode failed")
	}
}
