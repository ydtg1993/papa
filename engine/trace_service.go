package engine

import (
	"context"
	"fmt"
	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/internal/tracestore"
	"sync"
	"time"
)

// traceService owns the persistent file store and bounded one-shot selections.
// Engine only assembles it; Trace owns the per-attempt buffer.
type traceService struct {
	files  *tracestore.Store
	logger *logrus.Logger
	mu     sync.Mutex
	armed  map[int]time.Time
}

const maxTraceSelections = 1024
const traceSelectionTTL = time.Hour

type traceSelectionKey struct{}

// TraceNextExecution retains the next successfully claimed execution, including
// all retries. Requests expire after one hour. It does not execute the task.
func (e *Engine) TraceNextExecution(taskID int) error {
	if !e.traceEnabled() {
		return fmt.Errorf("步骤追踪未开启（crawler.trace.enabled）")
	}
	if taskID <= 0 {
		return fmt.Errorf("trace task ID must be positive")
	}
	s := e.traceService()
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	s.prune(now)
	if _, exists := s.armed[taskID]; !exists && len(s.armed) >= maxTraceSelections {
		return fmt.Errorf("trace selection capacity reached")
	}
	s.armed[taskID] = now.Add(traceSelectionTTL)
	return nil
}
func (s *traceService) prune(now time.Time) {
	for id, until := range s.armed {
		if !until.After(now) {
			delete(s.armed, id)
		}
	}
}
func (e *Engine) selectTraceExecution(ctx context.Context, taskID int) context.Context {
	selected := false
	if e.traceEnabled() {
		s := e.traceService()
		s.mu.Lock()
		s.prune(time.Now())
		_, selected = s.armed[taskID]
		delete(s.armed, taskID)
		s.mu.Unlock()
	}
	return context.WithValue(ctx, traceSelectionKey{}, selected)
}
func (s *traceService) write(t *Trace, steps []core.TraceStep) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.files.Write(ctx, tracestore.Batch{TaskID: t.taskID, IdentityHash: t.identityHash, ExecutionID: t.executionID, Attempt: t.attempt, RecordedAt: time.Now(), Truncated: t.truncated, Steps: steps}); err != nil && s.logger != nil {
		s.logger.Warnf("trace: write task %d attempt %d failed: %v", t.taskID, t.attempt, err)
	}
}
func (s *traceService) cleanup(ctx context.Context, retention time.Duration) {
	if ctx == nil {
		ctx = context.Background()
	}
	if retention <= 0 {
		return
	}
	if err := s.files.Cleanup(ctx, time.Now().Add(-retention)); err != nil && s.logger != nil {
		s.logger.Warnf("trace cleanup: %v", err)
	}
}
