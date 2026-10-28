package engine

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/pkg/events"
)

// collectorEmitter 把事件收进内存，供断言使用。
type collectorEmitter struct {
	mu  sync.Mutex
	got []events.Event
}

func (c *collectorEmitter) Emit(ev events.Event) {
	c.mu.Lock()
	c.got = append(c.got, ev)
	c.mu.Unlock()
}

func (c *collectorEmitter) snapshot() []events.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]events.Event, len(c.got))
	copy(out, c.got)
	return out
}

// waitEvent 等到指定类型的事件出现（worker 是异步的，finish/fail 在 handler 返回之后才发）。
func (c *collectorEmitter) waitEvent(t *testing.T, typ string) events.Event {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, ev := range c.snapshot() {
			if ev.Type == typ {
				return ev
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("等不到 %s 事件，实得 %+v", typ, c.snapshot())
	return events.Event{}
}

// 成功路径：claim → finish，且带 site/stage/task_id 与耗时。
func TestStageHandlerEmitsClaimAndFinish(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true // 全新任务
	e := newTestEngine(t, f)
	col := &collectorEmitter{}
	e.SetEventsEmitter(col)

	fetcher := &recordingFetcher{stage: "stub"}
	e.AddStage("stub", StageConfig{Site: "stub", MaxAttempts: 1, WorkerCount: 1, QueueSize: 8, Delay: config.DurationRange{}}, fetcher, nil)
	e.ApplyRegisterStage()

	if err := e.SubmitTask(&Task{Stage: "stub", Site: "stub", URL: "https://example.com/1"}); err != nil {
		t.Fatalf("SubmitTask = %v", err)
	}
	fetcher.waitCalls(t, 1)

	claimed := col.waitEvent(t, events.TypeTaskClaimed)
	if claimed.Site != "stub" || claimed.Stage != "stub" {
		t.Fatalf("claimed 事件维度不对：%+v", claimed)
	}
	finished := col.waitEvent(t, events.TypeTaskFinished)
	if finished.Site != "stub" || finished.Stage != "stub" {
		t.Fatalf("finished 事件维度不对：%+v", finished)
	}
	if finished.Outcome != "success" {
		t.Fatalf("finished.Outcome = %q", finished.Outcome)
	}
	if finished.TaskID == 0 {
		t.Fatalf("finished.TaskID = 0（任务应已落库拿到 ID）")
	}
	e.Stop(2 * time.Second) // 收尾（停 worker）；本用例只关心事件
}

// 失败路径：重试耗尽 → task_failed，带 kind 与错误文本。
func TestStageHandlerEmitsFailed(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true
	e := newTestEngine(t, f)
	col := &collectorEmitter{}
	e.SetEventsEmitter(col)

	fetcher := &recordingFetcher{stage: "stub", err: errors.New("boom")}
	e.AddStage("stub", StageConfig{Site: "stub", MaxAttempts: 1, WorkerCount: 1, QueueSize: 8, Delay: config.DurationRange{}}, fetcher, nil)
	e.ApplyRegisterStage()

	if err := e.SubmitTask(&Task{Stage: "stub", Site: "stub", URL: "https://example.com/2"}); err != nil {
		t.Fatalf("SubmitTask = %v", err)
	}
	failed := col.waitEvent(t, events.TypeTaskFailed)
	if failed.Outcome != "failed" {
		t.Fatalf("failed.Outcome = %q", failed.Outcome)
	}
	if failed.Kind == "" {
		t.Fatalf("failed.Kind 为空（应带错误分类）")
	}
	if failed.Error == "" {
		t.Fatalf("failed.Error 为空（应带错误文本）")
	}
}

// 队列轮次：endQueueRun 发 queue_round，带处理量与耗时。
func TestEndQueueRunEmitsQueueRound(t *testing.T) {
	e := &Engine{queueRuns: newQueueRuns()}
	col := &collectorEmitter{}
	e.SetEventsEmitter(col)

	e.beginQueueRun(QueueError)
	e.endQueueRun(QueueError, 3, nil)

	evs := col.snapshot()
	if len(evs) != 1 {
		t.Fatalf("应有 1 条事件，实得 %+v", evs)
	}
	ev := evs[0]
	if ev.Type != events.TypeQueueRound || ev.Queue != QueueError {
		t.Fatalf("事件不对：%+v", ev)
	}
	if ev.Counts["processed"] != 3 {
		t.Fatalf("processed = %d", ev.Counts["processed"])
	}
}

// 快照：stage_snapshot 必须带 Stages/Queues 两个 map（空也非 nil，读者无需判空分支）。
func TestEmitSnapshotCarriesMaps(t *testing.T) {
	e := &Engine{queueRuns: newQueueRuns(), cfg: &config.Config{}}
	col := &collectorEmitter{}
	e.SetEventsEmitter(col)

	e.emitSnapshot()

	evs := col.snapshot()
	if len(evs) != 1 || evs[0].Type != events.TypeStageSnapshot {
		t.Fatalf("应有 1 条 stage_snapshot，实得 %+v", evs)
	}
	if evs[0].Stages == nil || evs[0].Queues == nil {
		t.Fatalf("快照 map 不应为 nil：%+v", evs[0])
	}
}

// 事件流关闭（nil emitter）时 emit 不能 panic —— 默认关闭是主要形态。
func TestEmitWithoutEmitterIsNoop(t *testing.T) {
	e := &Engine{queueRuns: newQueueRuns(), cfg: &config.Config{}}
	e.emit(events.Event{Type: events.TypeRunStarted})
	e.beginQueueRun(QueueError)
	e.endQueueRun(QueueError, 1, errors.New("boom"))
	e.emitSnapshot()
}
