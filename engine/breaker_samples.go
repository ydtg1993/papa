package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ydtg1993/papa/v4/internal/breaker"
	"github.com/ydtg1993/papa/v4/models"
)

const breakerSampleTimeout = 10 * time.Minute

func breakerSampleNeed(count, index int) int {
	need := 3 / count
	if index < 3%count {
		need++
	}
	return need
}

// One selected sample per site across all stages; A/B/A rotates two samples.
func nextBreakerSample(s *smartBreaker) (string, string) {
	for _, c := range s.event.Checks {
		if c.Verification == "readonly" || c.TaskSuccesses >= c.RequiredTasks {
			continue
		}
		selected := -1
		for i, sample := range c.Samples {
			if slices.Contains(c.TimedOutIDs, recoverySampleID(sample)) || sample.Successes >= breakerSampleNeed(len(c.Samples), i) {
				continue
			}
			if selected < 0 || sample.Successes < c.Samples[selected].Successes {
				selected = i
			}
		}
		if selected >= 0 {
			return c.Stage, recoverySampleID(c.Samples[selected])
		}
	}
	return "", ""
}

func breakerSampleTimedOut(s *smartBreaker) bool {
	for _, c := range s.event.Checks {
		if len(c.TimedOutIDs) > 0 {
			return true
		}
	}
	return false
}

func (e *Engine) selectBreakerSampleLocked(s *smartBreaker, stage, id string, now time.Time) {
	l := s.event
	l.SampleStage, l.SampleID = stage, id
	l.SampleDeadline = now.Add(breakerSampleTimeout)
	if l.Deadline.Before(l.SampleDeadline) {
		l.SampleDeadline = l.Deadline
	}
	s.nextSampleRetry = time.Time{}
	e.noteBreakerLocked(s, "sample_selected", stage, fmt.Sprintf("串行验证选中 %s；等待和执行截止 %s（最长10分钟，受整轮预算限制）", id, l.SampleDeadline.Format(time.RFC3339)), now)
	s.controlRevision = l.Revision
}

func (e *Engine) expireBreakerSampleLocked(s *smartBreaker, now time.Time) {
	l := s.event
	if l.SampleID == "" || now.Before(l.SampleDeadline) {
		return
	}
	for i := range l.Checks {
		c := &l.Checks[i]
		if c.Stage != l.SampleStage || slices.Contains(c.TimedOutIDs, l.SampleID) {
			continue
		}
		c.TimedOutIDs = append(c.TimedOutIDs, l.SampleID)
		c.State, c.FinishedAt = "failed", now
		c.Detail = fmt.Sprintf("样本 %s 等待或执行超时；本周期保持未验证，迟到成功作废", l.SampleID)
		if s.inflight > 0 {
			c.Detail += "；执行尚未退出，不能启动下一个样本"
		}
		e.noteBreakerLocked(s, "sample_timeout", c.Stage, c.Detail, now)
		s.controlRevision = l.Revision
	}
	// Neither SQL still submitting nor a handler ignoring cancellation may
	// release the site's serial slot early.
	if s.inflight == 0 && !s.sampleDispatching && l.PendingFailure == nil {
		l.SampleID, l.SampleStage, l.SampleDeadline = "", "", time.Time{}
	}
}

func (e *Engine) admitBreakerSampleLocked(s *smartBreaker, task *Task, now time.Time) bool {
	l := s.event
	if s.event.PendingFailure != nil || s.inflight != 0 || s.sampleDispatching || s.probeRunning || s.probeExecuting {
		return false
	}
	e.expireBreakerSampleLocked(s, now)
	if l.SampleID == "" {
		stage, id := nextBreakerSample(s)
		if id == "" {
			id = recoverySampleID(breaker.RecoverySample{TaskID: int64(task.ID), URL: task.URL})
			if id == "" {
				return false
			}
			eligible := false
			for _, c := range l.Checks {
				if c.Stage != task.Stage || slices.Contains(c.TimedOutIDs, id) || len(c.TimedOutIDs) >= 3 {
					continue
				}
				eligible = !l.MixedSamples || (len(c.Samples) == 0 && c.Verification != "readonly" && c.TaskSuccesses < c.RequiredTasks && !slices.Contains(c.SuccessIDs, id))
			}
			if !eligible {
				return false
			}
			stage = task.Stage
		}
		e.selectBreakerSampleLocked(s, stage, id, now)
		return false // persist selection/deadline before any task can run
	}
	if s.savedRevision < s.controlRevision || !now.Before(l.SampleDeadline) || l.SampleStage != task.Stage || l.SampleID != recoverySampleID(breaker.RecoverySample{TaskID: int64(task.ID), URL: task.URL}) {
		return false
	}
	task.breakerSampleDeadline = l.SampleDeadline
	return true
}

// Reserve under the admission lock BEFORE SQL so normal scheduling cannot
// complete the selected sample between lookup and requeue. I/O holds no lock.
func (e *Engine) retryBreakerSamples(ctx context.Context) {
	if e.db == nil {
		return
	}
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
		l, now := s.event, time.Now()
		if l == nil || l.PendingFailure != nil || !l.MixedSamples || (l.State != "observing" && l.State != "ramping") || !now.Before(l.Deadline) || s.draining || s.inflight > 0 || s.sampleDispatching || s.savedRevision < s.controlRevision || now.Before(s.nextSampleRetry) || e.hasManualBreakerPause(site) {
			e.breakerMu.Unlock()
			continue
		}
		e.expireBreakerSampleLocked(s, now)
		if l.SampleID == "" {
			stage, id := nextBreakerSample(s)
			if id != "" {
				e.selectBreakerSampleLocked(s, stage, id, now)
			}
			e.breakerMu.Unlock()
			continue
		}
		taskID, parseErr := strconv.ParseInt(strings.TrimPrefix(l.SampleID, "task:"), 10, 64)
		if parseErr != nil || taskID <= 0 || s.savedRevision < s.controlRevision {
			e.breakerMu.Unlock()
			continue
		}
		id, stage, deadline := l.ID, l.SampleStage, l.SampleDeadline
		s.sampleDispatching = true
		s.nextSampleRetry = now.Add(max(time.Second, s.policy.Interval))
		e.breakerMu.Unlock()

		var record models.CrawlerTask
		err := e.db.WithContext(ctx).Where("id = ? AND site = ? AND stage = ?", taskID, site, stage).First(&record).Error
		requeued := false
		e.breakerMu.Lock()
		valid := s.event == l && l.SampleDeadline.Equal(deadline) && time.Now().Before(deadline) && (l.State == "observing" || l.State == "ramping") && !e.hasManualBreakerPause(site)
		e.breakerMu.Unlock()
		if valid && err == nil && record.Status != models.TaskStatusPending && record.Status != models.TaskStatusProcessing {
			retryCtx, cancel := context.WithDeadline(ctx, deadline)
			version := record.CompensationVersion
			err = e.retryTaskRecord(retryCtx, &record, record.Reprocess, &version)
			cancel()
			requeued = err == nil
		}
		e.breakerMu.Lock()
		s.sampleDispatching = false
		if s.event == l && l.ID == id && l.SampleDeadline.Equal(deadline) {
			if requeued {
				e.noteBreakerLocked(s, "sample_retry", stage, fmt.Sprintf("串行重投验证样本 %d；复用原有任务认领与版本规则", taskID), time.Now())
			} else if err != nil && !errors.Is(err, ErrTaskChanged) && !errors.Is(err, ErrTaskProcessing) {
				e.noteBreakerLocked(s, "sample_retry_unavailable", stage, fmt.Sprintf("验证样本 %d 暂不能重试：%v；等待至原截止时间，不延长预算", taskID, err), time.Now())
				s.nextSampleRetry = time.Now().Add(max(s.policy.ProbeInterval, s.policy.Interval))
			}
			e.expireBreakerSampleLocked(s, time.Now())
		}
		e.breakerMu.Unlock()
	}
}
