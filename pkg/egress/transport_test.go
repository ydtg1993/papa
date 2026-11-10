package egress

import (
	"net/http"
	"testing"
)

func unwrapTestTransport(t *testing.T, tr http.RoundTripper) *transport {
	t.Helper()
	for range 8 {
		if secured, ok := tr.(*transport); ok {
			return secured
		}
		wrapper, ok := tr.(interface{ BaseTransport() http.RoundTripper })
		if !ok {
			t.Fatalf("expected secured egress transport, got %T", tr)
		}
		tr = wrapper.BaseTransport()
	}
	t.Fatal("transport wrapper nesting exceeds test bound")
	return nil
}
