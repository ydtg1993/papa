package accounts

import (
	"encoding/json"
	"github.com/ydtg1993/papa/v4/admin/auth"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUnconfiguredAccountsStillListsDeclaredSites(t *testing.T) {
	h := &Handler{Sites: func() []string { return []string{"not-integrated"} }}
	r := httptest.NewRequest("GET", "/api/accounts", nil)
	r = r.WithContext(auth.WithOperator(r.Context(), "operator"))
	w := httptest.NewRecorder()
	h.Accounts(w, r)
	var result struct {
		Configured bool     `json:"configured"`
		Sites      []string `json:"sites"`
		Message    string   `json:"message"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || result.Configured || len(result.Sites) != 1 || result.Sites[0] != "not-integrated" || !strings.Contains(result.Message, "尚未施工") {
		t.Fatalf("missing site-specific unavailable state: %+v", result)
	}
}

func TestUnconfiguredAndOwnershipWithoutApp(t *testing.T) {
	h := &Handler{}
	for _, tc := range []struct {
		method, owner, path string
		code                int
	}{{"GET", "", "/api/accounts", 401}, {"GET", "operator", "/api/accounts", 200}, {"POST", "operator", "/api/accounts", 503}, {"POST", "", "/api/accounts/remote", 401}, {"POST", "operator", "/api/accounts/remote", 503}} {
		r := httptest.NewRequest(tc.method, tc.path, strings.NewReader("{}"))
		r = r.WithContext(auth.WithOperator(r.Context(), tc.owner))
		w := httptest.NewRecorder()
		if strings.HasSuffix(tc.path, "remote") {
			h.Remote(w, r)
		} else {
			h.Accounts(w, r)
		}
		if w.Code != tc.code {
			t.Fatalf("%+v: %d %s", tc, w.Code, w.Body.String())
		}
		if w.Header().Get("Cache-Control") != "no-store" {
			t.Fatal("account response can be cached")
		}
	}
	r := httptest.NewRequest(http.MethodDelete, "/api/accounts", nil)
	r = r.WithContext(auth.WithOperator(r.Context(), "operator"))
	w := httptest.NewRecorder()
	h.Accounts(w, r)
	if w.Code != 405 {
		t.Fatal(w.Code)
	}
}
