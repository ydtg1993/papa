package app

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"github.com/ydtg1993/papa/v4/admin/auth"
	"github.com/ydtg1993/papa/v4/admin/configstore"
	adminruntime "github.com/ydtg1993/papa/v4/admin/runtimecontrol"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/engine"
	"github.com/ydtg1993/papa/v4/internal/gormfake"
	"github.com/ydtg1993/papa/v4/internal/runtimecontrol"
	"github.com/ydtg1993/papa/v4/pkg/governance"
	"github.com/ydtg1993/papa/v4/pkg/loggers"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func runtimeTestApp(t *testing.T) (*App, *gormfake.DB) {
	t.Helper()
	fake := gormfake.New()
	fake.EnableTransactionStatements()
	db := gormfake.MustOpen(fake)
	cfg := &config.Config{}
	l := &loggers.LoggerSet{Engine: quietLogger(), DB: quietLogger(), Monitor: quietLogger(), Sys: quietLogger()}
	e := engine.NewEngine(db, cfg, l)
	t.Cleanup(func() { e.Stop(time.Second) })
	e.SetSite(core.Site{Key: "fixture"})
	if err := e.AddStage("detail", engine.StageConfig{Site: "fixture", WorkerCount: 4, QueueSize: 8, Delay: config.DurationRange{Min: time.Second, Max: 2 * time.Second}}, namedFetcher{"detail"}, nil); err != nil {
		t.Fatal(err)
	}
	a := &App{Config: cfg, Engine: e, DB: db, Logger: l, configStore: configstore.New(db)}
	policy, delays, err := e.ResolveRuntime(config.RuntimeConfig{})
	if err != nil {
		t.Fatal(err)
	}
	c, err := governance.New(policy)
	if err != nil {
		t.Fatal(err)
	}
	e.InstallGovernance(c, delays)
	a.runtimeState.service = runtimecontrol.New(e, adminruntime.StoreAdapter{Store: a.configStore}, cfg.Runtime)
	return a, fake
}
func runtimePost(a *App, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/runtime-policy", strings.NewReader(body))
	r = r.WithContext(auth.WithOperator(r.Context(), "fixture-operator"))
	a.runtimeHandler().Policy(w, r)
	return w
}
func TestRuntimePolicyPublishAfterPersistenceAndReset(t *testing.T) {
	a, f := runtimeTestApp(t)
	body := `{"revision":0,"document":{"schema_version":1,"overrides":{"sites":{"fixture":{"request":{"enabled":true,"max_inflight":2,"requests_per_second":3}}}}}}`
	w := runtimePost(a, body)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if a.Engine.Governance().Effective("fixture").Request.MaxInflight != 2 || a.runtimeState.service.Revision() != 1 {
		t.Fatal("policy not published")
	}
	if !strings.Contains(f.Written(), "fixture-operator") {
		t.Fatal("operator not persisted")
	}
	if w = runtimePost(a, body); w.Code != 409 {
		t.Fatal("stale edit accepted", w.Code)
	}
	f.SetExecError("offline")
	w = runtimePost(a, `{"revision":1,"document":{"schema_version":1,"overrides":{}}}`)
	if w.Code != 503 || a.runtimeState.service.Revision() != 1 || a.Engine.Governance().Effective("fixture").Request.MaxInflight != 2 {
		t.Fatal("failed write published", w.Code)
	}
	f.SetExecError("")
	f.SetRow(map[string]driver.Value{"revision": int64(1), "cfg_val": []byte(`{"schema_version":1,"overrides":{"sites":{"fixture":{"request":{"enabled":true,"max_inflight":2,"requests_per_second":3}}}}}`)})
	w = runtimePost(a, `{"revision":1,"document":{"schema_version":1,"overrides":{}}}`)
	if w.Code != 200 || a.runtimeState.service.Revision() != 2 || a.Engine.Governance().Effective("fixture").Request.MaxInflight != 0 {
		t.Fatal("reset failed", w.Code, w.Body.String())
	}
}
func TestRuntimePolicyValidationBeforeWrite(t *testing.T) {
	for _, body := range []string{
		`{"revision":0,"document":{"schema_version":9,"overrides":{}}}`,
		`{"revision":0,"document":{"schema_version":1,"overrides":{"sites":{"unknown":{}}}}}`,
		`{"revision":0,"document":{"schema_version":1,"overrides":{"sites":{"fixture":{"stages":{"detail":{"max_concurrent":5}}}}}}}`,
		`{"revision":0,"document":{"schema_version":1,"overrides":{"sites":{"fixture":{"pool":"missing"}}}}}`,
		`{"revision":0,"document":{"schema_version":1,"overrides":{"unexpected":1}}}`,
		`{"revision":0,"document":{"schema_version":1,"overrides":{}}} {}`,
	} {
		t.Run(body, func(t *testing.T) {
			a, f := runtimeTestApp(t)
			before := f.Written()
			w := runtimePost(a, body)
			if w.Code != 400 || f.Written() != before {
				t.Fatal("invalid policy touched DB", w.Code, w.Body.String())
			}
		})
	}
}
func TestRuntimePolicyRestartUsesSavedOverrideAndRejectsCorruption(t *testing.T) {
	a, f := runtimeTestApp(t)
	f.SetRow(map[string]driver.Value{"revision": int64(7), "cfg_val": []byte(`{"schema_version":1,"overrides":{"sites":{"fixture":{"request":{"max_inflight":2}}}}}`)})
	if err := a.initRuntime(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.stopRuntime)
	if a.runtimeState.service.Revision() != 7 || a.Engine.Governance().Effective("fixture").Request.MaxInflight != 2 {
		t.Fatal("restart lost override")
	}
	for _, raw := range []string{`null`, `[]`, `{"schema_version":2}`, `{"schema_version":1,"secret":true}`} {
		if _, err := runtimecontrol.Decode([]byte(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
func TestRuntimeReadDoesNotWriteOrAuthenticate(t *testing.T) {
	a, f := runtimeTestApp(t)
	before := f.Written()
	w := httptest.NewRecorder()
	a.runtimeHandler().Policy(w, httptest.NewRequest("GET", "/api/runtime-policy", nil))
	var body map[string]any
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &body) != nil {
		t.Fatal(w.Body.String())
	}
	w = httptest.NewRecorder()
	a.accountPoolsHandler().PoolsAPI(w, httptest.NewRequest("GET", "/api/account-pools", nil))
	if w.Code != 200 || before != f.Written() {
		t.Fatal("read mutated state")
	}
}

func TestRuntimeUncertainGETReconcilesDurableRevision(t *testing.T) {
	a, f := runtimeTestApp(t)
	f.SetExecError("offline")
	if w := runtimePost(a, `{"revision":0,"document":{"schema_version":1,"overrides":{}}}`); w.Code != 503 {
		t.Fatal(w.Code)
	}
	f.SetExecError("")
	f.SetQueryError("crawler_config", "offline")
	w := httptest.NewRecorder()
	a.runtimeHandler().Policy(w, httptest.NewRequest("GET", "/api/runtime-policy", nil))
	if w.Code != 503 || !a.runtimeState.service.Uncertain() {
		t.Fatal("uncertain result reported as confirmed")
	}
	f.SetQueryError("", "")
	f.SetRow(map[string]driver.Value{"revision": int64(3), "cfg_val": []byte(`{"schema_version":1,"overrides":{"sites":{"fixture":{"request":{"max_inflight":1}}}}}`)})
	w = httptest.NewRecorder()
	a.runtimeHandler().Policy(w, httptest.NewRequest("GET", "/api/runtime-policy", nil))
	if w.Code != 200 || a.runtimeState.service.Uncertain() || a.runtimeState.service.Revision() != 3 || a.Engine.Governance().Effective("fixture").Request.MaxInflight != 1 {
		t.Fatal("GET retained stale cache", w.Body.String())
	}
}
