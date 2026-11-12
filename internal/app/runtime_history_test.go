package app

import (
	"database/sql/driver"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v4/admin/configstore"
	"github.com/ydtg1993/papa/v4/internal/runtimecontrol"
)

func TestRuntimeHistoryAPIUsesCurrentRevisionAndSelectedChanges(t *testing.T) {
	a, fake := runtimeTestApp(t)
	initial := `{"revision":0,"document":{"schema_version":1,"overrides":{"sites":{"fixture":{"request":{"max_inflight":2}}}}}}`
	latest := `{"revision":1,"document":{"schema_version":1,"overrides":{"sites":{"fixture":{"weight":7,"request":{"max_inflight":5}}}}}}`
	for _, body := range []string{initial, latest} {
		if w := runtimePost(a, body); w.Code != 200 {
			t.Fatal(w.Code, w.Body.String())
		}
	}
	fake.SetRow(map[string]driver.Value{"cfg_key": configstore.KeyRuntimePolicy, "revision": int64(1), "document": []byte(`{"schema_version":1,"overrides":{"sites":{"fixture":{"request":{"max_inflight":2}}}}}`)})
	post := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		a.runtimeHistoryAPI(w, httptest.NewRequest("POST", "/api/runtime-policy/history", strings.NewReader(body)))
		return w
	}
	before := fake.Written()
	w := post(`{"action":"preview","revision":2,"target":1}`)
	var preview struct {
		Changes []runtimecontrol.Change `json:"changes"`
	}
	queries := strings.TrimPrefix(fake.Written(), before)
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &preview) != nil || len(preview.Changes) != 2 || strings.Contains(queries, "INSERT ") || strings.Contains(queries, "UPDATE ") || strings.Contains(queries, "DELETE ") {
		t.Fatal("preview must be read-only", w.Code, w.Body.String())
	}
	w = post(`{"action":"restore","revision":1,"target":1,"paths":[["overrides","sites","fixture","request","max_inflight"]]}`)
	if w.Code != 409 || a.runtimeState.service.Revision() != 2 {
		t.Fatal("stale restore accepted", w.Code)
	}
	w = post(`{"action":"restore","revision":2,"target":1,"paths":[["overrides","sites","fixture","request","max_inflight"]]}`)
	policy := a.Engine.Governance().Effective("fixture")
	if w.Code != 200 || a.runtimeState.service.Revision() != 3 || policy.Request.MaxInflight != 2 || policy.Weight != 7 {
		t.Fatal("restore must create new version and preserve unrelated setting", w.Code, w.Body.String())
	}
}
