package egress

import (
	"context"
	"errors"
	"net/netip"
	"testing"
)

func TestAllowedOriginsRestrictsPublicAndPrivateDestinations(t *testing.T) {
	cfg := Config{AllowedOrigins: []string{"http://127.0.0.1:12345"}, PrivateOrigins: []string{"http://127.0.0.1:12345"}}
	p, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	p.lookup = func(context.Context, string, string) ([]netip.Addr, error) {
		t.Fatal("denied origin reached DNS")
		return nil, nil
	}
	if _, _, err = p.Resolve(context.Background(), "http://127.0.0.1:12345/probe"); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{"https://example.test/", "http://127.0.0.1:12346/probe", "https://127.0.0.1:12345/probe"} {
		if _, _, err = p.Resolve(context.Background(), raw); !errors.Is(err, ErrDenied) {
			t.Fatalf("allowed %s: %v", raw, err)
		}
	}
	cfg.PrivateOrigins = nil
	p, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = p.Resolve(context.Background(), "http://127.0.0.1:12345/"); !errors.Is(err, ErrDenied) {
		t.Fatal("allowlist bypassed private policy", err)
	}
	if _, err = New(Config{AllowedOrigins: []string{"http://127.0.0.1:12345/path"}}); !errors.Is(err, ErrDenied) {
		t.Fatal("accepted non-origin", err)
	}
}
