package browser

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/launcher"
	"github.com/go-rod/rod/lib/launcher/flags"
)

func TestTempDirValidationAndProfileIsolation(t *testing.T) {
	root, err := PrepareTempDir(filepath.Join(t.TempDir(), "profiles"))
	if err != nil {
		t.Fatal(err)
	}
	l1, l2 := launcher.New(), launcher.New()
	a, err := NewTempProfile(l1, root, "browser-")
	if err != nil {
		t.Fatal(err)
	}
	b, err := NewTempProfile(l2, root, "browser-")
	if err != nil {
		t.Fatal(err)
	}
	if a == b || filepath.Dir(a) != root || filepath.Dir(b) != root || l1.Get(flags.UserDataDir) != a {
		t.Fatal(a, b, root)
	}
	if err = os.RemoveAll(a); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(b); err != nil {
		t.Fatal("sibling removed", err)
	}
	file := filepath.Join(root, "not-a-dir")
	if err = os.WriteFile(file, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = PrepareTempDir(file); err == nil {
		t.Fatal("accepted a file as profile root")
	}
	if _, err = PrepareTempDir(" "); err == nil {
		t.Fatal("accepted whitespace")
	}
}

func TestPoolTempDirConflictAndLaunchFailureCleanup(t *testing.T) {
	root := t.TempDir()
	for _, key := range []string{"user-data-dir", "--user-data-dir", "disk-cache-dir"} {
		if _, err := NewPool(PoolConfig{Size: 1, TempDir: root, Flags: map[string]string{key: "fixed"}}); err == nil {
			t.Fatalf("accepted conflict %s", key)
		}
	}
	p, err := NewPool(PoolConfig{Size: 1, TempDir: root, BinarySource: "path", BrowserPath: filepath.Join(root, "missing-executable")})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if p.TempDir() != root {
		t.Fatal(p.TempDir())
	}
	if _, err = p.Get(context.Background()); err == nil {
		t.Fatal("expected launch failure")
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatal("failed launch leaked profile", entries, err)
	}
}

func TestLivePoolTempDirCleanup(t *testing.T) {
	if os.Getenv("CRAWLER_ROD_DIAGNOSTIC_LIVE") != "1" {
		t.Skip("explicit live browser test")
	}
	root := t.TempDir()
	p, err := NewPool(PoolConfig{Size: 1, TempDir: root, Headless: true})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, err := p.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	profile := b.launcher.Get(flags.UserDataDir)
	if filepath.Dir(profile) != root {
		t.Fatalf("profile outside root: %s", profile)
	}
	if err = p.Put(b); err != nil {
		t.Fatal(err)
	}
	p.Close()
	if _, err = os.Stat(profile); !os.IsNotExist(err) {
		t.Fatalf("profile not cleaned: %v", err)
	}
	if _, err = os.Stat(root); err != nil {
		t.Fatal("root removed", err)
	}
}
