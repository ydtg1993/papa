package engine

import (
	"context"
	"slices"
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
	sites := e.breakers.siteKeysLocked()
	e.breakers.unlockAndEmit()
	for _, site := range sites {
		if ctx.Err() != nil {
			return
		}
		e.breakers.breakerMu.Lock()
		token := e.breakers.reserveSampleLocked(site, e.hasManualBreakerPause(site))
		e.breakers.unlockAndEmit()
		if token == nil {
			continue
		}
		taskID, stage, deadline := token.taskID, token.stage, token.deadline

		var record models.CrawlerTask
		err := e.db.WithContext(ctx).Where("id = ? AND site = ? AND stage = ?", taskID, site, stage).First(&record).Error
		requeued := false
		e.breakers.breakerMu.Lock()
		valid := e.breakers.sampleValidLocked(token, e.hasManualBreakerPause(site))
		e.breakers.unlockAndEmit()
		if valid && err == nil && record.Status != models.TaskStatusPending && record.Status != models.TaskStatusProcessing {
			retryCtx, cancel := context.WithDeadline(ctx, deadline)
			version := record.CompensationVersion
			err = e.retryTaskRecord(retryCtx, &record, record.Reprocess, &version)
			cancel()
			requeued = err == nil
		}
		e.breakers.breakerMu.Lock()
		e.breakers.finishSampleLocked(token, requeued, err)
		e.breakers.unlockAndEmit()
	}
}
