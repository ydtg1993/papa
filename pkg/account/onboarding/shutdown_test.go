package onboarding

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/account"
)

type shutdownBlockingProfiles struct {
	Store
	operation string
	entered   chan context.Context
	release   chan struct{}
}

func (s *shutdownBlockingProfiles) block(ctx context.Context, operation string) {
	if s.operation == operation {
		s.entered <- ctx
		<-s.release
	}
}
func (s *shutdownBlockingProfiles) Get(ctx context.Context, key account.Key) (Profile, error) {
	s.block(ctx, "get")
	return s.Store.Get(ctx, key)
}
func (s *shutdownBlockingProfiles) SaveCAS(ctx context.Context, p Profile, expected uint64) error {
	s.block(ctx, "save")
	return s.Store.SaveCAS(ctx, p, expected)
}
func (s *shutdownBlockingProfiles) List(ctx context.Context) ([]Profile, error) {
	s.block(ctx, "list")
	return s.Store.List(ctx)
}

func TestShutdownDeadlineIncludesProfileOperations(t *testing.T) {
	for _, operation := range []string{"get", "save", "list"} {
		t.Run(operation, func(t *testing.T) {
			profiles := &shutdownBlockingProfiles{Store: NewMemoryStore(), operation: operation, entered: make(chan context.Context, 1), release: make(chan struct{})}
			s, _, _ := newFixtureService(t, &fixtureProvider{}, profiles, time.Minute)
			done := make(chan struct{})
			go func() {
				defer close(done)
				if operation == "list" {
					_, _ = s.Profiles(context.Background())
				} else {
					_, _ = s.SaveDraft(context.Background(), Profile{Key: account.Key{Site: "fixture", ID: "draft"}, Label: "Draft"}, 0)
				}
			}()
			t.Cleanup(func() { close(profiles.release); <-done })
			var operationCtx context.Context
			select {
			case operationCtx = <-profiles.entered:
			case <-time.After(time.Second):
				t.Fatal("profile operation did not enter storage")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- s.Shutdown(ctx) }()
			select {
			case err := <-result:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("profile operation reported drained: %v", err)
				}
			case <-time.After(300 * time.Millisecond):
				t.Error("shutdown waited for the profile transaction lock beyond deadline")
				return
			}
			select {
			case <-operationCtx.Done():
			case <-time.After(time.Second):
				t.Error("profile storage operation was not canceled")
			}
			admission := make(chan error, 1)
			go func() {
				_, err := s.Start(context.Background(), "operator", account.Key{Site: "fixture", ID: "draft"}, 1, "password", nil, false)
				admission <- err
			}()
			select {
			case err := <-admission:
				if !errors.Is(err, ErrGone) {
					t.Errorf("stopped service admitted a new operation: %v", err)
				}
			case <-time.After(time.Second):
				t.Error("stopped admission still waited for the profile transaction mutex")
			}
			profiles.release <- struct{}{}
			<-done
			if err := s.Shutdown(context.Background()); err != nil {
				t.Fatalf("released profile operation did not drain: %v", err)
			}
		})
	}
}
