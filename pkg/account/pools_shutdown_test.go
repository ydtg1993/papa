package account

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

type shutdownBlockingCandidateStore struct {
	entered chan struct{}
	release chan struct{}
}

func (s *shutdownBlockingCandidateStore) Load(context.Context, Key) (*StoredState, error) {
	return nil, nil
}
func (s *shutdownBlockingCandidateStore) Save(context.Context, Key, *StoredState) error {
	close(s.entered)
	<-s.release
	return nil
}
func (s *shutdownBlockingCandidateStore) Delete(context.Context, Key) error { return nil }

func TestPoolsShutdownDeadlineIncludesCoordinatorDetach(t *testing.T) {
	m := NewManager(htmlfetch.NewClient(htmlfetch.Config{}))
	p, err := NewPools(context.Background(), m, NewMemoryPoolStore(), PoolOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	store := &shutdownBlockingCandidateStore{entered: make(chan struct{}), release: make(chan struct{})}
	registered := make(chan error, 1)
	go func() {
		registered <- m.RegisterCandidate(context.Background(), Key{Site: "fixture", ID: "candidate"}, Config{
			Origins: []string{"https://fixture.invalid"}, Store: store,
			Login: func(context.Context, *Credentials) error { return nil },
			Check: func(context.Context, *Credentials) (bool, error) { return true, nil },
		}, candidateToken("fixture"))
	}()
	t.Cleanup(func() { close(store.release); <-registered })
	select {
	case <-store.entered:
	case <-time.After(time.Second):
		t.Fatal("candidate save did not begin")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- p.Shutdown(ctx) }()
	select {
	case err := <-result:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("coordinator detach reported drained while candidate transaction held manager: %v", err)
		}
	case <-time.After(300 * time.Millisecond):
		t.Fatal("shutdown waited for the candidate transaction lock beyond deadline")
	}
	store.release <- struct{}{}
	drainCtx, finish := context.WithTimeout(context.Background(), time.Second)
	defer finish()
	if err := p.Shutdown(drainCtx); err != nil {
		t.Fatalf("released candidate did not allow coordinator detach: %v", err)
	}
	if m.Pools() != nil {
		t.Fatal("drained coordinator was not detached")
	}
}
