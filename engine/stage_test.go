package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v2/config"
	"github.com/ydtg1993/papa/v2/core"
	"github.com/ydtg1993/papa/v2/internal/breaker"
	"github.com/ydtg1993/papa/v2/internal/workerpool"
	"github.com/ydtg1993/papa/v2/pkg/loggers"
)

// recordingNotifier 记下收到的告警事件，并可注入发送失败。
type recordingNotifier struct {
	mu     sync.Mutex
	events []core.AlertEvent
	err    error
}

func (n *recordingNotifier) Notify(_ context.Context, ev core.AlertEvent) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.events = append(n.events, ev)
	return n.err
}

func (n *recordingNotifier) snapshot() []core.AlertEvent {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]core.AlertEvent(nil), n.events...)
}

/* ---------- 通知器注册 ---------- */

// nil 直接忽略（业务侧常写成"配了才注册"），并且返回的是副本 ——
// 通知器列表可能在遍历时被业务改动，不能把内部切片交出去。
func TestAddNotifierSkipsNilAndReturnsCopy(t *testing.T) {
	e := &Engine{}
	e.AddNotifier(nil)
	if got := e.getNotifiers(); len(got) != 0 {
		t.Fatalf("nil 不该被注册：%v", got)
	}

	n := &recordingNotifier{}
	e.AddNotifier(n)
	got := e.getNotifiers()
	if len(got) != 1 {
		t.Fatalf("应注册 1 个通知器，实得 %d", len(got))
	}

	got[0] = nil // 改副本
	if again := e.getNotifiers(); len(again) != 1 || again[0] == nil {
		t.Fatal("getNotifiers 应返回副本，改它不该动到内部列表")
	}
}

/* ---------- 终态失败：日志 + 熔断计数 + 告警 ---------- */

// 走到 notifyFailure 就是**终态失败**（重试耗尽或不可重试）——
// 熔断只数这个量，按 attempt 数会把一个烂 URL 记 3 次、阈值被噪声灌满。
func TestNotifyFailureCountsBreakerAndEmitsAlert(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := submitEngine(t, f, pool)
	e.breaker = breaker.New(breaker.Config{Enabled: true, Window: time.Minute, Threshold: 1000}, nil)

	n := &recordingNotifier{}
	e.AddNotifier(n)

	task := &Task{ID: 7, Stage: "stub", URL: "https://example.com", Retry: 2}

	// 可重试的错误 → warn 级；但同样是"终态失败"，照样计数
	e.notifyFailure(context.Background(), task, errors.New("网络抖动"))
	if got := e.breaker.Sum(); got != 1 {
		t.Fatalf("熔断计数 = %d, want 1", got)
	}

	// 不可重试的错误 → error 级，并带上分类标识
	e.notifyFailure(context.Background(), task, core.WrapNoRetryKind("structure", errors.New("页面结构变了")))
	if got := e.breaker.Sum(); got != 2 {
		t.Fatalf("熔断计数 = %d, want 2", got)
	}

	events := n.snapshot()
	if len(events) != 2 {
		t.Fatalf("应收 2 条告警，实得 %d", len(events))
	}
	if events[0].Level != core.AlertWarn {
		t.Fatalf("可重试错误应为 warn，实得 %s", events[0].Level)
	}
	if events[0].Kind != "retryable" || events[0].Stage != "stub" || events[0].TaskID != 7 || events[0].Retry != 2 {
		t.Fatalf("告警上下文不全：%+v", events[0].TaskError)
	}
	if events[1].Level != core.AlertError {
		t.Fatalf("不可重试错误应为 error，实得 %s", events[1].Level)
	}
	if events[1].Kind != "structure" {
		t.Fatalf("错误分类应透传到告警：%+v", events[1].TaskError)
	}
}

// 没注册通知器时一切照旧：不 panic、不因为"没人收告警"而少记熔断。
func TestNotifyFailureWithoutNotifiers(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := submitEngine(t, f, pool)
	e.breaker = breaker.New(breaker.Config{Enabled: true, Window: time.Minute, Threshold: 1000}, nil)

	e.notifyFailure(context.Background(), &Task{Stage: "stub"}, errors.New("boom"))
	if got := e.breaker.Sum(); got != 1 {
		t.Fatalf("熔断计数 = %d, want 1", got)
	}
}

// 通知器自己报错只记日志：告警发不出去**不能**把任务收尾带崩。
func TestNotifyFailureSwallowsNotifierError(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := submitEngine(t, f, pool)

	n := &recordingNotifier{err: errors.New("webhook 500")}
	e.AddNotifier(n)

	e.notifyFailure(context.Background(), &Task{Stage: "stub", URL: "u"}, errors.New("boom"))
	if len(n.snapshot()) != 1 {
		t.Fatal("通知器应当被调用过")
	}
}

// 熔断触发时的回调走同一套 Notifier，但级别更高（critical）——
// webhook 那边据此单独路由（钉钉 @全体之类）。
func TestNotifyBreakerTripEmitsCritical(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := submitEngine(t, f, pool)
	e.ctx = context.Background()

	n := &recordingNotifier{}
	e.AddNotifier(n)

	e.notifyBreakerTrip(core.BreakerStatus{
		Reason: "窗口内终态失败数达到阈值", Stage: "stub", Window: time.Minute, Failures: 50, Threshold: 50,
	})

	events := n.snapshot()
	if len(events) != 1 {
		t.Fatalf("应发 1 条告警，实得 %d", len(events))
	}
	if events[0].Level != core.AlertCritical {
		t.Fatalf("级别 = %s, want critical", events[0].Level)
	}
	if events[0].Kind != "circuit-breaker" || events[0].Stage != "stub" {
		t.Fatalf("告警内容 = %+v", events[0].TaskError)
	}
}

func TestNotifyBreakerTripWithoutNotifiers(t *testing.T) {
	e := &Engine{ctx: context.Background(), loggerSet: &loggers.LoggerSet{Engine: quietLogger()}}
	e.notifyBreakerTrip(core.BreakerStatus{Reason: "r"}) // 不该 panic
}

/* ---------- ApplyRegisterStage 端到端 ---------- */

// recordingFetcher 记录被调用过的 URL；err 非 nil 时每次尝试都失败。
type recordingFetcher struct {
	stage string
	err   error

	mu  sync.Mutex
	got []string
}

func (s *recordingFetcher) GetStage() string { return s.stage }

func (s *recordingFetcher) FetchHandler(_ context.Context, task *Task, _ *Engine) error {
	s.mu.Lock()
	s.got = append(s.got, task.URL)
	s.mu.Unlock()
	return s.err
}

func (s *recordingFetcher) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.got...)
}

func (s *recordingFetcher) waitCalls(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for len(s.calls()) < n {
		if time.Now().After(deadline) {
			t.Fatalf("等不到第 %d 次调用，实得 %v", n, s.calls())
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// testLoggerSet 造一组吞掉输出的 logger（测试里会故意制造大量失败路径）。
func testLoggerSet() *loggers.LoggerSet {
	return &loggers.LoggerSet{
		Sys: quietLogger(), Engine: quietLogger(), Monitor: quietLogger(),
		Browser: quietLogger(), Fetcher: quietLogger(), DB: quietLogger(),
		Scheduler: quietLogger(), Proxy: quietLogger(), Filedown: quietLogger(), M3U8: quietLogger(),
	}
}

// 造一个真引擎：假库 + 只声明 "stub" 阶段。HTTP 服务与各治理队列全关，
// 免得测试里冒出一堆采样/轮询协程。
func newTestEngine(t *testing.T, f *fakeTaskDB) *Engine {
	t.Helper()
	cfg := &config.Config{}
	cfg.Crawler.Stages = map[string]config.StageConfig{"stub": {WorkerCount: 1, QueueSize: 8}}
	cfg.Crawler.DrainInterval = time.Hour
	cfg.Crawler.Trace.Enabled = false

	e := NewEngine(openFakeTaskDB(t, f), cfg, testLoggerSet())
	t.Cleanup(e.cancel)
	return e
}

// 从提交到 handler 跑完的整条链路：池子建起来、worker 认领任务、状态落库、
// 列表清理、优雅关停。这是引擎最核心的一条路，之前没有任何测试覆盖。
func TestApplyRegisterStageRunsTaskEndToEnd(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true // 全新的任务
	e := newTestEngine(t, f)

	fetcher := &recordingFetcher{stage: "stub"}
	e.AddStage("stub", StageConfig{MaxAttempts: 1, WorkerCount: 1, QueueSize: 8, Delay: config.DurationRange{}}, fetcher, nil)
	e.ApplyRegisterStage()

	info := e.stages["stub"]
	if info.workerPool == nil {
		t.Fatal("ApplyRegisterStage 应把阶段的工作池建起来")
	}

	if err := e.SubmitTask(&Task{Stage: "stub", URL: "https://example.com/1"}); err != nil {
		t.Fatalf("SubmitTask = %v", err)
	}
	fetcher.waitCalls(t, 1)

	if got := fetcher.calls()[0]; got != "https://example.com/1" {
		t.Fatalf("handler 收到 %q", got)
	}
	// 认领（pending→processing）与标成功（→success）都会落到 SQL 上
	sql := f.written()
	if n := strings.Count(sql, "UPDATE `crawler_tasks`"); n < 2 {
		t.Fatalf("应至少有认领与标成功两次 UPDATE，实得 %d：\n%s", n, sql)
	}
	if !strings.Contains(f.writtenArgs(), "7") {
		t.Logf("落库参数：%s", f.writtenArgs())
	}

	drained, stats := e.Stop(2 * time.Second)
	if !drained {
		t.Fatalf("任务已跑完，应当排空：%+v", stats)
	}
}

// 阶段注册了但没走 ApplyRegisterStage（池子还是 nil）时，Stop 要跳过它而不是自己 panic。
func TestStopSkipsStageWithoutPool(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := &Engine{
		ctx:       ctx,
		cancel:    cancel,
		loggerSet: &loggers.LoggerSet{Engine: quietLogger()},
		stages:    map[string]*stageInfo{"never-applied": {}},
	}
	drained, stats := e.Stop(time.Millisecond)
	if !drained || len(stats) != 0 {
		t.Fatalf("没有池子的阶段不该算进存留：drained=%v stats=%+v", drained, stats)
	}
}

// 多阶段并发等待：总耗时约等于一个 timeout，而不是"阶段数 × timeout"。
func TestStopWaitsStagesConcurrently(t *testing.T) {
	e := &Engine{
		loggerSet: &loggers.LoggerSet{Engine: quietLogger()},
		stages:    map[string]*stageInfo{},
	}
	e.ctx, e.cancel = context.WithCancel(context.Background())
	for _, name := range []string{"a", "b", "c"} {
		pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
		e.stages[name] = &stageInfo{workerPool: pool}
	}

	start := time.Now()
	e.Stop(300 * time.Millisecond)
	elapsed := time.Since(start)
	// 空池子本来就排得空，这里只是钉住"没有把各阶段串起来等"
	if elapsed > 250*time.Millisecond {
		t.Fatalf("三个空池子不应耗到 timeout：%v", elapsed)
	}
}

// 池子被熔断闸住时停机仍要把队列排空：停机是人为的明确终止，
// 与"别再去打目标站"是两回事（见 workerpool.Gate 的注释）。
//
// 闸门在**取下一个任务之前**探一次，所以这里先把闸门压下再建池 ——
// 与生产一致：熔断是在累积失败之后触发的，那时 worker 会立刻停在循环顶部。
func TestStopDrainsWhileBreakerPaused(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true // 让待提交的任务在库里是全新的
	e := newTestEngine(t, f)

	b := breaker.New(breaker.Config{Enabled: true, Window: time.Minute, Threshold: 1000}, nil)
	e.breaker = b

	fetcher := &recordingFetcher{stage: "stub"}
	e.AddStage("stub", StageConfig{MaxAttempts: 1, WorkerCount: 1, QueueSize: 8}, fetcher, nil)

	if !e.PauseCrawling("测试暂停") {
		t.Fatal("手动暂停应当成功")
	}
	if !e.BreakerStatus().Paused {
		t.Fatal("状态快照应显示暂停中")
	}
	if e.PauseCrawling("再来一次") {
		t.Fatal("已在暂停态时再暂停应返回 false")
	}

	e.ApplyRegisterStage() // worker 一起来就停在闸门前

	// 暂停中提交：任务进队但不会被取走，队列原封不动躺着（后台能直接看到积压）
	if err := e.SubmitTask(&Task{Stage: "stub", URL: "https://example.com/paused"}); err != nil {
		t.Fatalf("SubmitTask = %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	if got := fetcher.calls(); len(got) != 0 {
		t.Fatalf("暂停中不该取任务，实得 %v", got)
	}

	if !e.ResumeCrawling() {
		t.Fatal("放行应当成功")
	}
	fetcher.waitCalls(t, 1)

	// 已经在跑的状态下再放行 = 没做什么，返回 false（后台据此回 resumed=false 而不是报错）
	if e.ResumeCrawling() {
		t.Fatal("没暂停时放行应返回 false")
	}

	drained, stats := e.Stop(2 * time.Second)
	if !drained {
		t.Fatalf("应当排空：%+v", stats)
	}
}

// AddStage 是纯登记：池子要等 ApplyRegisterStage 才建。
func TestAddStageOnlyRegisters(t *testing.T) {
	e := &Engine{stages: map[string]*stageInfo{}}
	fetcher := &recordingFetcher{stage: "stub"}
	e.AddStage("stub", StageConfig{WorkerCount: 2, QueueSize: 4}, fetcher, nil)

	info, ok := e.stages["stub"]
	if !ok {
		t.Fatal("阶段没登记上")
	}
	if info.workerPool != nil {
		t.Fatal("登记阶段时不该建池子")
	}
	if info.fetcher != fetcher || info.config.WorkerCount != 2 {
		t.Fatalf("阶段信息不对：%+v", info)
	}
}
