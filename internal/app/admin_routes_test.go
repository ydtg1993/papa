package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v4/admin/accountpools"
	"github.com/ydtg1993/papa/v4/admin/accounts"
	"github.com/ydtg1993/papa/v4/admin/diagnostics"
	adminruntime "github.com/ydtg1993/papa/v4/admin/runtimecontrol"
	"github.com/ydtg1993/papa/v4/admin/server"
	"github.com/ydtg1993/papa/v4/admin/taskflow"
	"github.com/ydtg1993/papa/v4/config"
)

func TestBuiltinPagesPreserveKeysMetadataAndResourceOrder(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		a := &App{Config: &config.Config{}}
		a.Config.TaskFlow.Enabled = enabled
		a.UsePage(Page{Key: "business", Script: "window.business = true;"})
		a.UseCSS("body { color: inherit; }")
		a.registerBuiltinPages()
		keys := []string{"business", "browser-diagnostics", "crawl-control", "account-pools", "accounts"}
		scripts := []string{"window.business = true;", diagnostics.Script, adminruntime.Script, accountpools.Script, accounts.Script}
		if enabled {
			keys = append(keys, "task-flow-policy")
			scripts = append(scripts, taskflow.Script)
		}
		if len(a.pages) != len(keys) {
			t.Fatalf("pages: %v", a.pages)
		}
		for i, key := range keys {
			if a.pages[i].Key != key || a.extraJS[i] != scripts[i] {
				t.Fatalf("page/resource order at %d", i)
			}
		}
		if a.pages[1].Section != "system" || a.pages[2].Section != "monitor" || a.pages[3].Section != "data" || a.pages[4].Label != "账号管理" {
			t.Fatal("builtin navigation compatibility changed")
		}
		if enabled && (a.pages[5].Group != "调度" || a.pages[5].Section != "flow" || a.extraCSS[1] != taskflow.CSS) {
			t.Fatal("task flow page compatibility changed")
		}
		mux := http.NewServeMux()
		a.mountCustomRoutes(mux, nil)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", "/static/custom.js", nil))
		if w.Body.String() != strings.Join(scripts, "\n;\n") {
			t.Fatal("served script composition differs")
		}
	}
}

func TestAllBuiltinAPIsRemainBehindHostGuard(t *testing.T) {
	a, _ := runtimeTestApp(t)
	mux := http.NewServeMux()
	a.mountAdminRoutes(mux, func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) })
	})
	for _, path := range []string{"/api/browser-diagnostics", "/api/browser-identities", "/api/runtime-policy", "/api/runtime-remote", "/api/account-pools", "/api/accounts", "/api/accounts/remote", "/api/account-compensation", "/api/runtime-policy/history", "/api/proxy-health"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("POST", path, nil))
		if w.Code != http.StatusUnauthorized {
			t.Errorf("%s is not guarded: %d", path, w.Code)
		}
	}
}

func TestBuiltinTaskFlowPreservesPublicResourceOverrides(t *testing.T) {
	js, css := server.TaskFlowScript, server.TaskFlowCSS
	t.Cleanup(func() { server.TaskFlowScript, server.TaskFlowCSS = js, css })
	server.TaskFlowScript, server.TaskFlowCSS = "window.hostTaskFlow = true;", "/* host task flow */"
	a := &App{Config: &config.Config{}}
	a.Config.TaskFlow.Enabled = true
	a.registerBuiltinPages()
	last := a.pages[len(a.pages)-1]
	if last.Key != "task-flow-policy" || last.Script != server.TaskFlowScript || a.extraCSS[0] != server.TaskFlowCSS {
		t.Fatal("public TaskFlow resource override ignored")
	}
}
