package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/internal/breaker"
	"github.com/ydtg1993/papa/v3/internal/workerpool"
	"github.com/ydtg1993/papa/v3/pkg/loggers"
	"github.com/ydtg1993/papa/v3/pkg/middleware/filedown"
	"github.com/ydtg1993/papa/v3/pkg/middleware/m3u8"
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

	mu    sync.Mutex
	got   []string
	times []time.Time // 每次调用的时刻，与 got 一一对应（验证退避节奏用）
}

func (s *recordingFetcher) GetStage() string { return s.stage }

func (s *recordingFetcher) FetchHandler(_ context.Context, task *Task, _ *Engine) error {
	s.mu.Lock()
	s.got = append(s.got, task.URL)
	s.times = append(s.times, time.Now())
	s.mu.Unlock()
	return s.err
}

func (s *recordingFetcher) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.got...)
}

// callsAt 返回各次调用的时刻（与 calls 一一对应）。
func (s *recordingFetcher) callsAt() []time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]time.Time(nil), s.times...)
}

func (s *recordingFetcher) waitCalls(t *testing.T, n int) {
	t.Helper()
	s.waitCallsWithin(t, n, 3*time.Second)
}

// waitCallsWithin 同上，但等待上限自己定 —— 用例把 stage 的 backoff 配得比 3s 还大时，
// 默认那个上限就成了"正好卡在截止线上"的偶发失败（实测：backoff 3s 的用例每几轮红一次）。
func (s *recordingFetcher) waitCallsWithin(t *testing.T, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
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
	// 阶段不在配置里了（参数搬进 Go 声明）：这个引擎的 "stub" 阶段由各用例自己 AddStage。
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

/* ---------- 成功后的「任务间隔延迟」与停机 ---------- */

// 停机不能被"任务跑完之后那段间隔延迟"拖住。
//
// 回归点：那段延迟原来是裸的 `<-time.After(cfg.Delay.Random())`，而模板里
// catalog 的 `delay: "5m"`、`stop_timeout: "5s"` —— 任务其实早就跑完了
// （状态已落库、去重表已清），worker 却还抱着并发位在睡，于是：
//
//	Engine.Stop 每次都等满 timeout → drained=false → app.Run 打出
//	「引擎未在 5s 内排空，仍有任务未跑完」并**跳过关库与浏览器池关闭**。
//
// 现在改成 select ctx.Done，与 20 行外重试退避那段的写法一致。
func TestStopIsNotBlockedByPostSuccessDelay(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true
	e := newTestEngine(t, f)

	fetcher := &recordingFetcher{stage: "stub"}
	e.AddStage("stub", StageConfig{
		MaxAttempts: 1, WorkerCount: 1, QueueSize: 8,
		// 模板里的量级：catalog 的 delay 就是 5m
		Delay: config.DurationRange{Min: 5 * time.Minute, Max: 5 * time.Minute},
	}, fetcher, nil)
	e.ApplyRegisterStage()

	if err := e.SubmitTask(&Task{Stage: "stub", URL: "https://example.com/1"}); err != nil {
		t.Fatalf("SubmitTask = %v", err)
	}
	fetcher.waitCalls(t, 1)
	time.Sleep(80 * time.Millisecond) // 让它进入成功后那段延迟

	start := time.Now()
	drained, stats := e.Stop(2 * time.Second)
	elapsed := time.Since(start)

	if !drained {
		t.Fatalf("任务已成功落库，排空不该被延迟拖住：stats=%+v", stats)
	}
	if elapsed > time.Second {
		t.Fatalf("Stop 应当立刻返回，实耗 %v（说明延迟仍不可打断）", elapsed)
	}
}

// 反过来钉住：没人停机时那段延迟照旧生效、照旧占着 worker。
// 「可打断」不等于「可以省掉」—— 它的存在意义就是让两次抓取之间隔开，
// 否则防反爬的那层意图就静默消失了。
func TestPostSuccessDelayStillRateLimits(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true
	e := newTestEngine(t, f)

	fetcher := &recordingFetcher{stage: "stub"}
	e.AddStage("stub", StageConfig{
		MaxAttempts: 1, WorkerCount: 1, QueueSize: 8,
		Delay: config.DurationRange{Min: 500 * time.Millisecond, Max: 500 * time.Millisecond},
	}, fetcher, nil)
	e.ApplyRegisterStage()

	if err := e.SubmitTask(&Task{Stage: "stub", URL: "https://example.com/1"}); err != nil {
		t.Fatalf("SubmitTask = %v", err)
	}
	fetcher.waitCalls(t, 1)

	// 假库上抓取本身是瞬时的，所以此刻 worker 必然已经在延迟里
	info := e.stages["stub"]
	time.Sleep(150 * time.Millisecond)
	if _, _, _, inProgress, _ := info.workerPool.Stats(); inProgress != 1 {
		t.Fatalf("延迟期间该任务应仍占着 worker，实得 inProgress=%d", inProgress)
	}
	if got := len(fetcher.calls()); got != 1 {
		t.Fatalf("延迟期间不该开始下一次抓取，实得 %d 次", got)
	}

	// 延迟走完自然收工
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, _, _, inProgress, _ := info.workerPool.Stats(); inProgress == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("延迟结束后应当收工")
		}
		time.Sleep(10 * time.Millisecond)
	}

	drained, stats := e.Stop(2 * time.Second)
	if !drained {
		t.Fatalf("应当排空：%+v", stats)
	}
}

// 一个阶段的 submitFunc 向**别的**阶段投任务是允许的 —— 池子必须全部建好、再跑回调。
//
// 回归点：原来是一趟遍历（边建池边跑回调），回调投给"还没建池"的那个阶段就是
// `info.workerPool.Submit` 的 nil 解引用 panic，而且崩不崩取决于 map 的遍历顺序。
// 这里两个阶段的回调**互相**投递，无论谁先被遍历到都躲不过 —— 顺序随机也能稳定复现。
// （脚手架和文档里的单个阶段投自己，所以模板路径碰不到它；跨阶段派发才是触发前提。）
func TestApplyRegisterStageAllowsCrossStageSubmit(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true // 起始任务都是全新的，走 INSERT

	cfg := &config.Config{}
	cfg.Crawler.DrainInterval = time.Hour
	e := NewEngine(openFakeTaskDB(t, f), cfg, testLoggerSet())
	t.Cleanup(e.cancel)

	stageCfg := StageConfig{MaxAttempts: 1, WorkerCount: 1, QueueSize: 8, Delay: config.DurationRange{}}
	fetcherA := &recordingFetcher{stage: "a"}
	fetcherB := &recordingFetcher{stage: "b"}

	var submitErrs []error
	e.AddStage("a", stageCfg, fetcherA, func(eng *Engine) {
		submitErrs = append(submitErrs, eng.SubmitTask(&Task{Stage: "b", URL: "https://example.com/init-b"}))
	})
	e.AddStage("b", stageCfg, fetcherB, func(eng *Engine) {
		submitErrs = append(submitErrs, eng.SubmitTask(&Task{Stage: "a", URL: "https://example.com/init-a"}))
	})

	e.ApplyRegisterStage() // 原来这里会 panic

	for _, err := range submitErrs {
		if err != nil {
			t.Fatalf("跨阶段投递不该失败：%v", err)
		}
	}
	// 两条起始任务都要真的跑到各自的 handler 上（证明投进了**对**的池子）
	fetcherB.waitCalls(t, 1)
	if got := fetcherB.calls()[0]; got != "https://example.com/init-b" {
		t.Fatalf("b 阶段收到 %q，want a 的回调投的那条", got)
	}
	fetcherA.waitCalls(t, 1)
	if got := fetcherA.calls()[0]; got != "https://example.com/init-a" {
		t.Fatalf("a 阶段收到 %q，want b 的回调投的那条", got)
	}
}

// 池子还没建就提交（AddStage 之后、ApplyRegisterStage 之前）要报错，不是 nil 解引用 panic。
// 判据与 spill.go / taskadmin.go 一致：这一类都算「阶段不可用」，后台能翻成 409。
func TestSubmitBeforeApplyRegisterStageFails(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true // 全新任务：否则会先命中 SubmitTask 的去重（"库里已有"直接 return nil），走不到池子那一步
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := submitEngine(t, f, pool)
	// 把池子摘掉，模拟"注册了但还没走到 ApplyRegisterStage"
	e.stages["stub"].workerPool = nil

	err := e.SubmitTask(&Task{Stage: "stub", URL: "https://example.com/1"})
	if err == nil {
		t.Fatal("池子没建时应报错，而不是 panic")
	}
	if !errors.Is(err, ErrStageNotRegistered) {
		t.Fatalf("错误应当是 ErrStageNotRegistered，实得：%v", err)
	}
}

// needsDownloaders 声明依赖下载器（可选接口），用来验证启动期的接线校验。
type needsDownloaders struct {
	recordingFetcher
	filedown bool
	m3u8     bool
}

func (s *needsDownloaders) NeedsFiledown() bool { return s.filedown }
func (s *needsDownloaders) NeedsM3U8() bool     { return s.m3u8 }

// assertPanicsContains 断言 f() panic 且信息里含 want（启动期的校验都是 panic：与配置校验层同取向）。
func assertPanicsContains(t *testing.T, want string, f func()) {
	t.Helper()
	defer func() {
		r := recover()
		if r == nil {
			t.Fatalf("应当 panic（想看到 %q）", want)
		}
		if !strings.Contains(fmt.Sprint(r), want) {
			t.Fatalf("panic 信息里缺 %q，实得：%v", want, r)
		}
	}()
	f()
}

// 声明了依赖下载器却没接线：**启动时**就炸掉，而不是等第一条任务跑到那一步 ——
// 那时失败的是任务、不是启动，报出来的是一句离得很远的 "file downloader is configured"。
// 报错里要写清怎么改（几乎总是"忘了在 ApplyRegisterStage 之前调 SetXxx"）。
func TestApplyRegisterStageRejectsUnwiredDownloader(t *testing.T) {
	cases := []struct {
		name     string
		fetcher  Fetcher
		wire     func(*Engine)
		wantWord string
	}{
		{
			name:     "声明了文件下载器但没接线",
			fetcher:  &needsDownloaders{recordingFetcher: recordingFetcher{stage: "stub"}, filedown: true},
			wantWord: "SetFiledown",
		},
		{
			name:     "声明了 m3u8 但没接线",
			fetcher:  &needsDownloaders{recordingFetcher: recordingFetcher{stage: "stub"}, m3u8: true},
			wantWord: "SetM3U8",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assertPanicsContains(t, c.wantWord, func() {
				e := stageDepEngine(t)
				e.AddStage("stub", stageDepCfg, c.fetcher, nil)
				e.ApplyRegisterStage()
			})
		})
	}
}

// 接了线、或压根没声明，都不该炸 —— 校验是给"声明了却没接线"这一种情况用的。
func TestApplyRegisterStageAcceptsWiredOrUndeclared(t *testing.T) {
	t.Run("接了线", func(t *testing.T) {
		e := stageDepEngine(t)
		e.SetFiledown(filedown.NewDownloader(filedown.DefaultConfig()))
		e.SetM3U8(m3u8.NewDownloader(m3u8.DefaultConfig()))
		e.AddStage("stub", stageDepCfg, &needsDownloaders{recordingFetcher: recordingFetcher{stage: "stub"}, filedown: true, m3u8: true}, nil)
		e.ApplyRegisterStage()
	})
	t.Run("没声明", func(t *testing.T) {
		e := stageDepEngine(t)
		e.AddStage("stub", stageDepCfg, &recordingFetcher{stage: "stub"}, nil)
		e.ApplyRegisterStage()
	})
}

var stageDepCfg = StageConfig{MaxAttempts: 1, WorkerCount: 1, QueueSize: 8, Delay: config.DurationRange{}}

// stageDepEngine 造一个只声明 stub 阶段的引擎（依赖校验用例的公共脚手架）。
func stageDepEngine(t *testing.T) *Engine {
	t.Helper()
	f := newFakeTaskDB()
	cfg := &config.Config{}
	cfg.Crawler.DrainInterval = time.Hour
	e := NewEngine(openFakeTaskDB(t, f), cfg, testLoggerSet())
	t.Cleanup(e.cancel)
	return e
}

// 重试的退避只发生在**两次尝试之间**：最后一次尝试失败之后要立刻落 failed，不能再等一个周期。
//
// 回归点：退避原来写在每一轮的**末尾**，于是最后一次失败之后还要睡满 backoff<<(N-1) ——
// 3 次尝试、30s 退避就是 120s，期间 worker 抱着一个并发位空等。实测（huangguo 真实环境）：
// 一个注定失败的 detail 任务从认领到落 failed 正好 210s = 30+60+120；episode 阶段只配
// 1 个 worker，等于把整个阶段堵 3.5 分钟。m3u8 / filedown 的退避写在每轮**开头**，本就没有这个问题。
func TestRetryBackoffOnlyBetweenAttempts(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true
	e := newTestEngine(t, f)

	const backoff = 3 * time.Second
	fetcher := &recordingFetcher{stage: "stub", err: errors.New("boom")} // 每次尝试都失败
	e.AddStage("stub", StageConfig{
		MaxAttempts: 2, WorkerCount: 1, QueueSize: 8, Backoff: backoff, Delay: config.DurationRange{},
	}, fetcher, nil)
	e.ApplyRegisterStage()

	if err := e.SubmitTask(&Task{Stage: "stub", URL: "https://example.com/1"}); err != nil {
		t.Fatalf("SubmitTask = %v", err)
	}
	// 上限给足：这个用例的 backoff 就是 3s，用默认的 3s 上限会正好卡在截止线上
	fetcher.waitCallsWithin(t, 2, 15*time.Second)

	// ① 两次尝试之间仍然要退避（别把退避本身一起改没了）
	times := fetcher.callsAt()
	if gap := times[1].Sub(times[0]); gap < backoff {
		t.Fatalf("两次尝试之间应等满一个退避（%v），实得 %v", backoff, gap)
	}

	// ② 最后一次失败之后不该再等：终态马上落库（老行为要再等 backoff<<1 = 6s）
	deadline := time.Now().Add(backoff / 2)
	for time.Now().Before(deadline) {
		if f.writtenArgs_hasError("boom") { // 落 failed 时会把原因写进 error 列
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("最后一次尝试失败后 %v 内没有落 failed —— 说明还在白等退避", backoff/2)
}

// 熔断按 scope（站点）分组：每站一把闸门，各停各的；不点名就是**全部**（后台那个「恢复抓取」）。
func TestBreakerScopesAreIndependent(t *testing.T) {
	f := newFakeTaskDB()
	cfg := &config.Config{}
	cfg.Crawler.DrainInterval = time.Hour
	cfg.Crawler.Breaker = config.BreakerConfig{Enabled: true, Window: 5 * time.Minute, Threshold: 50}
	e := NewEngine(openFakeTaskDB(t, f), cfg, testLoggerSet())
	t.Cleanup(e.cancel)

	// 站点 a 单开一把（阈值不同），站点 b 不单配 → 用默认那把（但 b 有自己的 scope 名）
	e.SetSiteBreaker("a", breaker.Config{Enabled: true, Window: 5 * time.Minute, Threshold: 10})
	e.SetSiteBreaker("b", breaker.Config{Enabled: true, Window: 5 * time.Minute, Threshold: 50})

	// 三个 scope 都要在状态里（默认 + a + b）
	sts := e.BreakerStatuses()
	for _, site := range []string{"", "a", "b"} {
		if _, ok := sts[site]; !ok {
			t.Fatalf("状态里应有 scope %q，实得 %v", site, sts)
		}
	}
	if sts["a"].Threshold != 10 || sts["b"].Threshold != 50 {
		t.Fatalf("各 scope 的阈值应各用各的：a=%d b=%d", sts["a"].Threshold, sts["b"].Threshold)
	}
	if sts["a"].Site != "a" {
		t.Fatalf("状态里应带上站点：%q", sts["a"].Site)
	}

	// 只闸住 a：默认 scope 与 b 照常跑
	if !e.PauseSite("a", "站点 a 被墙") {
		t.Fatal("PauseSite(a) 应当成功")
	}
	sts = e.BreakerStatuses()
	if !sts["a"].Paused || sts[""].Paused || sts["b"].Paused {
		t.Fatalf("只该闸住 a：%+v", sts)
	}
	// 点名放行 a
	if !e.ResumeSite("a") {
		t.Fatal("ResumeSite(a) 应当成功")
	}
	if e.BreakerStatuses()["a"].Paused {
		t.Fatal("a 应当已放行")
	}
	// 不存在的 scope：不静默作用到默认那把
	if e.PauseSite("ghost", "x") || e.ResumeSite("ghost") {
		t.Fatal("不存在的 scope 应当返回 false（后台据此回 400）")
	}
	// 全部：三个一起闸住、一起放行
	if !e.PauseCrawling("手动") {
		t.Fatal("PauseCrawling 应当成功")
	}
	sts = e.BreakerStatuses()
	if !sts[""].Paused || !sts["a"].Paused || !sts["b"].Paused {
		t.Fatalf("PauseCrawling 应当闸住所有 scope：%+v", sts)
	}
	if !e.ResumeCrawling() {
		t.Fatal("ResumeCrawling 应当成功")
	}
	sts = e.BreakerStatuses()
	if sts[""].Paused || sts["a"].Paused || sts["b"].Paused {
		t.Fatalf("ResumeCrawling 应当放行所有 scope：%+v", sts)
	}
}

// 触发时回调要带上站点：告警文案不能只有一句"全任务暂停"，得知道是哪个站（多站时这是唯一的区分）。
func TestBreakerTripCarriesSite(t *testing.T) {
	f := newFakeTaskDB()
	cfg := &config.Config{}
	cfg.Crawler.DrainInterval = time.Hour
	cfg.Crawler.Breaker = config.BreakerConfig{Enabled: true, Window: 5 * time.Minute, Threshold: 50}
	e := NewEngine(openFakeTaskDB(t, f), cfg, testLoggerSet())
	t.Cleanup(e.cancel)
	e.ctx = context.Background()

	n := &recordingNotifier{}
	e.AddNotifier(n)
	e.SetSiteBreaker("siteb", breaker.Config{Enabled: true, Window: 5 * time.Minute, Threshold: 1})

	// 直接驱动那把小闸门（阈值 1）：它触发时会走 engine 的告警回调
	b := e.siteBreakerOf("siteb")
	if b == nil {
		t.Fatal("站点 siteb 应当有自己的闸门")
	}
	b.RecordFailure("siteb_catalog")

	events := n.snapshot()
	if len(events) != 1 {
		t.Fatalf("应发 1 条告警，实得 %d", len(events))
	}
	if events[0].Site != "siteb" || events[0].Stage != "siteb_catalog" {
		t.Fatalf("告警里应带站点与阶段：%+v", events[0].TaskError)
	}
}
