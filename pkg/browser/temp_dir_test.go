package browser

import (
	"context"
	"os"
	"path/filepath"
	"sync"
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
		t.Fatal("shared profile root removed after Close", err)
	}
}

// A configured root is a shared namespace, not an ownership grant.
func TestPoolClosePreservesSharedTempDirRoot(t *testing.T) {
	root := filepath.Join(t.TempDir(), "explicit")
	p, err := NewPool(PoolConfig{Size: 0, DirectSize: 0, TempDir: root})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	other, err := NewPool(PoolConfig{TempDir: root})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	sentinel := filepath.Join(root, "not-owned-by-pool")
	if err := os.WriteFile(sentinel, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	p.Close()
	if b, err := os.ReadFile(sentinel); err != nil || string(b) != "keep" {
		t.Fatalf("closing one pool removed shared data: %v", err)
	}
	profile, err := NewTempProfile(launcher.New(), other.TempDir(), "other-")
	if err != nil {
		t.Fatal("shared pool cannot allocate profile after sibling Close", err)
	}
	other.Close()
	if _, err := os.Stat(profile); err != nil {
		t.Fatal("pool removed a profile it does not own", err)
	}
}

func TestPoolClosePreservesCreatingProfile(t *testing.T) {
	root := t.TempDir()
	p, err := NewPool(PoolConfig{Size: 1, TempDir: root})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	profile := filepath.Join(root, "creating-profile")
	p.newBrowserFn = func(proxy bool) (*Browser, error) {
		if err := os.Mkdir(profile, 0700); err != nil {
			return nil, err
		}
		close(entered)
		<-release
		return testBrowser(proxy, true), nil
	}
	go func() { _, err := p.Get(context.Background()); done <- err }()
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("creation did not start")
	}
	p.Close()
	if _, err := os.Stat(profile); err != nil {
		t.Fatal("Close removed in-flight creation directory", err)
	}
	unblock()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("creation published after Close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("creation did not drain")
	}
}

func TestLivePoolClosePreservesBorrowedAndSharedProfiles(t *testing.T) {
	if os.Getenv("CRAWLER_ROD_DIAGNOSTIC_LIVE") != "1" {
		t.Skip("explicit live browser test")
	}
	root := t.TempDir()
	cfg := PoolConfig{Size: 1, TempDir: root, Headless: true, Leakless: false, NoSandbox: true}
	p, err := NewPool(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	q, err := NewPool(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	b, err := p.Get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	profile := b.launcher.Get(flags.UserDataDir)
	// Close returns without stealing a borrower's instance or profile.
	p.Close()
	if !b.IsAlive() {
		t.Fatal("Close killed borrowed browser")
	}
	if _, err := os.Stat(profile); err != nil {
		t.Fatal("borrowed profile removed", err)
	}
	if stats := p.Stats(); !stats.Closed || stats.Borrowed != 1 {
		t.Fatalf("borrowed state lost: %+v", stats)
	}
	other, err := q.Get(ctx)
	if err != nil {
		t.Fatal("shared root no longer usable", err)
	}
	defer other.Close()
	otherProfile := other.launcher.Get(flags.UserDataDir)
	if err := p.Put(b); err == nil {
		t.Fatal("closed pool accepted return as reusable")
	}
	if _, err := os.Stat(profile); !os.IsNotExist(err) {
		t.Fatal("returned profile not cleaned", err)
	}
	if !other.IsAlive() {
		t.Fatal("sibling browser affected by cleanup")
	}
	if _, err := os.Stat(otherProfile); err != nil {
		t.Fatal("sibling profile removed", err)
	}
	if stats := p.Stats(); stats.Live != 0 {
		t.Fatalf("returned instance not drained: %+v", stats)
	}
	if err := q.Put(other); err != nil {
		t.Fatal(err)
	}
	q.Close()
	if _, err := os.Stat(otherProfile); !os.IsNotExist(err) {
		t.Fatal("idle profile not cleaned", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Fatal("shared root removed", err)
	}
}
