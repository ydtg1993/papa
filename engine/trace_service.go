package engine

import (
	"context"
	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/internal/tracestore"
	"time"
)

// traceService owns file persistence; Trace owns each bounded attempt buffer.
type traceService struct {
	files  *tracestore.Store
	logger *logrus.Logger
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
