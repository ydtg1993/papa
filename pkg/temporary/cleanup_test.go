package temporary

import (
	"context"
	"errors"
	"os"
	"testing"
)

type cleanupStore struct {
	pending, done, failBegin, failFinish bool
	row                                  CleanupRequest
	calls                                []string
}

func (s *cleanupStore) BeginCleanup(_ context.Context, id, actor string) (bool, error) {
	s.calls = append(s.calls, "begin")
	if s.failBegin {
		return false, errors.New("intent failed")
	}
	if s.done {
		return true, nil
	}
	s.pending = true
	s.row = CleanupRequest{id, actor}
	return false, nil
}
func (s *cleanupStore) FinishCleanup(context.Context, string) error {
	s.calls = append(s.calls, "finish")
	if s.failFinish {
		return errors.New("commit failed")
	}
	s.done = true
	s.pending = false
	return nil
}
func (s *cleanupStore) PendingCleanup(context.Context) ([]CleanupRequest, error) {
	if s.pending {
		return []CleanupRequest{s.row}, nil
	}
	return nil, nil
}
func TestCleanupRetainsIntentUntilCommitAndRecovers(t *testing.T) {
	f, id, file := makeFiles(t)
	path, e := f.FilePath(id, file)
	if e != nil {
		t.Fatal(e)
	}
	store := &cleanupStore{failFinish: true}
	c := Cleaner{Files: f, Store: store}
	if e = c.Cleanup(context.Background(), id, "reviewer"); e == nil {
		t.Fatal("failed commit ignored")
	}
	if !store.pending || store.done {
		t.Fatal("recovery intent lost")
	}
	if _, e = os.Stat(path); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("files not removed", e)
	}
	store.failFinish = false
	if e = c.Recover(context.Background()); e != nil || !store.done || store.pending {
		t.Fatal("recovery failed", e)
	}
	before := len(store.calls)
	if e = c.Recover(context.Background()); e != nil || len(store.calls) != before {
		t.Fatal("completed recovery repeated", e)
	}
}
func TestCleanupRequiresIntentBeforeFileDeletion(t *testing.T) {
	f, id, file := makeFiles(t)
	path, _ := f.FilePath(id, file)
	store := &cleanupStore{failBegin: true}
	c := Cleaner{Files: f, Store: store}
	if e := c.Cleanup(context.Background(), id, "reviewer"); e == nil {
		t.Fatal("intent failure ignored")
	}
	if _, e := os.Stat(path); e != nil {
		t.Fatal("deleted without durable intent")
	}
	store.failBegin = false
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if e := c.Cleanup(ctx, id, "reviewer"); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if e := c.Cleanup(context.Background(), id, ""); !errors.Is(e, ErrInvalidCleanup) {
		t.Fatal(e)
	}
	if _, e := os.Stat(path); e != nil {
		t.Fatal("cancelled operation deleted files")
	}
	// A missing/invalid root is not success: completion must stay pending.
	c.Files.Namespace = "../outside"
	if e := c.Cleanup(context.Background(), id, "reviewer"); e == nil || store.done || !store.pending {
		t.Fatal("file failure committed cleanup", e)
	}
}
