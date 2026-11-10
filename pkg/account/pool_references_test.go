package account

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

func TestPoolReferenceInsertAndDeletionShareCancellableGate(t *testing.T) {
	m, _, _ := poolManager(t, nil)
	var refs atomic.Int64
	p, err := NewPools(context.Background(), m, NewMemoryPoolStore(), PoolOptions{ScanInterval: time.Millisecond, References: func(context.Context, PoolKey) (int64, error) { return refs.Load(), nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	key := createPool(t, p, "p")
	snapshot, _ := p.Get(key)
	entered, release := make(chan struct{}), make(chan struct{})
	inserted := make(chan error, 1)
	go func() {
		inserted <- p.ProtectReferences(context.Background(), []PoolKey{key, key}, func() error { close(entered); <-release; refs.Add(1); return nil })
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	called := false
	if err = p.ProtectReferences(ctx, []PoolKey{key}, func() error { called = true; return nil }); !errors.Is(err, context.DeadlineExceeded) || called {
		t.Fatal("reference gate did not cancel", err)
	}
	disabled := make(chan error, 1)
	cfg := snapshot.PoolConfig
	cfg.Disabled = true
	go func() { _, err := p.Upsert(context.Background(), cfg, snapshot.Revision); disabled <- err }()
	close(release)
	if err = <-inserted; err != nil {
		t.Fatal(err)
	}
	if err = <-disabled; err != nil {
		t.Fatal(err)
	}
	snapshot, _ = p.Get(key)
	if err = p.Delete(context.Background(), key, snapshot.Revision); !errors.Is(err, ErrPoolReferenced) {
		t.Fatal("delete missed concurrent committed reference", err)
	}
}

func TestPoolDeleteRejectsLateReferenceAndAllowsVersionedRecreation(t *testing.T) {
	m, _, _ := poolManager(t, nil)
	counted, release := make(chan struct{}), make(chan struct{})
	p, err := NewPools(context.Background(), m, NewMemoryPoolStore(), PoolOptions{References: func(context.Context, PoolKey) (int64, error) { close(counted); <-release; return 0, nil }})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	key := PoolKey{Site: "s", Name: "p"}
	snapshot, err := p.Upsert(context.Background(), PoolConfig{Key: key, Disabled: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	deleted := make(chan error, 1)
	go func() { deleted <- p.Delete(context.Background(), key, snapshot.Revision) }()
	<-counted
	var writes atomic.Int32
	inserted := make(chan error, 1)
	go func() {
		inserted <- p.ProtectReferences(context.Background(), []PoolKey{key}, func() error { writes.Add(1); return nil })
	}()
	close(release)
	if err = <-deleted; err != nil {
		t.Fatal(err)
	}
	if err = <-inserted; !errors.Is(err, ErrPoolUnknown) || writes.Load() != 0 {
		t.Fatal("late reference entered deleted pool", err)
	}
	if _, err = p.Get(key); !errors.Is(err, ErrPoolUnknown) {
		t.Fatal("deleted pool remained active")
	}
	tombstone, err := p.GetIncludingDeleted(key)
	if err != nil || !tombstone.Deleted {
		t.Fatal("cannot discover tombstone revision", err)
	}
	if _, err = p.Upsert(context.Background(), PoolConfig{Key: key}, 0); !errors.Is(err, ErrPoolConflict) {
		t.Fatal("stale create overwrote tombstone")
	}
	recreated, err := p.Upsert(context.Background(), PoolConfig{Key: key}, tombstone.Revision)
	if err != nil || recreated.Revision <= tombstone.Revision {
		t.Fatal("deliberate recreation failed", err)
	}
}
