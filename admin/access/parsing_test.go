package access

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestParseWhitelist(t *testing.T) {
	got := ParseWhitelist([]string{"10.0.0.1", "192.168.0.0/16", "::1", "bad", ""}, silentLogger{})
	if len(got) != 3 {
		t.Fatalf("parsed count = %d, want 3", len(got))
	}
	if got[0].String() != "10.0.0.1/32" {
		t.Errorf("single ip = %s, want 10.0.0.1/32", got[0])
	}
	if got[1].String() != "192.168.0.0/16" {
		t.Errorf("cidr = %s, want 192.168.0.0/16", got[1])
	}
	if got[2].String() != "::1/128" {
		t.Errorf("ipv6 = %s, want ::1/128", got[2])
	}
}

func TestClientIP(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "1.2.3.4:5678"
	if ip := ClientIP(req); !ip.Equal(net.ParseIP("1.2.3.4")) {
		t.Errorf("clientIP = %v, want 1.2.3.4", ip)
	}
	req.RemoteAddr = "1.2.3.4"
	if ip := ClientIP(req); !ip.Equal(net.ParseIP("1.2.3.4")) {
		t.Errorf("ClientIP(no port) = %v, want 1.2.3.4", ip)
	}
}
