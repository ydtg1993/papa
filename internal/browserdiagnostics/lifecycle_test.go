package browserdiagnostics

import (
	"context"
	"errors"
	"github.com/ydtg1993/papa/v4/pkg/browser/identity"
	identityruntime "github.com/ydtg1993/papa/v4/pkg/browser/identity/runtime"
	"testing"
	"time"
)

func TestServiceShutdownCancelsAndDrainsBothDiagnosticEntrypoints(t *testing.T) {
	s := &Service{}
	ctx, done, ok := s.Begin(context.Background())
	if !ok {
		t.Fatal("admission rejected")
	}
	deadline, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if s.Shutdown(deadline) {
		t.Fatal("active operation reported drained")
	}
	if ctx.Err() == nil {
		t.Fatal("active diagnostic not canceled")
	}
	if _, err := s.Rod(context.Background()); !errors.Is(err, ErrClosing) {
		t.Fatalf("Rod after shutdown: %v", err)
	}
	if _, err := s.Diagnose(context.Background(), identity.Reference{}, identityruntime.Subject{}); !errors.Is(err, ErrClosing) {
		t.Fatalf("identity after shutdown: %v", err)
	}
	done()
	if !s.Shutdown(context.Background()) {
		t.Fatal("completed operation not drained")
	}
}

func TestServiceSharesRodAndIdentityAdmissionWithoutGlobalState(t *testing.T) {
	s := &Service{}
	s.diagnostic.Lock()
	defer s.diagnostic.Unlock()
	if _, err := s.Rod(context.Background()); !errors.Is(err, ErrBusy) {
		t.Fatalf("Rod admission: %v", err)
	}
	if _, err := s.Diagnose(context.Background(), identity.Reference{}, identityruntime.Subject{}); !errors.Is(err, ErrBusy) {
		t.Fatalf("identity admission: %v", err)
	}
	other := &Service{}
	report, err := other.Rod(context.Background())
	if err != nil || len(report.Steps) == 0 {
		t.Fatalf("independent component blocked: %v", err)
	}
	if !s.Shutdown(context.Background()) {
		t.Fatal("rejected admissions leaked pending work")
	}
}

type blockingViewStore struct {
	identityruntime.Store
	entered chan context.Context
	release chan struct{}
}

func (s *blockingViewStore) ListProfiles(ctx context.Context, _ string) ([]identity.Profile, error) {
	s.entered <- ctx
	<-s.release // Model a storage driver that cannot return immediately on cancellation.
	return nil, ctx.Err()
}

func TestServiceViewCancelsAndDrainsWithoutHTTP(t *testing.T) {
	store := &blockingViewStore{
		Store:   identityruntime.NewMemoryStore(),
		entered: make(chan context.Context, 1),
		release: make(chan struct{}),
	}
	s := &Service{}
	if err := s.Initialize(store); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { _, err := s.View(context.Background(), "fixture"); result <- err }()
	ctx := <-store.entered
	deadline, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	drained := s.Shutdown(deadline)
	close(store.release)
	if drained {
		t.Error("shutdown completed while view still used storage")
	}
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Errorf("view storage context was not canceled: %v", err)
	}
	if ctx.Err() == nil {
		t.Error("view did not receive service cancellation")
	}
	if !s.Shutdown(context.Background()) {
		t.Fatal("completed view not drained")
	}
	if _, err := s.View(context.Background(), "fixture"); !errors.Is(err, ErrClosing) {
		t.Fatalf("view admitted after shutdown: %v", err)
	}
}
