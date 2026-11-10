package onboarding

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/go-rod/rod"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/browser"
	"github.com/ydtg1993/papa/v4/pkg/egress"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

type remoteFixtureProvider struct {
	fixtureProvider
	pool       *browser.Pool
	url        string
	disconnect time.Duration
}

func (p *remoteFixtureProvider) Descriptor() Descriptor {
	return Descriptor{Site: "fixture", Label: "Browser fixture", Methods: []Method{{ID: "remote", Kind: "remote", Label: "Browser login"}}}
}
func (p *remoteFixtureProvider) Start(ctx context.Context, profile Profile, method string, input map[string]string) (Flow, error) {
	r, err := NewRemote(ctx, RemoteOptions{Pool: p.pool, URL: p.url, AllowedOrigins: []string{p.url}, Width: 1280, Height: 720, DisconnectTimeout: p.disconnect})
	if err != nil {
		return nil, err
	}
	return &remoteFixtureFlow{remote: r, url: p.url}, nil
}

type remoteFixtureFlow struct {
	remote  *Remote
	url     string
	started bool
}

func (f *remoteFixtureFlow) Next(ctx context.Context, input map[string]string) (Step, error) {
	if !f.started {
		f.started = true
		return Step{State: "remote", Remote: f.remote}, nil
	}
	c := &account.Credentials{Session: htmlfetch.NewSession(), Values: map[string]string{"verified": "yes"}}
	err := f.remote.WithPage(ctx, func(page *rod.Page) error {
		cookies, err := page.Cookies([]string{f.url})
		if err != nil {
			return err
		}
		for _, cookie := range cookies {
			if cookie.Name == "session" {
				c.Values["subject"] = cookie.Value
			}
		}
		return nil
	})
	if err != nil {
		return Step{}, err
	}
	if c.Values["subject"] == "" {
		return Step{State: "remote", Remote: f.remote}, nil
	}
	return Step{Candidate: c}, nil
}
func (f *remoteFixtureFlow) Close() error { f.remote.Close(); return nil }

func remoteFixture(t *testing.T) (*browser.Pool, string) {
	t.Helper()
	bin := os.Getenv("PAPA_EGRESS_BROWSER_BIN")
	if bin == "" {
		t.Skip("PAPA_EGRESS_BROWSER_BIN not configured")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			if r.Method != http.MethodPost {
				w.WriteHeader(405)
				return
			}
			_ = r.ParseForm()
			http.SetCookie(w, &http.Cookie{Name: "session", Value: r.Form.Get("subject"), HttpOnly: true, Path: "/"})
			fmt.Fprint(w, "ok")
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><html><body><input aria-label="account" style="position:absolute;left:20px;top:20px;width:200px;height:40px"><button style="position:absolute;left:20px;top:100px;width:120px;height:40px" onclick="fetch('/login',{method:'POST',headers:{'Content-Type':'application/x-www-form-urlencoded'},body:'subject='+encodeURIComponent(document.querySelector('input').value)}).then(()=>document.title='signed in')">Login</button><a style="position:absolute;left:220px;top:100px;width:120px;height:40px" target="_blank" href="/popup">Popup</a></body></html>`)
	}))
	t.Cleanup(srv.Close)
	pool, err := browser.NewPool(browser.PoolConfig{Size: 1, Headless: true, NoSandbox: true, BrowserPath: bin, Network: egress.Config{PrivateOrigins: []string{srv.URL}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	return pool, srv.URL
}

func TestRemoteRealBrowserViewportInputPopupIdentityAndOwnership(t *testing.T) {
	pool, url := remoteFixture(t)
	secrets, err := account.NewFileStore(t.TempDir(), []byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	m := account.NewManager(htmlfetch.NewClient(htmlfetch.Config{}))
	s, err := New(Options{Manager: m, Profiles: NewMemoryStore(), Secrets: secrets, TTL: 30 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.RegisterProvider(&remoteFixtureProvider{pool: pool, url: url}); err != nil {
		t.Fatal(err)
	}
	p := draftFixture(t, s, "remote")
	v := startFixture(t, s, p, "remote", nil, false)
	v = waitState(t, s, v.ID, "remote")
	if _, err = s.Remote(context.Background(), "other", RemoteRequest{ID: v.ID, Action: "claim"}); !errors.Is(err, ErrDenied) {
		t.Fatal("other operator controlled browser", err)
	}
	frame, err := s.Remote(context.Background(), "operator", RemoteRequest{ID: v.ID, Action: "claim"})
	if err != nil {
		t.Fatal(err)
	}
	if frame.Width != 1280 || frame.Height != 720 || frame.Lease == "" {
		t.Fatal(frame)
	}
	pngData, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(frame.ImageDataURL, "data:image/png;base64,"))
	if err != nil {
		t.Fatal(err)
	}
	im, err := png.Decode(bytes.NewReader(pngData))
	if err != nil || im.Bounds().Dx() != 1280 || im.Bounds().Dy() != 720 {
		t.Fatalf("viewport mapping %v %v", im, err)
	}
	if _, err = s.Remote(context.Background(), "operator", RemoteRequest{ID: v.ID, Action: "claim"}); !errors.Is(err, ErrBusy) {
		t.Fatal("second controller acquired", err)
	}
	if _, err = s.Remote(context.Background(), "operator", RemoteRequest{ID: v.ID, Action: "click", Lease: "wrong"}); !errors.Is(err, ErrDenied) {
		t.Fatal("wrong lease", err)
	}
	command := func(in RemoteRequest) RemoteFrame {
		t.Helper()
		time.Sleep(25 * time.Millisecond)
		in.ID = v.ID
		in.Lease = frame.Lease
		out, err := s.Remote(context.Background(), "operator", in)
		if err != nil {
			t.Fatal(in.Action, err)
		}
		return out
	}
	command(RemoteRequest{Action: "click", X: 60.0 / 1280, Y: 40.0 / 720})
	command(RemoteRequest{Action: "text", Text: "alice"})
	command(RemoteRequest{Action: "click", X: 60.0 / 1280, Y: 110.0 / 720})
	command(RemoteRequest{Action: "click", X: 250.0 / 1280, Y: 110.0 / 720})
	time.Sleep(350 * time.Millisecond)
	popup := command(RemoteRequest{Action: "frame"})
	if len(popup.Pages) != 2 {
		t.Fatalf("popup pages %+v", popup.Pages)
	}
	var selectID string
	for _, page := range popup.Pages {
		if page.ID != popup.Page {
			selectID = page.ID
		}
	}
	time.Sleep(350 * time.Millisecond)
	selected := command(RemoteRequest{Action: "select", Page: selectID})
	if selected.Page != selectID {
		t.Fatal("popup selection failed")
	}
	for _, bad := range []RemoteRequest{{Action: "eval", Text: "document.cookie"}, {Action: "key", Key: "F12"}, {Action: "click", X: 1.1, Y: .2}, {Action: "select", Page: "not-authorized"}} {
		time.Sleep(25 * time.Millisecond)
		bad.ID = v.ID
		bad.Lease = frame.Lease
		if _, err = s.Remote(context.Background(), "operator", bad); err == nil {
			t.Fatalf("unsafe command accepted %+v", bad)
		}
	}
	if _, err = s.Act("operator", v.ID, v.Revision, "continue", nil); err != nil {
		t.Fatal(err)
	}
	result := confirmFixture(t, s, v.ID)
	if result.State != "succeeded" {
		t.Fatal(result)
	}
	if _, err = s.Remote(context.Background(), "operator", RemoteRequest{ID: v.ID, Action: "frame", Lease: frame.Lease}); !errors.Is(err, ErrGone) {
		t.Fatal("completed browser still controllable", err)
	}
}

func TestRemoteRealBrowserDisconnectCancellationAndScope(t *testing.T) {
	pool, url := remoteFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := NewRemote(ctx, RemoteOptions{Pool: pool, URL: url, AllowedOrigins: []string{"https://other.invalid"}}); !errors.Is(err, ErrDenied) {
		t.Fatal(err)
	}
	r, err := NewRemote(ctx, RemoteOptions{Pool: pool, URL: url, AllowedOrigins: []string{url}, DisconnectTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.Done():
	case <-time.After(3 * time.Second):
		t.Fatal("disconnected remote did not expire")
	}
	r.Close()
	if _, err = r.request(ctx, RemoteRequest{Action: "claim"}); !errors.Is(err, ErrGone) {
		t.Fatal(err)
	}
	r, err = NewRemote(ctx, RemoteOptions{Pool: pool, URL: url, AllowedOrigins: []string{url}})
	if err != nil {
		t.Fatal("browser slot leaked", err)
	}
	frame, err := r.request(ctx, RemoteRequest{Action: "claim"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.request(ctx, RemoteRequest{Action: "release", Lease: frame.Lease}); err != nil {
		t.Fatal(err)
	}
	r.Close()
	select {
	case <-r.Done():
	default:
		t.Fatal("release did not close")
	}
}

func TestRemoteBusyRejectsWithoutQueuing(t *testing.T) {
	r := &Remote{}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, err := r.request(context.Background(), RemoteRequest{Action: "claim"}); !errors.Is(err, ErrBusy) {
		t.Fatal("remote request queued behind busy controller", err)
	}
}
