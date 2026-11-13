package account

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

func controlAction(name string, p *Pools, key Key, policy Policy, expected uint64) error {
	switch name {
	case "policy":
		policy.DailyLimit = 200
		return p.SetPolicy(context.Background(), key, policy, expected)
	case "disable":
		return p.SetDisabled(context.Background(), key, true, expected)
	case "check":
		return p.CheckExisting(context.Background(), key, expected)
	case "recover":
		return p.Verify(context.Background(), key, expected)
	default:
		panic("unknown test action")
	}
}

func TestUsageReservationsDoNotInvalidateManagementSnapshot(t *testing.T) {
	for _, action := range []string{"policy", "disable", "check", "recover"} {
		t.Run(action, func(t *testing.T) {
			m, srv, _ := poolManager(t, nil, "a")
			p := testPools(t, m, NewMemoryPoolStore())
			pool := createPool(t, p, "one", "a")
			attempt := acquirePool(t, p, pool)
			policy := DefaultPolicy()
			policy.DailyLimit = 100
			policy.TrackUsage = true
			setHealthPolicy(t, p, attempt.Account, policy)
			_, snapshotVersion, err := p.Policy(attempt.Account)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 8; i++ {
				if _, err = m.DoPool(WithAttempt(context.Background(), attempt), pool, htmlfetch.Request{URL: srv.URL}); err != nil {
					t.Fatal(err)
				}
			}
			before := managementFor(t, p, pool, attempt.Account)
			if before.Version <= snapshotVersion || before.LastControlVersion != snapshotVersion || before.Usage.Operations != 8 {
				t.Fatalf("usage changed control boundary: snapshot=%d state=%+v", snapshotVersion, before)
			}
			if err = controlAction(action, p, attempt.Account, policy, snapshotVersion); err != nil {
				t.Fatalf("usage-only writes rejected stale OA snapshot: %v", err)
			}
			after := managementFor(t, p, pool, attempt.Account)
			if after.LastControlVersion <= before.Version {
				t.Fatalf("control action failed to advance boundary: %+v", after)
			}
			if err = p.SetPolicy(context.Background(), attempt.Account, policy, snapshotVersion); !errors.Is(err, ErrPoolConflict) {
				t.Fatalf("old control snapshot accepted twice: %v", err)
			}
		})
	}
}

func TestRealControlChangesRejectOldSnapshotDespiteLaterUsage(t *testing.T) {
	for _, change := range []string{"policy", "health", "disable"} {
		t.Run(change, func(t *testing.T) {
			m, _, _ := poolManager(t, nil, "a")
			p := testPools(t, m, NewMemoryPoolStore())
			pool := createPool(t, p, "one", "a")
			attempt := acquirePool(t, p, pool)
			policy := DefaultPolicy()
			policy.TrackUsage = true
			policy.Buffer = time.Hour
			setHealthPolicy(t, p, attempt.Account, policy)
			_, snapshotVersion, _ := p.Policy(attempt.Account)
			if err := p.consume(context.Background(), attempt.Account, false); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "policy":
				policy.DailyLimit = 100
				if err := p.SetPolicy(context.Background(), attempt.Account, policy, snapshotVersion); err != nil {
					t.Fatal(err)
				}
				if err := p.consume(context.Background(), attempt.Account, false); err != nil {
					t.Fatal(err)
				}
			case "health":
				if !p.ReportFailure(attempt, NeedsVerify, 0) {
					t.Fatal("health change not applied")
				}
			case "disable":
				if err := p.SetDisabled(context.Background(), attempt.Account, true, snapshotVersion); err != nil {
					t.Fatal(err)
				}
			}
			for _, action := range []string{"policy", "disable", "check", "recover"} {
				if err := controlAction(action, p, attempt.Account, policy, snapshotVersion); !errors.Is(err, ErrPoolConflict) {
					t.Fatalf("%s accepted snapshot predating %s: %v", action, change, err)
				}
			}
			// A concurrent pure event can legitimately consume current+1 between
			// the read and write, so use an actually unreachable future version.
			if err := p.SetPolicy(context.Background(), attempt.Account, policy, ^uint64(0)); !errors.Is(err, ErrPoolConflict) {
				t.Fatalf("future version accepted: %v", err)
			}
		})
	}
}

func TestConcurrentControlsAfterUsageStillHaveSingleWinner(t *testing.T) {
	m, _, _ := poolManager(t, nil, "a")
	p := testPools(t, m, NewMemoryPoolStore())
	pool := createPool(t, p, "one", "a")
	attempt := acquirePool(t, p, pool)
	policy := DefaultPolicy()
	policy.TrackUsage = true
	setHealthPolicy(t, p, attempt.Account, policy)
	_, snapshot, _ := p.Policy(attempt.Account)
	if err := p.consume(context.Background(), attempt.Account, false); err != nil {
		t.Fatal(err)
	}
	var winners, conflicts atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := p.SetPolicy(context.Background(), attempt.Account, policy, snapshot)
			if err == nil {
				winners.Add(1)
			} else if errors.Is(err, ErrPoolConflict) {
				conflicts.Add(1)
			} else {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if winners.Load() != 1 || conflicts.Load() != 15 {
		t.Fatalf("control conflict weakened: winners=%d conflicts=%d", winners.Load(), conflicts.Load())
	}
}

func TestManagementRejectsMissingVersionBoundaries(t *testing.T) {
	for _, state := range []ManagementState{
		{Version: 10, Generation: 10},
		{Version: 10, LastControlVersion: 10},
	} {
		m, _, _ := poolManager(t, nil, "a")
		store := NewMemoryPoolStore()
		state.Key = Key{Site: "s", ID: "a"}
		state.Policy = DefaultPolicy()
		if err := store.SaveManagementCAS(context.Background(), state, 0); err != nil {
			t.Fatal(err)
		}
		p, err := NewPools(context.Background(), m, store, PoolOptions{})
		if err == nil {
			p.Close()
			t.Fatal("accepted management state without version boundaries")
		}
	}
}

func TestManagementControlVersionPersistsAcrossRestart(t *testing.T) {
	m, _, _ := poolManager(t, nil, "a")
	key := Key{Site: "s", ID: "a"}
	store := NewMemoryPoolStore()
	policy := DefaultPolicy()
	policy.TrackUsage = true
	if err := store.SaveManagementCAS(context.Background(), ManagementState{Key: key, Version: 10, Generation: 10, LastControlVersion: 10, Policy: policy}, 0); err != nil {
		t.Fatal(err)
	}
	p, err := NewPools(context.Background(), m, store, PoolOptions{ScanInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	if err = p.consume(context.Background(), key, false); err != nil {
		p.Close()
		t.Fatal(err)
	}
	rows, err := store.LoadManagement(context.Background())
	if err != nil {
		p.Close()
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Version != 11 || rows[0].LastControlVersion != 10 {
		p.Close()
		t.Fatalf("control boundary not persisted: %+v", rows)
	}
	p.Close()
	p2 := testPools(t, m, store)
	if err = p2.SetPolicy(context.Background(), key, policy, 10); err != nil {
		t.Fatalf("restart rejected known control snapshot across usage: %v", err)
	}
	if err = p2.SetPolicy(context.Background(), key, policy, 10); !errors.Is(err, ErrPoolConflict) {
		t.Fatalf("restart lost real control conflict: %v", err)
	}
}

type blockedUsageStore struct {
	*MemoryPoolStore
	block   atomic.Bool
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockedUsageStore) SaveManagementCAS(ctx context.Context, state ManagementState, expected uint64) error {
	if s.block.Load() && state.Usage.Operations > 0 {
		s.once.Do(func() {
			close(s.entered)
			select {
			case <-s.release:
			case <-ctx.Done():
			}
		})
	}
	return s.MemoryPoolStore.SaveManagementCAS(ctx, state, expected)
}

func TestControlSnapshotSurvivesInFlightUsageReservation(t *testing.T) {
	m, _, _ := poolManager(t, nil, "a")
	store := &blockedUsageStore{MemoryPoolStore: NewMemoryPoolStore(), entered: make(chan struct{}), release: make(chan struct{})}
	p := testPools(t, m, store)
	pool := createPool(t, p, "one", "a")
	attempt := acquirePool(t, p, pool)
	policy := DefaultPolicy()
	policy.TrackUsage = true
	setHealthPolicy(t, p, attempt.Account, policy)
	_, snapshot, _ := p.Policy(attempt.Account)
	store.block.Store(true)
	usageDone := make(chan error, 1)
	go func() { usageDone <- p.consume(context.Background(), attempt.Account, false) }()
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("reservation did not reach persistence")
	}
	controlDone := make(chan error, 1)
	go func() { controlDone <- p.SetPolicy(context.Background(), attempt.Account, policy, snapshot) }()
	close(store.release)
	if err := <-usageDone; err != nil {
		t.Fatal(err)
	}
	if err := <-controlDone; err != nil {
		t.Fatalf("queued management edit conflicted with usage: %v", err)
	}
}
