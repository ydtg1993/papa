package engine

import (
	"context"
	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/internal/tracestore"
	"github.com/ydtg1993/papa/v3/models"
	"time"
)

// traceService owns file persistence and cleanup; Trace owns its attempt buffer.
type traceService struct {
	files  *tracestore.Store
	logger *logrus.Logger
}

func (e *Engine) traceService() *traceService {
	e.traceOnce.Do(func() {
		o := e.cfg.Crawler.TraceFiles
		if o.Dir == "" {
			o.Dir = "traces"
		}
		e.traces = &traceService{files: tracestore.New(tracestore.Options{Dir: o.Dir, MaxFileBytes: o.MaxFileBytes, MaxTotalBytes: o.MaxTotalBytes, MaxFiles: o.MaxFiles})}
		if e.loggerSet != nil {
			e.traces.logger = e.loggerSet.Engine
		}
	})
	return e.traces
}
func (s *traceService) write(t *Trace, records []models.TaskTrace) {
	steps := make([]core.TraceStep, 0, len(records))
	for _, r := range records {
		status := "ok"
		if r.Status == models.TraceFailed {
			status = "failed"
		}
		if r.Status == models.TraceWarn {
			status = "warn"
		}
		steps = append(steps, core.TraceStep{Attempt: r.Attempt, Seq: r.Seq, Step: r.Step, Status: status, Kind: r.Kind, Message: r.Message, Data: string(r.Data), Duration: r.Duration, CreatedAt: r.CreatedAt})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.files.Write(ctx, tracestore.Batch{TaskID: t.taskID, IdentityHash: t.identityHash, ExecutionID: t.executionID, Attempt: t.attempt, RecordedAt: time.Now(), Truncated: t.truncated, Steps: steps}); err != nil && s.logger != nil {
		s.logger.Warnf("trace write task %d: %v", t.taskID, err)
	}
}
func (s *traceService) cleanup(ctx context.Context, retention time.Duration) {
	if retention <= 0 {
		return
	}
	if err := s.files.Cleanup(ctx, time.Now().Add(-retention)); err != nil && s.logger != nil {
		s.logger.Warnf("trace cleanup: %v", err)
	}
}
