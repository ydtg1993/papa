package execution

import (
	"context"
	"errors"
	"fmt"
	"github.com/ydtg1993/papa/v4/internal/taskstore"
	"gorm.io/gorm"
	"sync"
	"time"
)

type Logger interface{ Errorf(string, ...any) }

// State is zero-value usable. Only confirmed writes remove unresolved work.
type State struct {
	mu         sync.Mutex
	unresolved map[int]string
}

func (s *State) Remember(id int, stage string) {
	if id == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.unresolved == nil {
		s.unresolved = make(map[int]string)
	}
	s.unresolved[id] = stage
}
func (s *State) Forget(id int) { s.mu.Lock(); defer s.mu.Unlock(); delete(s.unresolved, id) }
func (s *State) Snapshot() map[int]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[int]string, len(s.unresolved))
	for id, key := range s.unresolved {
		out[id] = key
	}
	return out
}
func (s *State) Retry(ctx context.Context, id int, stage, operation string, fn func() error, log Logger) error {
	delay := 100 * time.Millisecond
	var lastErr error
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			s.Remember(id, stage)
			return fmt.Errorf("task %d %s interrupted: %w", id, operation, errors.Join(err, lastErr))
		}
		lastErr = fn()
		if lastErr == nil {
			return nil
		}
		if errors.Is(lastErr, gorm.ErrRecordNotFound) || errors.Is(lastErr, taskstore.ErrChanged) {
			return fmt.Errorf("task %d %s: %w", id, operation, lastErr)
		}
		if attempt == 0 || attempt%10 == 0 {
			log.Errorf("task %d %s failed; retaining worker and retrying: %s", id, operation, lastErr)
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
		case <-timer.C:
		}
		delay = min(delay*2, 2*time.Second)
	}
}
