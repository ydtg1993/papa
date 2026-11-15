package app

import (
	"context"
	"database/sql"
	ep "github.com/ydtg1993/papa/v4/pkg/executionpolicy"
	"testing"
	"time"
)

type blockedExecutionWrite struct {
	ep.Store
	db               *sql.DB
	entered, release chan struct{}
}

func (s *blockedExecutionWrite) Load(context.Context) (ep.History, error) {
	return ep.History{Revision: 1, Document: ep.InitialDocument()}, nil
}
func (s *blockedExecutionWrite) Operation(context.Context, string) (ep.Operation, error) {
	return ep.Operation{}, ep.ErrNotFound
}
func (s *blockedExecutionWrite) Save(_ context.Context, u ep.Update, _ string) (ep.History, error) {
	close(s.entered)
	<-s.release
	return ep.History{Revision: 2, Document: u.Document}, s.db.Ping()
}

func TestShutdownRetainsDatabaseForPublicExecutionPolicyWrite(t *testing.T) {
	a, _ := runtimeTestApp(t)
	a.Config.Crawler.StopTimeout = 20 * time.Millisecond
	db, err := a.DB.DB()
	if err != nil {
		t.Fatal(err)
	}
	s := &blockedExecutionWrite{db: db, entered: make(chan struct{}), release: make(chan struct{})}
	a.executionPolicy = ep.New(s, nil, nil)
	if err := a.executionPolicy.Initialize(context.Background()); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := a.ExecutionPolicy().Update(context.Background(), ep.Update{ExpectedRevision: 1, OperationID: "public-go-write", Document: ep.InitialDocument()}, "fixture")
		done <- err
	}()
	<-s.entered
	t.Cleanup(func() {
		close(s.release)
		if err := <-done; err != nil {
			t.Error(err)
		}
		db.Close()
	})
	a.shutdownRun(false)
	if err := db.Ping(); err != nil {
		t.Fatalf("database closed before public configuration write drained: %v", err)
	}
}
