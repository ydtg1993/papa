package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestExtensionMetadataOwnsRegistrationSnapshot(t *testing.T) {
	pages := []ExtensionPage{{Key: "fixture", Owns: []string{"page:original"}}}
	mux := http.NewServeMux()
	RegisterExtensions(mux, nil, pages, nil, nil)
	pages[0].Owns[0] = "page:changed"
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/pages", nil))
	var response struct {
		Pages []struct {
			Owns []string `json:"owns"`
		} `json:"pages"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Pages) != 1 || len(response.Pages[0].Owns) != 1 || response.Pages[0].Owns[0] != "page:original" {
		t.Fatalf("registered navigation retained caller-owned state: %s", w.Body.String())
	}
}

func TestExtensionMetadataGuardAndResourceOrder(t *testing.T) {
	mux := http.NewServeMux()
	guard := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("fixture-auth") != "yes" {
				w.WriteHeader(401)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
	RegisterExtensions(mux, guard, []ExtensionPage{{Key: "fixture"}}, []string{"first()", "second()"}, []string{"a{}", "b{}"})
	request := func(path string, authenticated bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		if authenticated {
			r.Header.Set("fixture-auth", "yes")
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	if w := request("/api/pages", false); w.Code != 401 {
		t.Fatal(w.Code)
	}
	if w := request("/api/pages", true); w.Code != 200 || !strings.Contains(w.Body.String(), `"section":"data"`) || !strings.Contains(w.Body.String(), `"label":"fixture"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request("/static/custom.js", false); w.Code != 200 || w.Body.String() != "first()\n;\nsecond()" {
		t.Fatal(w.Code, w.Body.String())
	}
	if w := request("/static/custom.css", false); w.Code != 200 || w.Body.String() != "a{}\nb{}" {
		t.Fatal(w.Code, w.Body.String())
	}
}
