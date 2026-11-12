package engine

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ydtg1993/papa/v4/models"
)

// Reuse the regular versioned task retry/claim path. This runs in the existing
// persistence loop, never under the manager/admission lock during database I/O.
func (e *Engine) retryBreakerSamples(ctx context.Context) {
	if e.db == nil {
		return
	}
	type candidate struct {
		site, id string
		trial    uint64
		taskID   int64
		stage    string
	}
	var candidates []candidate
	now := time.Now()
	e.breakerMu.Lock()
	for site, s := range e.smartBreakers {
		l := s.event
		if l == nil || !l.MixedSamples || (l.State != "observing" && l.State != "ramping") || !now.Before(l.Deadline) || s.draining || s.inflight > 0 || s.savedRevision < s.controlRevision || now.Before(s.nextSampleRetry) || e.hasManualBreakerPause(site) {
			continue
		}
		for _, check := range l.Checks {
			if check.Verification == "readonly" || check.TaskSuccesses >= check.RequiredTasks {
				continue
			}
			selected := -1
			for i, sample := range check.Samples {
				need := 3 / len(check.Samples)
				if i < 3%len(check.Samples) {
					need++
				}
				if sample.TaskID > 0 && sample.Successes < need {
					if selected < 0 || sample.Successes < check.Samples[selected].Successes {
						selected = i
					}
				}
			}
			if selected >= 0 {
				candidates = append(candidates, candidate{site, l.ID, s.trial, check.Samples[selected].TaskID, check.Stage})
			}
		}
		s.nextSampleRetry = now.Add(max(time.Second, s.policy.Interval))
	}
	e.breakerMu.Unlock()
	for _, c := range candidates {
		if ctx.Err() != nil {
			return
		}
		var record models.CrawlerTask
		err := e.db.WithContext(ctx).Where("id = ? AND site = ? AND stage = ?", c.taskID, c.site, c.stage).First(&record).Error
		if err == nil && (record.Status == models.TaskStatusPending || record.Status == models.TaskStatusProcessing) {
			continue
		}
		e.breakerMu.Lock()
		s := e.smartBreakers[c.site]
		valid := s != nil && s.event != nil && s.event.ID == c.id && s.trial == c.trial && (s.event.State == "observing" || s.event.State == "ramping") && time.Now().Before(s.event.Deadline) && !s.draining && s.inflight == 0 && !e.hasManualBreakerPause(c.site)
		e.breakerMu.Unlock()
		if !valid {
			continue
		}
		if err == nil {
			version := record.CompensationVersion
			err = e.retryTaskRecord(ctx, &record, record.Reprocess, &version)
		}
		e.breakerMu.Lock()
		if s.event.ID == c.id && s.trial == c.trial {
			if err != nil {
				// CAS races mean the original task path already moved the record.
				// Other failures back off, allowing transient storage/eligibility to recover.
				if !errors.Is(err, ErrTaskChanged) && !errors.Is(err, ErrTaskProcessing) {
					e.noteBreakerLocked(s, "sample_retry_unavailable", c.stage, fmt.Sprintf("验证样本 %d 无法重试：%v", c.taskID, err), time.Now())
					s.nextSampleRetry = time.Now().Add(max(s.policy.ProbeInterval, s.policy.Interval))
				}
			} else {
				e.noteBreakerLocked(s, "sample_retry", c.stage, fmt.Sprintf("按原有任务认领和幂等规则重试失败样本 %d", c.taskID), time.Now())
			}
		}
		e.breakerMu.Unlock()
	}
}
