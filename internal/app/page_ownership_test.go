package app

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPageOwnershipValidationAndMetadata(t *testing.T) {
	a := &App{}
	a.UsePage(Page{Key: "workflow", Owns: []string{"module:rules", "page:detail"}, Script: "/* trusted */"})
	mux := http.NewServeMux()
	a.mountCustomRoutes(mux, nil)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/api/pages", nil))
	if !strings.Contains(w.Body.String(), `"owns":["module:rules","page:detail"]`) {
		t.Fatal(w.Body.String())
	}
	for _, p := range []Page{{Key: "duplicate", Owns: []string{"module:rules"}}, {Key: "detail", Owns: []string{"page:workflow"}}, {Key: "invalid", Owns: []string{"bad:module"}}} {
		t.Run(p.Key, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Fatal("expected ownership rejection")
				}
			}()
			p.Script = "/* trusted */"
			a.UsePage(p)
		})
	}
}
