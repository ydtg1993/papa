package tasktrace

import (
	"path/filepath"
	"testing"
)

func TestResolveArchivedFile(t *testing.T) {
	root := filepath.Join("tmp", "archive")
	ok := []struct{ rel, want string }{
		{"example/task-1-try-0-aaaaaaaa.html", filepath.Join(root, "example", "task-1-try-0-aaaaaaaa.html")},
		{"example/./task-1-try-0-aaaaaaaa.html", filepath.Join(root, "example", "task-1-try-0-aaaaaaaa.html")},
	}
	for _, c := range ok {
		got, err := ResolveArchivedFile(root, c.rel)
		if err != nil || got != c.want {
			t.Fatalf("resolve(%q) = %q, %v; want %q", c.rel, got, err, c.want)
		}
	}
	for _, bad := range []string{"", "..", "../x", "a/../../x", "/abs/path", "a\x00b"} {
		if _, err := ResolveArchivedFile(root, bad); err == nil {
			t.Fatalf("resolve(%q) 应当报错", bad)
		}
	}
}
