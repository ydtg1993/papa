package browser

import (
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/proto"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

func TestSessionBridgeCookieScopeAttributesAndDeletion(t *testing.T) {
	s := htmlfetch.NewSession()
	u, _ := url.Parse("https://site.example.test/auth/login")
	other, _ := url.Parse("https://other.test/")
	expires := time.Now().Add(time.Hour).Truncate(time.Second)
	s.SetCookies(u, []*http.Cookie{
		{Name: "host", Value: "secret", Path: "/auth", Secure: true, HttpOnly: true, SameSite: http.SameSiteStrictMode, Expires: expires},
		{Name: "domain", Value: "shared", Domain: "example.test", Path: "/", SameSite: http.SameSiteLaxMode},
		{Name: "deleted", Value: "old", Path: "/"},
	})
	s.SetCookies(other, []*http.Cookie{{Name: "other", Value: "private", Path: "/"}})
	state, err := PageStateFromSession(s, u.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Cookies) != 3 {
		t.Fatalf("scoped cookie count = %d", len(state.Cookies))
	}
	var host *proto.NetworkCookie
	kept := state.Cookies[:0]
	for _, c := range state.Cookies {
		if c.Name == "host" {
			host = c
		}
		if c.Name != "deleted" {
			kept = append(kept, c)
		}
	}
	if host == nil || host.Domain != "site.example.test" || host.Path != "/auth" || !host.HTTPOnly || !host.Secure || host.Session || host.SameSite != proto.NetworkCookieSameSiteStrict || int64(host.Expires) != expires.Unix() {
		t.Fatal("cookie attributes changed")
	}
	state.Cookies = kept
	host.Value = "new"
	back, err := SessionWithPageState(s, state)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Snapshot().Cookies) != 4 || len(back.Snapshot().Cookies) != 3 {
		t.Fatal("deletion or source isolation failed")
	}
	for _, c := range back.Snapshot().Cookies {
		if c.Cookie.Name == "host" && c.Cookie.Domain != "" {
			t.Fatal("host-only cookie widened")
		}
	}
	child, _ := url.Parse("https://child.site.example.test/auth")
	for _, c := range back.Cookies(child) {
		if c.Name == "host" {
			t.Fatal("host cookie leaked to subdomain")
		}
	}
	if len(back.Cookies(other)) != 1 {
		t.Fatal("unrelated origin lost")
	}
	state.Cookies[0].PartitionKeyOpaque = true
	if _, err = SessionWithPageState(s, state); err == nil {
		t.Fatal("partitioned cookie accepted")
	}
}
