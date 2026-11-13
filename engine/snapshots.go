package engine

import (
	"strings"
	"time"

	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/pkg/events"
)

// startSnapshotEmitter 按 obs.events.snapshot_interval 周期发 stage_snapshot。
//
// 与 server.enabled 无关：事件流本身的用途是给报告引擎做时间序列，
// 监控页开不开不影响它。事件流关闭或 interval<=0 时不起 goroutine。
func (e *Engine) startSnapshotEmitter() {
	if e.events == nil || !e.cfg.Obs.Events.Enabled {
		return
	}
	interval := e.cfg.Obs.Events.SnapshotInterval
	if interval <= 0 {
		return
	}
	// 与其它 ticker 一样进 tickerWG；只尝试读取有界内存快照，不查库、不等待业务锁。
	e.tickerWG.Add(1)
	go func() {
		defer e.tickerWG.Done()
		e.emitSnapshot()
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-e.ctx.Done():
				return
			case <-ticker.C:
				e.emitSnapshot()
			}
		}
	}()
}

// emitSnapshot 把当前阶段/队列快照作为一条事件发出。
func (e *Engine) emitSnapshot() {
	ev := events.Event{
		Type:   events.TypeStageSnapshot,
		Stages: make(map[string]events.StageSnapshot),
		Queues: make(map[string]events.QueueSnapshot),
	}
	// Observation reads only bounded scalar copies. It never waits for the
	// engine metadata lock or copies per-worker activity history.
	if e.mu.TryRLock() {
		for key, mon := range e.statsQueue {
			if len(ev.Stages) >= events.MaxMapEntries-2 {
				ev.Truncated = true
				break
			}
			info := e.stages[key]
			if info == nil {
				continue
			}
			submitted, completed, failed, inflight, queued := mon.WorkPool.Stats()
			totals := e.stageTotals[key]
			ev.Stages[key] = events.StageSnapshot{Site: info.config.Site, Stage: info.stage, QueueLen: queued,
				InProgress: int(inflight), Workers: info.config.WorkerCount,
				Submitted: totals.Submitted + submitted, Completed: totals.Completed + completed, Failed: totals.Failed + failed}
		}
		e.mu.RUnlock()
	} else {
		ev.Truncated = true
	}
	for name, q := range e.queueRuns {
		if !q.mu.TryLock() {
			ev.Truncated = true
			continue
		}
		s := q.stat
		atStart := q.atStart
		q.mu.Unlock()
		var total int64
		if c := e.queueCounters[name]; c != nil {
			total = c.Load()
		}
		var runProcessed int64
		if s.Running {
			runProcessed = total - atStart
		}
		ev.Queues[name] = events.QueueSnapshot{Enabled: e.queueEnabled(name), Running: s.Running, TotalProcessed: total,
			RunProcessed: runProcessed, Backlog: int64(s.Backlog), LastDurationMS: s.LastDuration.Milliseconds(), LastError: s.LastError}
	}
	e.emit(ev)
}

// emitTaskFailed 发一条 task_failed（终态失败）。err 为 nil 时不发。
//
// 三处终态失败共用：panic 收尾、不可重试直接失败、重试耗尽。
func (e *Engine) emitTaskFailed(task *Task, err error, claimedAt time.Time) {
	if err == nil {
		return
	}
	e.emit(events.Event{
		Type:       events.TypeTaskFailed,
		Site:       task.Site,
		Stage:      task.Stage,
		TaskID:     uint(task.ID),
		URL:        task.URL,
		Outcome:    "failed",
		Kind:       string(core.ErrorKind(err)),
		Attempt:    task.Retry,
		DurationMS: elapsedMS(claimedAt),
		Error:      events.TruncateError(err.Error()),
	})
}

// elapsedMS 从 since 到现在的毫秒数；since 为零值（还没认领）返回 0。
func elapsedMS(since time.Time) int64 {
	if since.IsZero() {
		return 0
	}
	d := time.Since(since)
	if d < 0 {
		return 0
	}
	return int64(d / time.Millisecond)
}

// splitStageKey 把 `site:stage` 复合键拆回两段（站点键与阶段名都不含冒号）。
func splitStageKey(key string) (site, stage string) {
	if i := strings.IndexByte(key, ':'); i >= 0 {
		return key[:i], key[i+1:]
	}
	return "", key
}
