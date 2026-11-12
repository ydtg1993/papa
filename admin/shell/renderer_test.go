package shell

import (
	"net/http/httptest"
	"sync"
	"testing"
	"testing/fstest"
)

type quietLogger struct{}

func (quietLogger) Errorf(string, ...any) {}
func TestRendererConcurrentRequestsUseCurrentPresentationCapabilities(t *testing.T) {
	assets := fstest.MapFS{"template.html": {Data: []byte(`{{.AuthRequired}}/{{.OaoCharts}}/{{.OaoMotion}}`)}}
	renderer := NewRenderer(assets, RenderOptions{AuthRequired: func() bool { return false }, OptionalAssets: func() (bool, bool) { return true, false }, Logger: quietLogger{}})
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := httptest.NewRecorder()
			renderer.ServeHTTP(w, httptest.NewRequest("GET", "/monitor", nil))
			if w.Code != 200 || w.Body.String() != "false/true/false" || w.Header().Get("Content-Type") != "text/html; charset=utf-8" {
				t.Errorf("render %d %s", w.Code, w.Body.String())
			}
		}()
	}
	wg.Wait()
}
func TestRendererTemplateFailureReturnsGenericError(t *testing.T) {
	renderer := NewRenderer(fstest.MapFS{}, RenderOptions{Logger: quietLogger{}})
	w := httptest.NewRecorder()
	renderer.ServeHTTP(w, httptest.NewRequest("GET", "/monitor", nil))
	if w.Code != 500 || w.Body.String() != "Internal Server Error\n" {
		t.Fatal(w.Code, w.Body.String())
	}
}
