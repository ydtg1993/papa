package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

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

// Reserve under the admission lock BEFORE SQL so normal scheduling cannot
// complete the selected sample between lookup and requeue. I/O holds no lock.
func (e *Engine) retryBreakerSamples(ctx context.Context) {
	if e.db == nil {
		return
	}
	e.breakers.breakerMu.Lock()
	var sites []string
	for site := range e.breakers.smartBreakers {
		sites = append(sites, site)
	}
	e.breakers.breakerMu.Unlock()
	for _, site := range sites {
		if ctx.Err() != nil {
			return
		}
		e.breakers.breakerMu.Lock()
		s := e.breakers.smartBreakers[site]
		l, now := s.event, time.Now()
		if l == nil || l.PendingFailure != nil || !l.MixedSamples || (l.State != "observing" && l.State != "ramping") || !now.Before(l.Deadline) || s.draining || s.inflight > 0 || s.sampleDispatching || s.savedRevision < s.controlRevision || now.Before(s.nextSampleRetry) || e.hasManualBreakerPause(site) {
			e.breakers.breakerMu.Unlock()
			continue
		}
		e.breakers.expireBreakerSampleLocked(s, now)
		if l.SampleID == "" {
			stage, id := nextBreakerSample(s)
			if id != "" {
				e.breakers.selectBreakerSampleLocked(s, stage, id, now)
			}
			e.breakers.breakerMu.Unlock()
			continue
		}
		taskID, parseErr := strconv.ParseInt(strings.TrimPrefix(l.SampleID, "task:"), 10, 64)
		if parseErr != nil || taskID <= 0 || s.savedRevision < s.controlRevision {
			e.breakers.breakerMu.Unlock()
			continue
		}
		id, stage, deadline := l.ID, l.SampleStage, l.SampleDeadline
		s.sampleDispatching = true
		s.nextSampleRetry = now.Add(max(time.Second, s.policy.Interval))
		e.breakers.breakerMu.Unlock()

		var record models.CrawlerTask
		err := e.db.WithContext(ctx).Where("id = ? AND site = ? AND stage = ?", taskID, site, stage).First(&record).Error
		requeued := false
		e.breakers.breakerMu.Lock()
		valid := s.event == l && l.SampleDeadline.Equal(deadline) && time.Now().Before(deadline) && (l.State == "observing" || l.State == "ramping") && !e.hasManualBreakerPause(site)
		e.breakers.breakerMu.Unlock()
		if valid && err == nil && record.Status != models.TaskStatusPending && record.Status != models.TaskStatusProcessing {
			retryCtx, cancel := context.WithDeadline(ctx, deadline)
			version := record.CompensationVersion
			err = e.retryTaskRecord(retryCtx, &record, record.Reprocess, &version)
			cancel()
			requeued = err == nil
		}
		e.breakers.breakerMu.Lock()
		s.sampleDispatching = false
		if s.event == l && l.ID == id && l.SampleDeadline.Equal(deadline) {
			if requeued {
				e.breakers.noteBreakerLocked(s, "sample_retry", stage, fmt.Sprintf("串行重投验证样本 %d；复用原有任务认领与版本规则", taskID), time.Now())
			} else if err != nil && !errors.Is(err, ErrTaskChanged) && !errors.Is(err, ErrTaskProcessing) {
				e.breakers.noteBreakerLocked(s, "sample_retry_unavailable", stage, fmt.Sprintf("验证样本 %d 暂不能重试：%v；等待至原截止时间，不延长预算", taskID, err), time.Now())
				s.nextSampleRetry = time.Now().Add(max(s.policy.ProbeInterval, s.policy.Interval))
			}
			e.breakers.expireBreakerSampleLocked(s, time.Now())
		}
		e.breakers.breakerMu.Unlock()
	}
}
