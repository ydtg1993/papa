package app

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

func TestRuntimeAccountPoolAPITombstoneAndReadOnly(t *testing.T) {
	a, _ := runtimeTestApp(t)
	m := account.NewManager(htmlfetch.NewClient(htmlfetch.Config{}))
	var authCalls atomic.Int32
	if err := m.Register(account.Key{Site: "fixture", ID: "first"}, account.Config{
		Origins: []string{"https://fixture.invalid"},
		Login:   func(context.Context, *account.Credentials) error { authCalls.Add(1); return nil },
		Check:   func(context.Context, *account.Credentials) (bool, error) { authCalls.Add(1); return true, nil },
	}); err != nil {
		t.Fatal(err)
	}
	a.Engine.SetAccountManager(m)
	p, err := account.NewPools(context.Background(), m, account.NewMemoryPoolStore(), account.PoolOptions{
		ScanInterval: time.Hour,
		References:   func(context.Context, account.PoolKey) (int64, error) { return 0, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	a.runtimeState.pools = p
	post := func(body string, status int) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		a.accountPoolsAPI(w, httptest.NewRequest("POST", "/api/account-pools", strings.NewReader(body)))
		if w.Code != status {
			t.Fatalf("status %d want %d: %s", w.Code, status, w.Body.String())
		}
		return w
	}
	save := func(revision uint64) string {
		return fmt.Sprintf(`{"action":"save","revision":%d,"config":{"key":{"site":"fixture","name":"primary"},"members":["first"],"disabled":true}}`, revision)
	}
	post(save(0), 200)
	key := account.PoolKey{Site: "fixture", Name: "primary"}
	before, err := p.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		w := httptest.NewRecorder()
		a.accountPoolsAPI(w, httptest.NewRequest("GET", "/api/account-pools", nil))
		if w.Code != 200 {
			t.Fatal(w.Body.String())
		}
	}
	after, _ := p.Get(key)
	if authCalls.Load() != 0 || after.Revision != before.Revision {
		t.Fatal("read performed authentication or mutation")
	}
	// A stale edit cannot replace the current configuration.
	post(save(0), 409)
	// Even a disabled pool cannot be deleted while referenced by policy.
	policy := a.runtimeState.effective.Sites["fixture"]
	poolName := "primary"
	policy.Pool = &poolName
	a.runtimeState.effective.Sites["fixture"] = policy
	del := fmt.Sprintf(`{"action":"delete","revision":%d,"key":{"site":"fixture","name":"primary"}}`, before.Revision)
	post(del, 409)
	policy.Pool = nil
	a.runtimeState.effective.Sites["fixture"] = policy
	post(del, 200)
	w := post(save(0), 409)
	var conflict struct {
		Deleted account.PoolSnapshot `json:"deleted"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &conflict); err != nil {
		t.Fatal(err)
	}
	if !conflict.Deleted.Deleted || conflict.Deleted.Revision <= before.Revision {
		t.Fatal("missing tombstone version")
	}
	// Re-creation requires an explicit write against that deletion revision.
	post(save(conflict.Deleted.Revision), 200)
	recreated, err := p.Get(key)
	if err != nil || recreated.Deleted || recreated.Revision <= conflict.Deleted.Revision {
		t.Fatal("explicit recreation failed", err)
	}
}
