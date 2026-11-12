package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ydtg1993/papa/v4/internal/breaker"
	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/obs/incident"
	"github.com/ydtg1993/papa/v4/pkg/redact"
	"gorm.io/gorm"
)

// The existing recovery snapshot is the journal. Save the result before writing
// the terminal row; a lost SQL acknowledgement can then be confirmed after restart.
func (e *Engine) retainBreakerFailure(ctx context.Context, task *Task, version taskRunVersion, cause error) error {
	e.breakerMu.Lock()
	s := e.smartBreakers[task.Site]
	if s == nil || s.event == nil {
		e.breakerMu.Unlock()
		return errors.New("serial failure has no recovery state")
	}
	if p := s.event.PendingFailure; p != nil {
		if p.TaskID != task.ID || p.Repeat != version.Repeat || p.Reprocess != version.Reprocess {
			e.breakerMu.Unlock()
			return errors.New("another serial failure is awaiting settlement")
		}
	} else {
		f := incident.ClassifyError(cause)
		kind, policy := failureCompensation(cause)
		s.event.PendingFailure = &breaker.PendingFailure{TaskID: task.ID, Stage: task.Stage, URL: task.URL,
			Repeat: version.Repeat, Reprocess: version.Reprocess, At: time.Now(), SampleDeadline: task.breakerSampleDeadline,
			Error: redact.Text(cause.Error()), Category: f.Category, Fingerprint: f.Fingerprint,
			FailureKind: kind, CompensationMode: string(policy.Mode), CompensationReason: policy.Reason, NotBefore: policy.NotBefore}
		for i := range s.event.Checks {
			if s.event.Checks[i].Stage == task.Stage {
				s.event.Checks[i].State = "failed"
				s.event.Checks[i].Detail = "真实失败待落库确认；不能继续验证或当作健康证明：" + redact.Text(cause.Error())
			}
		}
		e.noteBreakerLocked(s, "failure_pending", task.Stage, fmt.Sprintf("任务 %d 轮次 %d:%d 真实失败待落库确认；阻止后续验证：%s", task.ID, version.Repeat, version.Reprocess, cause), time.Now())
		s.controlRevision = s.event.Revision
	}
	e.breakerMu.Unlock()
	e.rememberUnsettled(task)
	return e.flushBreakerReports(ctx)
}

// Queue resets must not replace a round whose result is still being settled,
// including the interval between counting it and saving the checkpoint.
func (e *Engine) breakerSettlementBlocked(site string) bool {
	e.breakerMu.Lock()
	defer e.breakerMu.Unlock()
	s := e.smartBreakers[site]
	return s != nil && s.event != nil && (s.event.PendingFailure != nil || s.savedRevision < s.controlRevision)
}

// Called by the existing report writer. sampleDispatching owns SQL outside the
// lock, and inflight prevents overlapping the original handler's cleanup.
func (e *Engine) settleBreakerFailures(ctx context.Context) {
	e.breakerMu.Lock()
	var sites []string
	for site := range e.smartBreakers {
		sites = append(sites, site)
	}
	e.breakerMu.Unlock()
	for _, site := range sites {
		if ctx.Err() != nil {
			return
		}
		e.breakerMu.Lock()
		s := e.smartBreakers[site]
		if !s.cfg.Enabled || s.event == nil || s.event.PendingFailure == nil || s.inflight != 0 || s.sampleDispatching || s.savedRevision < s.controlRevision || time.Since(s.event.PendingFailure.LastAttempt) < time.Minute {
			e.breakerMu.Unlock()
			continue
		}
		p := *s.event.PendingFailure
		s.sampleDispatching = true
		s.event.PendingFailure.Attempts++
		s.event.PendingFailure.LastAttempt = time.Now()
		e.noteBreakerLocked(s, "failure_settlement_retry", p.Stage, fmt.Sprintf("第 %d 次结算任务 %d 轮次 %d:%d；只重试落库，不重跑业务", s.event.PendingFailure.Attempts, p.TaskID, p.Repeat, p.Reprocess), time.Now())
		s.controlRevision = s.event.Revision
		task := &Task{ID: p.TaskID, Site: site, Stage: p.Stage, URL: p.URL, breakerEpoch: s.epoch, breakerRound: fmt.Sprintf("%d:%d", p.Repeat, p.Reprocess), breakerSampleDeadline: p.SampleDeadline}
		e.breakerMu.Unlock()

		err := e.flushBreakerReports(ctx)
		if err == nil {
			var row models.CrawlerTask
			err = e.db.WithContext(ctx).First(&row, p.TaskID).Error
			if err == nil {
				// Preserve identity/flow/repeat bookkeeping when releasing the
				// original task. The terminal CAS still fences the saved round.
				restored := e.taskFromRecord(&row)
				restored.breakerEpoch, restored.breakerRound, restored.breakerSampleDeadline = task.breakerEpoch, task.breakerRound, p.SampleDeadline
				task = restored
			}
		}
		if err == nil {
			updates := map[string]any{"status": models.TaskStatusFailed, "error": gorm.Expr("CONCAT(COALESCE(error, ''), ?)", p.Error+"\n"),
				"compensation_version": gorm.Expr("compensation_version + 1"), "failure_kind": p.FailureKind,
				"compensation_mode": p.CompensationMode, "compensation_reason": p.CompensationReason, "next_compensation_at": taskTimePointer(p.NotBefore),
				"last_finished_at": p.At.UTC()}
			err = e.persistTerminalUpdates(ctx, task, taskRunVersion{p.Repeat, p.Reprocess}, models.TaskStatusFailed, updates)
		}
		if err == nil {
			e.recordSmartOutcome(task, errors.New(p.Error))
			e.breakerMu.Lock()
			if s.event.SampleDeadline.Equal(p.SampleDeadline) {
				e.noteBreakerLocked(s, "sample_exited", p.Stage, "原样本已退出且失败结算完成；不以结算代替健康证明", time.Now())
				s.event.SampleID, s.event.SampleStage, s.event.SampleDeadline = "", "", time.Time{}
				s.controlRevision = s.event.Revision
			}
			e.breakerMu.Unlock()
			e.emitTaskFailed(task, errors.New(p.Error), time.Time{})
			e.finishTask(task)
		} else {
			e.breakerMu.Lock()
			e.noteBreakerLocked(s, "failure_settlement_wait", p.Stage, "失败落库仍未确认，继续阻止验证；一分钟后重试："+err.Error(), time.Now())
			s.controlRevision = s.event.Revision
			e.breakerMu.Unlock()
		}
		e.breakerMu.Lock()
		s.sampleDispatching = false
		e.breakerMu.Unlock()
	}
}
