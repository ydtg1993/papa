package crawler

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v2/config"
)

// newStatsEngine 构造只带队列监控所需字段的引擎，避免依赖 DB。
func newStatsEngine(t *testing.T) *Engine {
	t.Helper()
	e := &Engine{
		cfg:       &config.Config{},
		queueRuns: newQueueRuns(),
	}
	e.queueCounters = map[string]*atomic.Int64{
		QueueError:   &e.errorRetriedCount,
		QueueRecover: &e.recoveredCount,
		QueueRepeat:  &e.repeatRepolledCount,
	}
	e.runtime.Store(&config.RuntimeConfig{})
	return e
}

func TestQueueStatsRunLifecycle(t *testing.T) {
	e := newStatsEngine(t)

	// 未执行过：无运行次数、无完成时间
	got := e.GetQueueStats()[QueueError]
	if got.Running || got.Runs != 0 || got.LastProcessed != 0 || !got.LastFinishAt.IsZero() {
		t.Fatalf("initial stat = %+v, want zero value", got)
	}

	e.beginQueueRun(QueueError)
	got = e.GetQueueStats()[QueueError]
	if !got.Running || got.StartedAt.IsZero() {
		t.Fatalf("after begin: running=%v started_at=%v, want running with start time", got.Running, got.StartedAt)
	}

	time.Sleep(2 * time.Millisecond)
	e.endQueueRun(QueueError, 7, nil)
	got = e.GetQueueStats()[QueueError]
	if got.Running {
		t.Fatal("after end: still running")
	}
	if got.Runs != 1 || got.LastProcessed != 7 {
		t.Fatalf("after end: runs=%d last_processed=%d, want 1/7", got.Runs, got.LastProcessed)
	}
	if got.LastFinishAt.IsZero() || got.LastDuration <= 0 {
		t.Fatalf("after end: finish_at=%v duration=%v, want non-zero", got.LastFinishAt, got.LastDuration)
	}
	if got.LastError != "" {
		t.Fatalf("after end: last_error=%q, want empty", got.LastError)
	}

	// 执行出错：错误被记录，并在下次成功执行后清空
	e.beginQueueRun(QueueError)
	e.endQueueRun(QueueError, 0, errors.New("query failed"))
	if got = e.GetQueueStats()[QueueError]; got.LastError != "query failed" || got.Runs != 2 {
		t.Fatalf("after error: last_error=%q runs=%d, want \"query failed\"/2", got.LastError, got.Runs)
	}
	e.beginQueueRun(QueueError)
	e.endQueueRun(QueueError, 1, nil)
	if got = e.GetQueueStats()[QueueError]; got.LastError != "" {
		t.Fatalf("after recovered run: last_error=%q, want empty", got.LastError)
	}
}

func TestQueueStatsProgressAndTotal(t *testing.T) {
	e := newStatsEngine(t)

	// 累计计数：未运行时 total 直接取累计值，本轮进度为 0
	e.errorRetriedCount.Store(10)
	got := e.GetQueueStats()[QueueError]
	if got.TotalProcessed != 10 || got.RunProcessed != 0 {
		t.Fatalf("idle: total=%d run=%d, want 10/0", got.TotalProcessed, got.RunProcessed)
	}

	// 运行中：本轮进度 = 当前累计 - 本轮起点
	e.beginQueueRun(QueueError)
	e.errorRetriedCount.Store(25)
	got = e.GetQueueStats()[QueueError]
	if got.TotalProcessed != 25 || got.RunProcessed != 15 {
		t.Fatalf("running: total=%d run=%d, want 25/15", got.TotalProcessed, got.RunProcessed)
	}

	// 结束后本轮进度归零，累计保留
	e.endQueueRun(QueueError, 15, nil)
	got = e.GetQueueStats()[QueueError]
	if got.TotalProcessed != 25 || got.RunProcessed != 0 || got.LastProcessed != 15 {
		t.Fatalf("finished: total=%d run=%d last=%d, want 25/0/15", got.TotalProcessed, got.RunProcessed, got.LastProcessed)
	}
}

func TestQueueStatsBacklog(t *testing.T) {
	e := newStatsEngine(t)

	if got := e.GetQueueStats()[QueueRepeat]; got.Backlog != 0 || !got.BacklogAt.IsZero() {
		t.Fatalf("initial backlog = %d/%v, want 0/zero", got.Backlog, got.BacklogAt)
	}

	e.setQueueBacklog(QueueRepeat, 42)
	got := e.GetQueueStats()[QueueRepeat]
	if got.Backlog != 42 || got.BacklogAt.IsZero() {
		t.Fatalf("backlog = %d/%v, want 42/non-zero", got.Backlog, got.BacklogAt)
	}
}

func TestQueueStatsEnabledFollowsConfig(t *testing.T) {
	e := newStatsEngine(t)
	e.cfg.ErrorQueue.Enabled = true
	if got := e.GetQueueStats()[QueueError]; !got.Enabled {
		t.Fatal("error_queue should be enabled from base config")
	}

	// 运行期覆盖优先：关闭后快照同步反映
	off := false
	e.runtime.Store(&config.RuntimeConfig{
		ErrorQueue: config.RuntimeErrorQueueConfig{Enabled: &off},
	})
	if got := e.GetQueueStats()[QueueError]; got.Enabled {
		t.Fatal("error_queue should be disabled by runtime overlay")
	}
}

// TestQueueStatsAllQueuesReported 三个队列都应出现在快照里，界面不会漏显示。
func TestQueueStatsAllQueuesReported(t *testing.T) {
	got := newStatsEngine(t).GetQueueStats()
	for _, name := range []string{QueueError, QueueRecover, QueueRepeat} {
		s, ok := got[name]
		if !ok {
			t.Fatalf("queue %s missing from snapshot", name)
		}
		if s.Name != name {
			t.Fatalf("queue %s: Name=%q", name, s.Name)
		}
	}
	if len(got) != 3 {
		t.Fatalf("snapshot has %d queues, want 3", len(got))
	}
}

// TestQueueStatsNilSafe 监控接口在引擎未完整初始化时不应 panic。
func TestQueueStatsNilSafe(t *testing.T) {
	e := &Engine{}
	if got := e.GetQueueStats(); len(got) != 0 {
		t.Fatalf("bare engine snapshot = %v, want empty", got)
	}
	e.beginQueueRun(QueueError)
	e.endQueueRun(QueueError, 1, nil)
	e.setQueueBacklog(QueueError, 5)
}
