package executionpolicy

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v4/admin/auth"
	policy "github.com/ydtg1993/papa/v4/pkg/executionpolicy"
)

type stub struct {
	Service
	writes int
	actor  string
	fail   error
}

func (s *stub) Read(context.Context) (policy.View, error) { return policy.View{Revision: 9}, nil }
func (s *stub) Update(_ context.Context, _ policy.Update, actor string) (policy.View, error) {
	s.writes++
	s.actor = actor
	return policy.View{Revision: 10}, s.fail
}
func (s *stub) Validate(_ context.Context, d policy.Document) (policy.Preview, error) {
	return policy.Preview{Document: d}, nil
}
func (s *stub) History(context.Context, int) ([]policy.History, error) {
	return []policy.History{}, nil
}
func call(h *Handler, method, path, body, actor, origin string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if actor != "" {
		r = r.WithContext(auth.WithOperator(r.Context(), actor))
	}
	r.Header.Set("Origin", origin)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}
func TestAuthorizationAndStrictDecode(t *testing.T) {
	s := &stub{}
	h := New(Options{Service: s})
	for _, tc := range []struct {
		body, actor, origin string
		status              int
	}{{`{}`, "", "", 401}, {`{"operator":"forged"}`, "alice", "", 400}, {`{} {}`, "alice", "", 400}, {`null`, "alice", "", 400}, {`[]`, "alice", "", 400}, {`{}`, "alice", "//example.com", 403}, {`{}`, "alice", "https://example.com/path", 403}, {`{"reason":"` + strings.Repeat("x", 128<<10) + `"}`, "alice", "", 400}, {`{}`, "alice", "https://foreign.invalid", 403}, {`{}`, "alice", "", 200}} {
		w := call(h, "PUT", "/api/config/execution-policy", tc.body, tc.actor, tc.origin)
		if w.Code != tc.status {
			t.Fatalf("%+v: %d %s", tc, w.Code, w.Body)
		}
	}
	if s.writes != 1 || s.actor != "alice" {
		t.Fatalf("writes=%d actor=%q", s.writes, s.actor)
	}
	h.Authorize = func(_ *http.Request, action string) error {
		if action != "write" {
			t.Fatal(action)
		}
		return &policy.Error{Code: "denied", Message: "private"}
	}
	if w := call(h, "POST", "/api/config/execution-policy/validate", `{}`, "alice", ""); w.Code != 403 {
		t.Fatal(w.Code)
	}
}
func TestConflictPreservesLatestAndErrors(t *testing.T) {
	s := &stub{}
	h := New(Options{Service: s})
	for code, status := range map[string]int{"conflict": 409, "invalid": 400, "not_found": 404, "stopping": 503, "unavailable": 503} {
		s.fail = &policy.Error{Code: code, Message: "test"}
		w := call(h, "PUT", "/api/config/execution-policy", `{}`, "alice", "")
		if w.Code != status {
			t.Fatalf("%s: %d", code, w.Code)
		}
		if code == "conflict" {
			var result struct {
				Latest policy.View `json:"latest"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Latest.Revision != 9 {
				t.Fatal(w.Body)
			}
		}
	}
}
func TestValidationHasNoWritesAndBoundedHistory(t *testing.T) {
	s := &stub{}
	h := New(Options{Service: s})
	raw, _ := json.Marshal(policy.InitialDocument())
	if w := call(h, "POST", "/api/config/execution-policy/validate", string(raw), "alice", ""); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if s.writes != 0 {
		t.Fatal("validation wrote")
	}
	for _, limit := range []string{"0", "101", "bad"} {
		if w := call(h, "GET", "/api/config/execution-policy/history?limit="+limit, "", "alice", ""); w.Code != 400 {
			t.Fatal(limit, w.Code)
		}
	}
}

func TestReadOnlyAuthorizationAndNoCache(t *testing.T) {
	s := &stub{}
	h := New(Options{Service: s, Authorize: func(_ *http.Request, action string) error {
		if action == "write" {
			return &policy.Error{Code: "denied", Message: "read only"}
		}
		return nil
	}})
	for _, path := range []string{"/api/config/execution-policy", "/api/config/execution-policy/history", "/api/config/capabilities"} {
		if w := call(h, "GET", path, "", "", ""); w.Code != 401 {
			t.Fatalf("unauthenticated %s: %d", path, w.Code)
		}
		w := call(h, "GET", path, "", "reader", "")
		if w.Code != 200 || w.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("read %s: %d %v", path, w.Code, w.Header())
		}
	}
	for _, req := range []struct{ method, path string }{{"PUT", "/api/config/execution-policy"}, {"POST", "/api/config/execution-policy/rollback"}, {"POST", "/api/config/execution-policy/validate"}} {
		if w := call(h, req.method, req.path, `{}`, "reader", ""); w.Code != 403 {
			t.Fatalf("write %s: %d", req.path, w.Code)
		}
	}
	if s.writes != 0 {
		t.Fatal("read-only caller wrote policy")
	}
}
