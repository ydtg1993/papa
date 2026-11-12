package engine

import (
	"context"
	"fmt"
	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/gorm"
	"time"
)

// traceService owns storage and retention policy; Trace owns attempt state.
// Options are a value snapshot, refreshed by the facade for each operation.
type traceService struct {
	options          config.TraceConfig
	db               *gorm.DB
	dbLogger, logger *logrus.Logger
}

func (s traceService) retention() time.Duration {
	if s.options.Retention < 0 {
		return 0
	}
	if s.options.Retention == 0 {
		return defaultTraceRetention
	}
	return s.options.Retention
}
func (s traceService) newTrace(taskID, attempt int) *Trace {
	if !s.options.Enabled || taskID == 0 {
		return nil
	}
	return newTrace(s.db, s.dbLogger, taskID, attempt)
}
func (s traceService) write(records []models.TaskTrace, taskID, attempt int) {
	if err := s.db.Create(&records).Error; err != nil {
		s.dbLogger.Warnf("trace: write %d steps of task %d attempt %d failed: %s", len(records), taskID, attempt, err.Error())
	}
}
func (s traceService) list(taskID int) ([]TraceStep, error) {
	if !s.options.Enabled {
		return nil, fmt.Errorf("步骤追踪未开启（crawler.trace.enabled）")
	}
	var rows []models.TaskTrace
	err := s.db.Where("task_id = ?", taskID).
		Order("attempt, seq").
		Limit(maxTraceSteps).
		Find(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("load trace of task %d: %w", taskID, err)
	}
	out := make([]TraceStep, 0, len(rows))
	for _, r := range rows {
		status := "ok"
		switch r.Status {
		case models.TraceFailed:
			status = "failed"
		case models.TraceWarn:
			status = "warn"
		}
		out = append(out, TraceStep{
			Attempt:   r.Attempt,
			Seq:       r.Seq,
			Step:      r.Step,
			Status:    status,
			Kind:      r.Kind,
			Message:   r.Message,
			Data:      string(r.Data),
			Duration:  r.Duration,
			CreatedAt: r.CreatedAt,
		})
	}
	return out, nil
}

func (s traceService) cleanup(ctx context.Context, retention time.Duration) {
	cutoff := time.Now().Add(-retention)
	var total int64
	for {
		select {
		case <-ctx.Done():
			s.dbLogger.Infof("trace cleanup: interrupted after deleting %d rows", total)
			return
		default:
		}
		res := s.db.Where("created_at < ?", cutoff).Limit(traceDeleteBatch).Delete(&models.TaskTrace{})
		if res.Error != nil {
			s.dbLogger.Errorf("trace cleanup: %s", res.Error.Error())
			return
		}
		total += res.RowsAffected
		if res.RowsAffected < traceDeleteBatch {
			break
		}
	}
	if total > 0 {
		s.logger.Infof("trace cleanup: deleted %d rows older than %s", total, cutoff.Format(time.RFC3339))
	}
}
