package server

import (
	"io/fs"
	"testing"
)

func TestStaticEmbedded(t *testing.T) {
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		t.Fatalf("fs.Sub: %v", err)
	}
	for _, name := range []string{"mo.css", "mo.js"} {
		if _, err := fs.Stat(sub, name); err != nil {
			t.Errorf("expected static/%s: %v", name, err)
		}
	}
}
