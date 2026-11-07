package server

import (
	"net/http"
	"strings"
	"testing"
	"testing/fstest"
)

func TestMonitorOptionalVisualReferencesMatchLinkedDependency(t *testing.T) {
	response := serve(newFakeMonitor(t), http.MethodGet, "/monitor", nil)
	if response.Code != http.StatusOK {
		t.Fatal(response.Code)
	}
	charts, motion := optionalOaoAssets()
	for name, present := range map[string]bool{"oao-charts": charts, "oao-motion": motion} {
		for _, ext := range []string{".js", ".css"} {
			if strings.Contains(response.Body.String(), "/static/oao/"+name+ext) != present {
				t.Fatalf("optional %s%s referenced without matching linked capability", name, ext)
			}
		}
	}
}

func TestOptionalVisualAssetsRequireCompletePair(t *testing.T) {
	assets := fstest.MapFS{"oao-charts.js": {Data: []byte("chart")}}
	if hasVisualAssets(assets, "oao-charts") {
		t.Fatal("missing stylesheet must not enable charts")
	}
	assets["oao-charts.css"] = &fstest.MapFile{Data: []byte("style")}
	if !hasVisualAssets(assets, "oao-charts") || hasVisualAssets(assets, "oao-motion") {
		t.Fatal("optional modules must be detected independently")
	}
}

func TestMonitorReducedMotionDisablesFeedbackTransforms(t *testing.T) {
	b, err := staticFS.ReadFile("static/visuals.css")
	if err != nil {
		t.Fatal(err)
	}
	css := string(b)
	feedback := ":is(.nav-item,.theme-toggle,.site-tab,.card,.tile,.btn,.pager .pg,.bell):is(:hover,:active) { transform:none !important; }"
	if !strings.Contains(css, `html[data-oao-motion="reduced"] `+feedback) || !strings.Contains(css, "@media(prefers-reduced-motion:reduce) { "+feedback) {
		t.Fatal("both user and system reduced motion must disable feedback transforms")
	}
	if strings.Contains(css, "* { transform:none") {
		t.Fatal("reduced motion must preserve structural transforms such as switch state")
	}
}
