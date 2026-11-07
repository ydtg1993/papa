package config

import (
	"testing"

	"github.com/ydtg1993/papa/v3/pkg/egress"
)

func TestNetworkPrivateOriginsAreExplicitAndValidated(t *testing.T) {
	cfg := loadString(t, baseConfig+"network:\n  private_origins:\n    - http://127.0.0.1:8123\n    - https://intranet.example:8443\n")
	if len(cfg.Network.PrivateOrigins) != 2 {
		t.Fatal("network grants were not decoded")
	}
	for _, bad := range []string{"http://host/path", "file:///secret", "http://user:secret@host", "*", "http://host:0"} {
		t.Run(bad, func(t *testing.T) {
			assertPanicContains(t, func() {
				Validate(&Config{Network: egress.Config{PrivateOrigins: []string{bad}}}, nil)
			}, "network.private_origins")
		})
	}
}
