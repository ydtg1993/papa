package app

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/admin/auth"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/account/onboarding"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

type accountUIProvider struct{}

func (accountUIProvider) Descriptor() onboarding.Descriptor {
	return onboarding.Descriptor{Site: "fixture", Label: "合成登录站点", Methods: []onboarding.Method{{ID: "password", Kind: "password", Label: "密码登录", Fields: []onboarding.Field{{Name: "password", Label: "密码", Secret: true, Required: true}}}}}
}
func (accountUIProvider) Config(context.Context, onboarding.Profile, func(context.Context) (map[string]string, error)) (account.Config, error) {
	return account.Config{Origins: []string{"https://fixture.invalid"}, Login: func(context.Context, *account.Credentials) error { return errors.New("manual login required") }, Check: func(_ context.Context, c *account.Credentials) (bool, error) {
		return c.Values["subject"] == "alice", nil
	}}, nil
}
func (accountUIProvider) Start(_ context.Context, _ onboarding.Profile, _ string, in map[string]string) (onboarding.Flow, error) {
	if in["password"] != "private-password" {
		return nil, errors.New("sensitive provider error token=must-not-leak")
	}
	return accountUIFlow{}, nil
}
func (accountUIProvider) Verify(context.Context, onboarding.Profile, *account.Credentials) (onboarding.Identity, error) {
	return onboarding.Identity{Subject: "alice", Display: "ali***", HTTP: true, Scopes: []string{"read"}}, nil
}

type accountUIFlow struct{}

func (accountUIFlow) Next(context.Context, map[string]string) (onboarding.Step, error) {
	return onboarding.Step{Candidate: &account.Credentials{Session: htmlfetch.NewSession(), Headers: map[string]string{"Authorization": "Bearer private-token"}, Values: map[string]string{"subject": "alice"}}}, nil
}
func (accountUIFlow) Close() error { return nil }
func accountsTestApp(t *testing.T) *App {
	t.Helper()
	a, _ := runtimeTestApp(t)
	m := account.NewManager(htmlfetch.NewClient(htmlfetch.Config{}))
	a.Engine.SetAccountManager(m)
	secrets, err := account.NewFileStore(t.TempDir(), []byte("01234567890123456789012345678901"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := onboarding.New(onboarding.Options{Manager: m, Profiles: onboarding.NewMemoryStore(), Secrets: secrets})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RegisterProvider(accountUIProvider{}); err != nil {
		t.Fatal(err)
	}
	if err = a.UseAccountOnboarding(s); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return a
}

func TestUseAccountOnboardingRejectsManagerMismatch(t *testing.T) {
	a := accountsTestApp(t)
	secrets, _ := account.NewFileStore(t.TempDir(), []byte("01234567890123456789012345678901"))
	other, err := onboarding.New(onboarding.Options{Manager: account.NewManager(htmlfetch.NewClient(htmlfetch.Config{})), Profiles: onboarding.NewMemoryStore(), Secrets: secrets})
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if err = a.UseAccountOnboarding(other); err == nil {
		t.Fatal("replaced installed service")
	}
	a2, _ := runtimeTestApp(t)
	a2.Engine.SetAccountManager(account.NewManager(nil))
	if err = a2.UseAccountOnboarding(other); err == nil {
		t.Fatal("mismatched manager accepted")
	}
}
func accountsRequest(a *App, operator, method, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r = r.WithContext(auth.WithOperator(r.Context(), operator))
	w := httptest.NewRecorder()
	if path == "/api/accounts/remote" {
		a.accountsHandler().Remote(w, r)
	} else {
		a.accountsHandler().Accounts(w, r)
	}
	return w
}
func TestAccountsAPIRequiresOperatorAndRedactsSensitiveState(t *testing.T) {
	a := accountsTestApp(t)
	for _, path := range []string{"/api/accounts", "/api/accounts/remote"} {
		w := accountsRequest(a, "", http.MethodPost, path, `{}`)
		if w.Code != 401 {
			t.Fatalf("unauthenticated %s=%d", path, w.Code)
		}
	}
	w := accountsRequest(a, "operator", http.MethodPost, "/api/accounts", `{"action":"draft","profile":{"key":{"site":"fixture","id":"one"},"label":"合成账号","state":"ready","identity":{"subject":"attacker"}}}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var p onboarding.Profile
	_ = json.Unmarshal(w.Body.Bytes(), &p)
	if p.State != "draft" || p.Identity != nil {
		t.Fatal("client controlled verification state")
	}
	w = accountsRequest(a, "operator", http.MethodPost, "/api/accounts", `{"action":"start","key":{"site":"fixture","id":"one"},"revision":1,"method":"password","input":{"password":"private-password"}}`)
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var v onboarding.Progress
	_ = json.Unmarshal(w.Body.Bytes(), &v)
	if strings.Contains(w.Body.String(), "private-password") {
		t.Fatal("secret leaked")
	}
	w = accountsRequest(a, "other", http.MethodGet, "/api/accounts?process="+v.ID, "")
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
	w = accountsRequest(a, "other", http.MethodPost, "/api/accounts", `{"action":"cancel","id":"`+v.ID+`"}`)
	if w.Code != 403 {
		t.Fatal(w.Code)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		w = accountsRequest(a, "operator", http.MethodGet, "/api/accounts?process="+v.ID, "")
		_ = json.Unmarshal(w.Body.Bytes(), &v)
		if v.State == "confirm_identity" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if v.State != "confirm_identity" {
		t.Fatal(v)
	}
	body, _ := json.Marshal(map[string]any{"action": "confirm", "id": v.ID, "revision": v.Revision})
	w = accountsRequest(a, "operator", http.MethodPost, "/api/accounts", string(body))
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	for time.Now().Before(deadline) {
		w = accountsRequest(a, "operator", http.MethodGet, "/api/accounts?process="+v.ID, "")
		_ = json.Unmarshal(w.Body.Bytes(), &v)
		if v.State == "succeeded" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if v.State != "succeeded" {
		t.Fatal(v)
	}
	w = accountsRequest(a, "operator", http.MethodGet, "/api/accounts", "")
	for _, secret := range []string{"private-password", "private-token", "secret_ref", "inputs_ref"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("public response contains %s", secret)
		}
	}
	w = accountsRequest(a, "operator", http.MethodGet, "/api/accounts?process=before-restart", "")
	if w.Code != 410 {
		t.Fatal(w.Code)
	}
	w = accountsRequest(a, "operator", http.MethodPost, "/api/accounts/remote", `{"action":"eval","eval":"document.cookie"}`)
	if w.Code != 400 {
		t.Fatal(w.Code)
	}
}
