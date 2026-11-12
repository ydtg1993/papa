package shell

import (
	"bytes"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

func TestAssembledResourcesImplementReadOnlyFS(t *testing.T) {
	assets := Assets()
	if err := fstest.TestFS(assets, "template.html", "static/mo.js", "static/mo.css", "static/fonts/archivo-black-latin.woff2"); err != nil {
		t.Fatal(err)
	}
	before, err := assets.ReadFile("static/mo.js")
	if err != nil {
		t.Fatal(err)
	}
	copy, _ := assets.ReadFile("static/mo.js")
	copy[0] ^= 0xff
	after, _ := assets.ReadFile("static/mo.js")
	if !bytes.Equal(before, after) {
		t.Fatal("caller changed shared resource data")
	}
	for _, name := range []string{"../template.html", "/template.html"} {
		if _, err := assets.Open(name); !errors.Is(err, fs.ErrInvalid) {
			t.Fatalf("invalid path %q: %v", name, err)
		}
	}
	page, _ := assets.ReadFile("template.html")
	css, _ := assets.ReadFile("static/mo.css")
	if strings.Contains(string(page), "{{/* feature:") || strings.Contains(string(css), "/* feature-css:") {
		t.Fatal("unexpanded feature fragments shipped")
	}
	if _, err := assets.ReadFile("static/controller.js"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("ambiguous controller basename was published", err)
	}
}

func TestResourceHTTPRangePreservesFontBytes(t *testing.T) {
	assets := Assets()
	data, err := assets.ReadFile("static/fonts/archivo-black-latin.woff2")
	if err != nil {
		t.Fatal(err)
	}
	sub, err := fs.Sub(assets, "static")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/fonts/archivo-black-latin.woff2", nil)
	req.Header.Set("Range", "bytes=3-17")
	w := httptest.NewRecorder()
	http.FileServer(http.FS(sub)).ServeHTTP(w, req)
	if w.Code != http.StatusPartialContent || !bytes.Equal(w.Body.Bytes(), data[3:18]) {
		t.Fatalf("font range changed: status=%d bytes=%d", w.Code, w.Body.Len())
	}
}

func TestAssemblyRejectsIncompleteAndDuplicateInputs(t *testing.T) {
	missing := fstest.MapFS{"views/template.html": {Data: []byte("{{/* feature:missing/page.html */}}")}}
	if _, err := assemble([]featureSource{{"shell", missing}}); err == nil || !strings.Contains(err.Error(), "unresolved feature template") {
		t.Fatalf("missing fragment silently accepted: %v", err)
	}
	if _, err := assemble([]featureSource{{"shell", missing}, {"shell", missing}}); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("duplicate resource ownership silently accepted: %v", err)
	}
}
