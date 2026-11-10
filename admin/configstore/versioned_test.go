package configstore

import (
	"context"
	"database/sql/driver"
	"errors"
	"github.com/ydtg1993/papa/v4/internal/gormfake"
	"strings"
	"testing"
)

func TestRuntimePolicyRequiresCAS(t *testing.T) {
	f := gormfake.New()
	s := New(gormfake.MustOpen(f))
	if !errors.Is(s.Save(KeyRuntimePolicy, map[string]any{}, "operator"), ErrCASRequired) {
		t.Fatal("blind overwrite accepted")
	}
	if f.Written() != "" {
		t.Fatal("protected write touched DB")
	}
}
func TestRuntimePolicyCASCreationAndConditionalUpdate(t *testing.T) {
	f := gormfake.New()
	f.EnableTransactionStatements()
	s := New(gormfake.MustOpen(f))
	ctx := context.Background()
	if _, v, e := s.LoadVersioned(ctx, KeyRuntimePolicy); e != nil || v != 0 {
		t.Fatalf("absent: %d %v", v, e)
	}
	v, e := s.SaveCAS(ctx, KeyRuntimePolicy, map[string]any{"schema_version": 1}, 0, "alice")
	if e != nil || v != 1 || strings.Contains(f.Written(), "ON DUPLICATE") {
		t.Fatalf("exclusive creation: %d %v %s", v, e, f.Written())
	}
	f.SetAffected(0)
	if _, e = s.SaveCAS(ctx, KeyRuntimePolicy, map[string]any{}, 1, "bob"); !errors.Is(e, ErrConflict) {
		t.Fatalf("stale revision: %v", e)
	}
	if !strings.Contains(f.Written(), "cfg_key = ? AND revision = ?") {
		t.Fatal(f.Written())
	}
	f.SetAffected(1)
	v, e = s.SaveCAS(ctx, KeyRuntimePolicy, map[string]any{"schema_version": 1}, 2, "alice")
	if e != nil || v != 3 {
		t.Fatalf("reset preserves version: %d %v", v, e)
	}
}
func TestRuntimePolicyVersionedReadAndFailures(t *testing.T) {
	f := gormfake.New()
	f.EnableTransactionStatements()
	s := New(gormfake.MustOpen(f))
	ctx := context.Background()
	f.SetRow(map[string]driver.Value{"cfg_val": []byte(`{"schema_version":1}`), "revision": int64(9)})
	if raw, v, e := s.LoadVersioned(ctx, KeyRuntimePolicy); e != nil || v != 9 || len(raw) == 0 {
		t.Fatalf("%d %v", v, e)
	}
	f.SetExecError("write failed")
	if _, e := s.SaveCAS(ctx, KeyRuntimePolicy, map[string]any{}, 9, "alice"); e == nil || errors.Is(e, ErrConflict) {
		t.Fatalf("lost DB error: %v", e)
	}
	f.SetQueryError("crawler_config", "read failed")
	if _, _, e := s.LoadVersioned(ctx, KeyRuntimePolicy); e == nil {
		t.Fatal("read failed open")
	}
	if _, e := s.SaveCAS(ctx, KeyRuntimePolicy, map[string]any{"revision": 1}, 9, "alice"); e == nil {
		t.Fatal("duplicate version allowed")
	}
}
