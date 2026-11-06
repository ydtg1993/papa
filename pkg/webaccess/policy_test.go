package webaccess

import (
	"errors"
	"net/netip"
	"testing"
	"time"
)

func TestPolicyCanonicalOriginAndPaths(t *testing.T) {
	s, err := New(Config{Policy: Policy{Origins: []Origin{{URL: "https://EXAMPLE.com:443", PathPrefixes: []string{"/catalog"}}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, raw := range []string{"https://example.com/catalog", "https://example.com/catalog/item?q=1"} {
		if _, _, err := s.target(raw, false); err != nil {
			t.Fatalf("allowed %q: %v", raw, err)
		}
	}
	for _, raw := range []string{"https://example.com.evil/catalog", "http://example.com/catalog", "https://example.com/catalogue", "https://example.com/catalog/../secret", "https://example.com/catalog/%2e%2e/secret", "https://example.com/catalog/%252e%252e/secret", "https://user:secret@example.com/catalog", "https://example.com:444/catalog", "https://example.com/catalog%2fsecret", "https://example.com/catalog\\secret"} {
		if _, _, err := s.target(raw, false); !errors.Is(err, ErrPolicy) {
			t.Errorf("accepted %q: %v", raw, err)
		}
	}
}

func TestSpecialAndMappedAddressesDenied(t *testing.T) {
	for _, raw := range []string{"127.0.0.1", "10.0.0.1", "169.254.169.254", "100.64.0.1", "192.0.2.1", "198.18.0.1", "224.0.0.1", "0.0.0.0", "240.0.0.1", "::", "::1", "::ffff:127.0.0.1", "fc00::1", "fe80::1", "2001:db8::1", "64:ff9b::a00:1", "2002:a00:1::"} {
		if publicIP(netip.MustParseAddr(raw)) {
			t.Errorf("accepted %s", raw)
		}
	}
	for _, raw := range []string{"8.8.8.8", "2606:4700:4700::1111"} {
		if !publicIP(netip.MustParseAddr(raw)) {
			t.Errorf("rejected %s", raw)
		}
	}
}

func TestCanonicalURLMatchesTransportAndRejectsAmbiguity(t *testing.T) {
	for raw, want := range map[string]string{
		"https://EXAMPLE.com:443/private?q=1#fragment": "https://example.com/private?q=1",
		"http://EXAMPLE.com:80":                        "http://example.com/",
		"https://example.com/%70rivate":                "https://example.com/private",
		"http://[2001:4860:4860::8888]:8080/a":         "http://[2001:4860:4860::8888]:8080/a",
	} {
		got, err := CanonicalURL(raw)
		if err != nil || got != want {
			t.Fatalf("%q: got %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"https://user:secret@example.com/private", "https://example.com/a/../private", "https://example.com/a%2fprivate", "file:///secret"} {
		if got, err := CanonicalURL(raw); !errors.Is(err, ErrPolicy) || got != "" {
			t.Fatalf("accepted ambiguous URL: %q %v", got, err)
		}
	}
}

func TestBudgetsReturnsEffectiveDetachedDefaults(t *testing.T) {
	s, err := New(Config{Policy: Policy{Origins: []Origin{{URL: "https://example.com"}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	b := s.Budgets()
	if b.MaxPages != 8 || b.MaxRequests != 128 || b.MaxResponseBytes != 2<<20 || b.MaxTotalBytes != 16<<20 || b.MaxDuration != 180*time.Second || b.MaxRedirects != 8 {
		t.Fatalf("unexpected effective limits: %+v", b)
	}
	b.MaxPages = 32
	if s.Budgets().MaxPages != 8 {
		t.Fatal("returned budget mutates session")
	}
}
