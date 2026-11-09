package temporary

import (
	"errors"
	"github.com/google/uuid"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func makeFiles(t *testing.T) (Files, string, string) {
	t.Helper()
	f := Files{Parent: t.TempDir(), Namespace: "experiments"}
	id, file := uuid.NewString(), uuid.NewString()
	root, e := f.Open(true)
	if e != nil {
		t.Fatal(e)
	}
	defer root.Close()
	if e = root.Mkdir(id, 0700); e != nil {
		t.Fatal(e)
	}
	if e = root.WriteFile(filepath.Join(id, file), []byte("evidence"), 0600); e != nil {
		t.Fatal(e)
	}
	return f, id, file
}
func TestFilesConfineRemovalToOwnedUUID(t *testing.T) {
	f, id, file := makeFiles(t)
	root, e := f.Open(false)
	if e != nil {
		t.Fatal(e)
	}
	other := uuid.NewString()
	if e = root.Mkdir(other, 0700); e != nil {
		t.Fatal(e)
	}
	if e = root.WriteFile(filepath.Join(other, file), []byte("keep"), 0600); e != nil {
		t.Fatal(e)
	}
	root.Close()
	shared := filepath.Join(f.Parent, "shared")
	if e = os.WriteFile(shared, []byte("keep"), 0600); e != nil {
		t.Fatal(e)
	}
	for _, bad := range []string{"", uuid.Nil.String(), "..", "../" + id, strings.ToUpper(id), id + "/../" + other} {
		if f.Remove(bad) == nil {
			t.Fatal("unsafe removal accepted", bad)
		}
		if _, e = f.FilePath(id, bad); e == nil {
			t.Fatal("unsafe file accepted", bad)
		}
	}
	path, e := f.FilePath(id, file)
	if e != nil {
		t.Fatal(e)
	}
	if e = f.Remove(id); e != nil {
		t.Fatal(e)
	}
	if e = f.Remove(id); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(path); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("owned file survived", e)
	}
	if _, e = os.Stat(shared); e != nil {
		t.Fatal("shared file removed", e)
	}
	if _, e = f.FilePath(other, file); e != nil {
		t.Fatal("other experiment removed", e)
	}
}
func TestFilesRejectUnsafeNamespaceAndSymlink(t *testing.T) {
	f, id, _ := makeFiles(t)
	for _, ns := range []string{"", ".", "..", "../outside", "a/b", `C:\outside`} {
		bad := f
		bad.Namespace = ns
		if _, e := bad.Open(true); e == nil {
			t.Fatal("unsafe namespace", ns)
		}
	}
	outside := t.TempDir()
	marker := filepath.Join(outside, "keep")
	if e := os.WriteFile(marker, []byte("keep"), 0600); e != nil {
		t.Fatal(e)
	}
	link := filepath.Join(f.Parent, f.Namespace, id, "link")
	if e := os.Symlink(outside, link); e != nil {
		t.Skip("symlink creation not permitted on this host")
	}
	if e := f.Remove(id); e == nil {
		t.Fatal("linked subtree accepted")
	}
	if _, e := os.Stat(marker); e != nil {
		t.Fatal("external marker removed", e)
	}
}
