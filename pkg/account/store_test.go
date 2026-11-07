package account

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/ydtg1993/papa/v4/pkg/browser"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

func TestAccountEncryptedStoreRestartLogoutAndTamper(t *testing.T) {
	ctx := context.Background()
	s := newAuthSite(t)
	dir := t.TempDir()
	secret := bytes.Repeat([]byte{9}, 32)
	store, err := NewFileStore(dir, secret)
	if err != nil {
		t.Fatal(err)
	}
	key := Key{"site", "alice"}
	cfg := s.cfg("alice")
	cfg.Store = store
	m := NewManager(s.client)
	register(t, m, key, cfg)
	if _, err = m.Do(ctx, key, htmlfetch.Request{URL: s.server.URL}); err != nil {
		t.Fatal(err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "*.state"))
	if len(files) != 1 {
		t.Fatal(files)
	}
	data, _ := os.ReadFile(files[0])
	if bytes.Contains(data, []byte("private-refresh")) || bytes.Contains(data, []byte("alice:1")) {
		t.Fatal("plaintext credentials on disk")
	}
	restarted := NewManager(s.client)
	register(t, restarted, key, cfg)
	if _, err = restarted.Do(ctx, key, htmlfetch.Request{URL: s.server.URL}); err != nil {
		t.Fatal(err)
	}
	if n, checks, _ := s.counts(); n != 1 || checks != 2 {
		t.Fatalf("restart login/check=%d/%d", n, checks)
	}
	other := Key{"site", "bob"}
	otherPath, _ := store.location(other)
	if err = os.WriteFile(otherPath, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Load(ctx, other); err == nil {
		t.Fatal("cross-account state accepted")
	}
	data[len(data)-1] ^= 1
	if err = os.WriteFile(files[0], data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Load(ctx, key); err == nil {
		t.Fatal("tamper accepted")
	}
	if err = restarted.Logout(ctx, key); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Load(ctx, key); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if _, err = restarted.Do(ctx, key, htmlfetch.Request{URL: s.server.URL}); !errors.Is(err, ErrPaused) {
		t.Fatal(err)
	}
	if err = restarted.Resume(key); err != nil {
		t.Fatal(err)
	}
	if _, err = restarted.Do(ctx, key, htmlfetch.Request{URL: s.server.URL}); err != nil {
		t.Fatal(err)
	}
	if n, _, _ := s.counts(); n != 2 {
		t.Fatal(n)
	}
}

type faultStore struct{ mode string }

func (f *faultStore) Load(context.Context, Key) (*StoredState, error) { return nil, os.ErrNotExist }
func (f *faultStore) Save(context.Context, Key, *StoredState) error {
	if f.mode == "panic" {
		panic("store panic")
	}
	return errors.New("disk error with secret")
}
func (f *faultStore) Delete(context.Context, Key) error { return errors.New("delete failed") }
func TestAccountStoreFailureDoesNotPublishOrDeadlock(t *testing.T) {
	for _, mode := range []string{"error", "panic"} {
		t.Run(mode, func(t *testing.T) {
			s := newAuthSite(t)
			m := NewManager(s.client)
			key := Key{"site", "alice"}
			cfg := s.cfg("alice")
			cfg.Store = &faultStore{mode: mode}
			register(t, m, key, cfg)
			func() {
				defer func() {
					p := recover()
					if mode == "panic" && p == nil {
						t.Error("store panic not propagated")
					}
				}()
				_, err := m.Do(context.Background(), key, htmlfetch.Request{URL: s.server.URL})
				if mode == "error" && (err == nil || strings.Contains(err.Error(), "secret")) {
					t.Fatal(err)
				}
			}()
			if m.Status()[0].HTTPReady {
				t.Fatal("unpersisted authentication published")
			}
			if err := m.Pause(key); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestAccountRefusesUnrestorableStateBeforeOverwrite(t *testing.T) {
	s := newAuthSite(t)
	m := NewManager(s.client)
	key := Key{"site", "alice"}
	cfg := s.cfg("alice")
	store, err := NewFileStore(t.TempDir(), bytes.Repeat([]byte{4}, 32))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Store = store
	register(t, m, key, cfg)
	ctx := context.Background()
	if _, err = m.Do(ctx, key, htmlfetch.Request{URL: s.server.URL}); err != nil {
		t.Fatal(err)
	}
	path, _ := store.location(key)
	before, _ := os.ReadFile(path)
	e, _ := m.get(key)
	u, _ := url.Parse(s.server.URL)
	e.mu.Lock()
	cookies := make([]*http.Cookie, htmlfetch.MaxSessionStateCookies+1)
	for i := range cookies {
		cookies[i] = &http.Cookie{Name: fmt.Sprintf("extra%d", i), Value: "x", Path: "/"}
	}
	e.current.Session.SetCookies(u, cookies)
	saveErr := e.saveLocked(ctx)
	e.mu.Unlock()
	if saveErr == nil {
		t.Fatal("saved snapshot that cannot restore")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("invalid snapshot overwrote valid state")
	}
	if _, err := clonePageState(&browser.PageState{Version: 999, Origin: s.server.URL}); err == nil {
		t.Fatal("invalid browser state accepted")
	}
	if got, err := origin("https://BÜCHER.example:443"); err != nil || got != "https://xn--bcher-kva.example" {
		t.Fatal(got, err)
	}
}
