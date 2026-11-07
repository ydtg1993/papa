package auth

import (
	"database/sql/driver"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/internal/gormfake"
	"gorm.io/gorm"
)

func TestDefaultAuthenticationNeverAuthorizesEmptyStore(t *testing.T) {
	f := gormfake.New()
	f.SetCounts(0, 0)
	c := NewTokenCounter(gormfake.MustOpen(f), time.Hour)
	for _, token := range []string{"", "untrusted"} {
		r := httptest.NewRequest("POST", "/api/settings/shutdown", nil)
		r.Header.Set("Authorization", "Bearer "+token)
		if op, ok := c.Verifier(nil)(r); ok || op != "" {
			t.Fatalf("empty token store authorized management request: operator=%q allowed=%v", op, ok)
		}
		if _, ok := c.Confirmer(nil)(token); ok {
			t.Fatal("empty token store authorized confirmation")
		}
	}
	if !c.Required() {
		t.Fatal("empty token store hid login")
	}
}

func TestAuthenticationIgnoresStaleZeroCounts(t *testing.T) {
	f := gormfake.New()
	f.SetCounts(0, 0)
	c := NewTokenCounter(gormfake.MustOpen(f), time.Hour)
	c.CountAll()
	c.CountEnabled() // Simulate cached zero before a CLI creates the first token.
	f.SetRow(map[string]driver.Value{"operator": "alice"})
	r := httptest.NewRequest("GET", "/api/monitor", nil)
	r.Header.Set("Authorization", "Bearer new-token")
	if op, ok := c.Verifier(nil)(r); !ok || op != "alice" {
		t.Fatalf("fresh token not checked against database: operator=%q allowed=%v", op, ok)
	}
	f.SetRow(nil)
	if _, ok := c.Verifier(nil)(r); ok {
		t.Fatal("revoked token still authorized")
	}
	if _, ok := c.Confirmer(nil)("new-token"); ok {
		t.Fatal("revoked token confirmed")
	}
}

func TestVerifiedTokenRequiresNamedOperator(t *testing.T) {
	f := gormfake.New()
	f.SetRow(map[string]driver.Value{"operator": "  "})
	if _, ok, err := Verify(gormfake.MustOpen(f), "token"); err != nil || ok {
		t.Fatal("nameless token accepted", ok, err)
	}
}

func TestDryRunDatabaseNeverAuthenticates(t *testing.T) {
	db := gormfake.MustOpen(gormfake.New()).Session(&gorm.Session{DryRun: true})
	if _, ok, err := Verify(db, "token"); ok || err != nil {
		t.Fatalf("dry-run database authorized: ok=%v err=%v", ok, err)
	}
}
