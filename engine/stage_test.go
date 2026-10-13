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
	// 闸门按站点取：这台引擎里那个阶段属于站点 stub
	e.SetSiteBreaker("stub", breaker.Config{Enabled: true, Window: time.Minute, Threshold: 1000})
	gate := e.breakerFor("stub")

	n := &recordingNotifier{}
	e.AddNotifier(n)

	task := &Task{ID: 7, Stage: "stub", Site: "stub", URL: "https://example.com", Retry: 2}

	// 可重试的错误 → warn 级；但同样是"终态失败"，照样计数
	e.notifyFailure(context.Background(), task, errors.New("网络抖动"))
	if got := gate.Sum(); got != 1 {
		t.Fatalf("熔断计数 = %d, want 1", got)
	}

	// 不可重试的错误 → error 级，并带上分类标识
	e.notifyFailure(context.Background(), task, core.WrapNoRetryKind("structure", errors.New("页面结构变了")))
	if got := gate.Sum(); got != 2 {
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
	e.SetSiteBreaker("stub", breaker.Config{Enabled: true, Window: time.Minute, Threshold: 1000})

	e.notifyFailure(context.Background(), &Task{Stage: "stub", Site: "stub"}, errors.New("boom"))
	if got := e.breakerFor("stub").Sum(); got != 1 {
		t.Fatalf("熔断计数 = %d, want 1", got)
	}
}

// 那个站点没有闸门（自己搭引擎没 SetSiteBreaker、或旧行 site 为空）时：**跳过计数、不 panic** ——
// 没有闸门就没有"熔断"这回事，但任务收尾（日志 + 告警）照旧。
func TestNotifyFailureWithoutGate(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := submitEngine(t, f, pool) // 一台没配任何闸门的引擎

	n := &recordingNotifier{}
	e.AddNotifier(n)
	e.notifyFailure(context.Background(), &Task{Stage: "stub", Site: "ghost"}, errors.New("boom"))

	if got := len(n.snapshot()); got != 1 {
		t.Fatalf("没有闸门也该照发告警，实得 %d 条", got)
	}
}

// 通知器自己报错只记日志：告警发不出去**不能**把任务收尾带崩。
func TestNotifyFailureSwallowsNotifierError(t *testing.T) {
	f := newFakeTaskDB()
	pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
	e := submitEngine(t, f, pool)

	n := &recordingNotifier{err: errors.New("webhook 500")}
	e.AddNotifier(n)

	e.notifyFailure(context.Background(), &Task{Stage: "stub", Site: "stub", URL: "u"}, errors.New("boom"))
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
	// 不用重赋 e.ctx：NewEngine 已经建好可取消的 ctx，重赋会与 delayDispatcher
	// 读同一个字段（data race），还会让 dispatcher 永不退出。

	n := &recordingNotifier{}
	e.AddNotifier(n)

	e.notifyBreakerTrip(core.BreakerStatus{
		Reason: "窗口内终态失败数达到阈值", Stage: "stub", Site: "stub", Window: time.Minute, Failures: 50, Threshold: 50,
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
	sites []string    // 每次调用时任务的站点，与 got 一一对应（多站同名阶段靠它分辨）
	times []time.Time // 每次调用的时刻，与 got 一一对应（验证退避节奏用）
}

func (s *recordingFetcher) GetStage() string { return s.stage }

func (s *recordingFetcher) FetchHandler(_ context.Context, task *Task, _ *Engine) error {
	s.mu.Lock()
	s.got = append(s.got, task.URL)
	s.sites = append(s.sites, task.Site)
	s.times = append(s.times, time.Now())
	s.mu.Unlock()
	return s.err
}

func (s *recordingFetcher) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.got...)
}

// calledSites 返回各次调用时任务的站点（与 calls 一一对应）。
func (s *recordingFetcher) calledSites() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sites...)
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

// stubKey 引擎测试里那条"大家都用"的阶段键：站点与阶段名都叫 stub（各用例的任务都带 Site: "stub"）。
// 键是 `site:stage` —— 阶段名只在本站内唯一，所以登记与取池都按这个复合键。
// 这里写字面量而不是 core.StageKey(...)，顺带把拼法本身也钉住。
const stubKey = "stub:stub"

// 从提交到 handler 跑完的整条链路：池子建起来、worker 认领任务、状态落库、
// 列表清理、优雅关停。这是引擎最核心的一条路，之前没有任何测试覆盖。
func TestApplyRegisterStageRunsTaskEndToEnd(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true // 全新的任务
	e := newTestEngine(t, f)

	fetcher := &recordingFetcher{stage: "stub"}
	e.AddStage("stub", StageConfig{Site: "stub", MaxAttempts: 1, WorkerCount: 1, QueueSize: 8, Delay: config.DurationRange{}}, fetcher, nil)
	e.ApplyRegisterStage()

	if e.pools.get(stubKey) != nil {
		t.Fatal("ApplyRegisterStage 不应建池（池子懒建，等第一条任务 submit 才建）")
	}

	if err := e.SubmitTask(&Task{Stage: "stub", Site: "stub", URL: "https://example.com/1"}); err != nil {
		t.Fatalf("SubmitTask = %v", err)
	}
	if e.pools.get(stubKey) == nil {
		t.Fatal("首次 submit 应懒建阶段的工作池")
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
		pools:     newPoolRegistry(),
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
		pools:     newPoolRegistry(),
	}
	e.ctx, e.cancel = context.WithCancel(context.Background())
	for _, name := range []string{"a", "b", "c"} {
		pool := workerpool.NewWorkerPool[*Task](1, 8, 1)
		e.stages[name] = &stageInfo{}
		e.pools.set(name, pool)
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
	// 假库有两个模式：noRows = 库里的行"查不到"（提交路径按全新任务走 INSERT），
	// 反之查得到（提交路径会命中去重、静默跳过）。提交那一刻要前者，回灌那一刻要后者 ——
	// 真库里两种情况都是真的（行刚 INSERT 过、当然查得到），假库不落写所以只能手动切。
	f := newFakeTaskDB()
	f.noRows = true
	e := newTestEngine(t, f)

	e.SetSiteBreaker("stub", breaker.Config{Enabled: true, Window: time.Minute, Threshold: 1000})

	fetcher := &recordingFetcher{stage: "stub"}
	e.AddStage("stub", StageConfig{Site: "stub", MaxAttempts: 1, WorkerCount: 1, QueueSize: 8}, fetcher, nil)

	if !e.PauseCrawling("测试暂停") {
		t.Fatal("手动暂停应当成功")
	}
	if !e.BreakerStatuses()["stub"].Paused {
		t.Fatal("状态快照应显示暂停中")
	}
	if e.PauseCrawling("再来一次") {
		t.Fatal("已在暂停态时再暂停应返回 false")
	}

	e.ApplyRegisterStage() // worker 一起来就停在闸门前

	// 暂停中提交：**在入队点就收口** —— 任务不入队、寄放到溢出表（行已落库、状态 pending），
	// 池子也不该被建起来（暂停期间连 worker 都不需要）。
	if err := e.SubmitTask(&Task{Stage: "stub", Site: "stub", URL: "https://example.com/paused"}); err != nil {
		t.Fatalf("SubmitTask = %v", err)
	}
	time.Sleep(80 * time.Millisecond)
	if got := fetcher.calls(); len(got) != 0 {
		t.Fatalf("暂停中不该取任务，实得 %v", got)
	}
	if got := e.spillBacklog(); got != 1 {
		t.Fatalf("暂停中的任务应当寄放在溢出表里，实得 %d", got)
	}
	if pool := e.pools.get(stubKey); pool != nil {
		t.Fatal("暂停期间不该建池（收口在建池之前）")
	}

	f.noRows = false // 回灌要按 id 回查那一行（见上面假库两种模式的说明）

	if !e.ResumeCrawling() {
		t.Fatal("放行应当成功")
	}
	// 恢复要**立刻**把寄放的任务灌回去（wakeDrainNow），不能等 drain_interval ——
	// 本夹具把它设成了 1 小时，所以这条断言就是那个唤醒机制的证明。
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
	e := &Engine{stages: map[string]*stageInfo{}, pools: newPoolRegistry()}
	fetcher := &recordingFetcher{stage: "stub"}
	e.AddStage("stub", StageConfig{Site: "stub", WorkerCount: 2, QueueSize: 4}, fetcher, nil)

	info, ok := e.stages[stubKey]
	if !ok {
		t.Fatal("阶段没登记上")
	}
	if e.pools.get(stubKey) != nil {
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
	e.AddStage("stub", StageConfig{Site: "stub",
		MaxAttempts: 1, WorkerCount: 1, QueueSize: 8,
		// 模板里的量级：catalog 的 delay 就是 5m
		Delay: config.DurationRange{Min: 5 * time.Minute, Max: 5 * time.Minute},
	}, fetcher, nil)
	e.ApplyRegisterStage()

	if err := e.SubmitTask(&Task{Stage: "stub", Site: "stub", URL: "https://example.com/1"}); err != nil {
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
	e.AddStage("stub", StageConfig{Site: "stub",
		MaxAttempts: 1, WorkerCount: 1, QueueSize: 8,
		Delay: config.DurationRange{Min: 500 * time.Millisecond, Max: 500 * time.Millisecond},
	}, fetcher, nil)
	e.ApplyRegisterStage()

	if err := e.SubmitTask(&Task{Stage: "stub", Site: "stub", URL: "https://example.com/1"}); err != nil {
		t.Fatalf("SubmitTask = %v", err)
	}
	fetcher.waitCalls(t, 1)

	// 假库上抓取本身是瞬时的，所以此刻 worker 必然已经在延迟里
	pool := e.pools.get(stubKey)
	time.Sleep(150 * time.Millisecond)
	if _, _, _, inProgress, _ := pool.Stats(); inProgress != 1 {
		t.Fatalf("延迟期间该任务应仍占着 worker，实得 inProgress=%d", inProgress)
	}
	if got := len(fetcher.calls()); got != 1 {
		t.Fatalf("延迟期间不该开始下一次抓取，实得 %d 次", got)
	}

	// 延迟走完自然收工
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, _, _, inProgress, _ := pool.Stats(); inProgress == 0 {
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
	// 两个站各声明自己的阶段：任务上的 Site 必须与声明里的 Site 对上（键是 `site:stage`）。
	// Entry: auto 不能少 —— 启动那一趟只投 `EntryAuto` 的阶段（manual 的等后台点火）。
	stageCfgA, stageCfgB := stageCfg, stageCfg
	stageCfgA.Site, stageCfgB.Site = "a", "b"
	stageCfgA.Entry, stageCfgB.Entry = EntryAuto, EntryAuto
	fetcherA := &recordingFetcher{stage: "a"}
	fetcherB := &recordingFetcher{stage: "b"}

	var submitErrs []error
	e.AddStage("a", stageCfgA, fetcherA, func(eng *Engine) {
		submitErrs = append(submitErrs, eng.SubmitTask(&Task{Stage: "b", Site: "b", URL: "https://example.com/init-b"}))
	})
	e.AddStage("b", stageCfgB, fetcherB, func(eng *Engine) {
		submitErrs = append(submitErrs, eng.SubmitTask(&Task{Stage: "a", Site: "a", URL: "https://example.com/init-a"}))
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

// 池子懒建：没建池就提交（AddStage 之后、ApplyRegisterStage 之前）不再报错，而是当场把池子建起来、
// 让任务真跑起来 —— 不再是 nil 解引用 panic，也不是「阶段不可用」报错。
func TestSubmitCreatesPoolLazily(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true // 全新任务：走 INSERT + 入队
	e := newTestEngine(t, f)

	fetcher := &recordingFetcher{stage: "stub"}
	e.AddStage("stub", StageConfig{Site: "stub", MaxAttempts: 1, WorkerCount: 1, QueueSize: 8, Delay: config.DurationRange{}}, fetcher, nil)
	// 注意：不调 ApplyRegisterStage —— 池子该由首次 submit 懒建

	if err := e.SubmitTask(&Task{Stage: "stub", Site: "stub", URL: "https://example.com/1"}); err != nil {
		t.Fatalf("首次 submit 应懒建池子并成功，实得：%v", err)
	}
	if e.pools.get(stubKey) == nil {
		t.Fatal("首次 submit 后应有活池")
	}
	fetcher.waitCalls(t, 1) // 任务真的跑起来了
}

// **这一版的核心承诺**：两个站可以各有自己的 `catalog` —— 两个池子、各收各的任务、
// 统计各占一条；同一个 URL 在两站各跑一次，谁也不会把谁顶掉。
// 之前阶段名是进程全局唯一的，这种声明在 RegisterSites 就会 panic。
func TestSameStageNameInTwoSitesIsIsolated(t *testing.T) {
	f := newFakeTaskDB()
	f.noRows = true // 全新任务：走 INSERT
	e := newTestEngine(t, f)
	e.cfg.Server.Enabled = true // 统计器跟着池子一起建（buildPool 里按这个开关）

	fetcherA := &recordingFetcher{stage: "catalog"}
	fetcherB := &recordingFetcher{stage: "catalog"}
	cfgA := StageConfig{Site: "a", MaxAttempts: 1, WorkerCount: 1, QueueSize: 8, Delay: config.DurationRange{}}
	cfgB := cfgA
	cfgB.Site = "b"

	// 阶段名一模一样，站点不同 —— 登记表、池子、统计三条都不该互相顶掉
	e.AddStage("catalog", cfgA, fetcherA, nil)
	e.AddStage("catalog", cfgB, fetcherB, nil)
	e.ApplyRegisterStage()

	// 同一个 URL：站点不同就是两条独立任务（去重键是 site:stage:url）
	const sameURL = "https://example.com/same"
	for _, site := range []string{"a", "b"} {
		if err := e.SubmitTask(&Task{Stage: "catalog", Site: site, URL: sameURL}); err != nil {
			t.Fatalf("站点 %s 投递失败：%v", site, err)
		}
	}

	poolA := e.pools.get(core.StageKey("a", "catalog"))
	poolB := e.pools.get(core.StageKey("b", "catalog"))
	if poolA == nil || poolB == nil {
		t.Fatalf("两个站都该有自己的池子，实得 a=%v b=%v", poolA, poolB)
	}
	if poolA == poolB {
		t.Fatal("两个站必须是两个池子（并发/队列/间隔各自独立）")
	}

	fetcherA.waitCalls(t, 1)
	fetcherB.waitCalls(t, 1)
	if got := fetcherA.calledSites(); len(got) != 1 || got[0] != "a" {
		t.Fatalf("a 站的 handler 只该跑 a 站的任务，实得 %v", got)
	}
	if got := fetcherB.calledSites(); len(got) != 1 || got[0] != "b" {
		t.Fatalf("b 站的 handler 只该跑 b 站的任务，实得 %v", got)
	}

	// 统计各一条：键是 `site:stage`，两段都带全（后台靠这两个字段分组与显示）
	stats := e.GetStageStats()
	for _, key := range []string{"a:catalog", "b:catalog"} {
		ss, ok := stats[key]
		if !ok {
			t.Fatalf("统计里缺 %q：%+v", key, stats)
		}
		if ss.Stage != "catalog" {
			t.Fatalf("%s 的裸阶段名应当是 catalog，实得 %q", key, ss.Stage)
		}
		if ss.Site != key[:1] {
			t.Fatalf("%s 的站点字段不对：%q", key, ss.Site)
		}
	}

	// 同一个 URL 两条任务都真的落库了（假库不解析 SQL，索引那一层要真库才验得到）
	if n := strings.Count(f.written(), "INSERT INTO `crawler_tasks`"); n != 2 {
		t.Fatalf("两个站各该插一行，实得 %d 行：\n%s", n, f.written())
	}
}

// 暂停**按站点隔离**：闸住 A 站不该影响 B 站。
//
// 这条原来测的是"每站一把闸门对象"，现在暂停只有一处（engine/pause.go 的信号表），
// 所以测的是那条信号的 scope 判定；熔断器退化成每站一个计数器（下面顺带钉住它还是每站一份）。
func TestPauseIsolatesPerSite(t *testing.T) {
	f := newFakeTaskDB()
	e := newTestEngine(t, f)

	cfg := breaker.Config{Enabled: true, Window: time.Minute, Threshold: 1}
	e.SetSiteBreaker("a", cfg)
	e.SetSiteBreaker("b", cfg)

	if e.breakerFor("a") == e.breakerFor("b") {
		t.Fatal("两个站的计数器应当各一份（失败数不能互相灌）")
	}
	if e.breakerFor("ghost") != nil {
		t.Fatal("没配过熔断的站点应当是 nil（= 没有熔断这回事），不是回落到谁身上")
	}

	if !e.PauseSite("a", "a 站被墙") {
		t.Fatal("暂停 a 站应当成功")
	}
	if e.PauseSite("a", "再来一次") {
		t.Fatal("已停着时应当返回 false（后台据此不记审计）")
	}
	if e.PauseSite("ghost", "没这个站") {
		t.Fatal("不认识的站点应当返回 false（后台据此回 400）")
	}

	// 整站暂停涵盖该站的任何阶段，且不碰别的站
	if !e.isPaused(pauseScope{Site: "a", Stage: "catalog"}) {
		t.Fatal("整站暂停应当涵盖该站所有阶段")
	}
	if e.isPaused(pauseScope{Site: "b", Stage: "catalog"}) {
		t.Fatal("a 站被闸住不该影响 b 站")
	}

	// 后台逐站看到的状态要一致（横幅与「恢复抓取」按这张表来）
	sts := e.BreakerStatuses()
	if !sts["a"].Paused || sts["b"].Paused {
		t.Fatalf("逐站状态不对：%+v", sts)
	}
	if sts["a"].Site != "a" || sts["a"].Source != PauseSourceManual {
		t.Fatalf("状态里要带上站点与来源：%+v", sts["a"])
	}
	if sts["b"].Source != "" {
		t.Fatalf("没暂停的站不该有 source：%+v", sts["b"])
	}
}

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

// 接了线、或压根没声明，都不该炸 —— 校验是给"声明了却没接线"这一种情况用的。
func TestApplyRegisterStageAcceptsWiredOrUndeclared(t *testing.T) {
	t.Run("没声明任何依赖", func(t *testing.T) {
		e := stageDepEngine(t)
		e.AddStage("stub", stageDepCfg, &recordingFetcher{stage: "stub"}, nil)
		e.ApplyRegisterStage()
	})
	t.Run("声明了下载器依赖的历史写法（钩子已删除，方法留着也不影响）", func(t *testing.T) {
		// NeedsFiledown / NeedsM3U8 随"下载器只有站点/阶段两层"一起删了：框架一定备好基座，
		// 那两条校验在结构上不可能触发。业务 fetcher 上残留的同名方法只是普通方法，不再被调用。
		e := stageDepEngine(t)
		e.AddStage("stub", stageDepCfg, &legacyNeedsDownloaders{recordingFetcher{stage: "stub"}}, nil)
		e.ApplyRegisterStage()
	})
}

// legacyNeedsDownloaders 模拟"老代码里还留着 NeedsFiledown/NeedsM3U8 方法"的 fetcher：
// 那两个接口已经不存在了，方法留着不该有任何影响（不该 panic、也不该被别人当接口用）。
type legacyNeedsDownloaders struct{ recordingFetcher }

func (s *legacyNeedsDownloaders) NeedsFiledown() bool { return true }
func (s *legacyNeedsDownloaders) NeedsM3U8() bool     { return true }

var stageDepCfg = StageConfig{Site: "stub", MaxAttempts: 1, WorkerCount: 1, QueueSize: 8, Delay: config.DurationRange{}}

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
	e.AddStage("stub", StageConfig{Site: "stub",
		MaxAttempts: 2, WorkerCount: 1, QueueSize: 8, Backoff: backoff, Delay: config.DurationRange{},
	}, fetcher, nil)
	e.ApplyRegisterStage()

	if err := e.SubmitTask(&Task{Stage: "stub", Site: "stub", URL: "https://example.com/1"}); err != nil {
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

// 熔断按站点分组：每站一把闸门，各停各的；不点名就是**全部**（后台那个「恢复抓取」）。
// 没有"默认 scope"那一档 —— 声明要有 Key（引擎这台是声明层 SetSiteBreaker 建的）、任务要有 site。
func TestBreakerScopesAreIndependent(t *testing.T) {
	f := newFakeTaskDB()
	e := siteEngine(t, f)

	// 站点 a 单开一把（阈值不同），站点 b 另一把
	e.SetSiteBreaker("a", breaker.Config{Enabled: true, Window: 5 * time.Minute, Threshold: 10})
	e.SetSiteBreaker("b", breaker.Config{Enabled: true, Window: 5 * time.Minute, Threshold: 50})

	// 两个站点都要在状态里
	sts := e.BreakerStatuses()
	for _, site := range []string{"a", "b"} {
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

	// 只闸住 a：b 照常跑
	if !e.PauseSite("a", "站点 a 被墙") {
		t.Fatal("PauseSite(a) 应当成功")
	}
	sts = e.BreakerStatuses()
	if !sts["a"].Paused || sts["b"].Paused {
		t.Fatalf("只该闸住 a：%+v", sts)
	}
	// 点名放行 a
	if !e.ResumeSite("a") {
		t.Fatal("ResumeSite(a) 应当成功")
	}
	if e.BreakerStatuses()["a"].Paused {
		t.Fatal("a 应当已放行")
	}
	// 不存在的 scope：返回 false（后台据此回 400）；空键也不是一个站点
	if e.PauseSite("ghost", "x") || e.ResumeSite("ghost") || e.PauseSite("", "x") {
		t.Fatal("不存在的 scope 应当返回 false（后台据此回 400）")
	}
	// 全部：两个一起闸住、一起放行
	if !e.PauseCrawling("手动") {
		t.Fatal("PauseCrawling 应当成功")
	}
	sts = e.BreakerStatuses()
	if !sts["a"].Paused || !sts["b"].Paused {
		t.Fatalf("PauseCrawling 应当闸住所有站点：%+v", sts)
	}
	if !e.ResumeCrawling() {
		t.Fatal("ResumeCrawling 应当成功")
	}
	sts = e.BreakerStatuses()
	if sts["a"].Paused || sts["b"].Paused {
		t.Fatalf("ResumeCrawling 应当放行所有站点：%+v", sts)
	}
}

// 熔断触发要带上站点与阶段：告警文案不能只有一句"全任务暂停"，得知道是哪个站
// （多站时这是唯一的区分），并且要**写进暂停信号**（后台横幅读的就是它）。
//
// 触发这条路原来在熔断器的构造回调里；现在它在 `notifyFailure`（终态失败那一刻）：
// 计数 → 写暂停信号 → 写成功才发告警。所以这条用例推的是 notifyFailure。
func TestBreakerTripCarriesSite(t *testing.T) {
	f := newFakeTaskDB()
	cfg := &config.Config{}
	cfg.Crawler.DrainInterval = time.Hour
	e := NewEngine(openFakeTaskDB(t, f), cfg, testLoggerSet())
	t.Cleanup(e.cancel)
	// 不用重赋 e.ctx：NewEngine 已经建好可取消的 ctx，重赋会与 delayDispatcher
	// 读同一个字段（data race），还会让 dispatcher 永不退出。

	n := &recordingNotifier{}
	e.AddNotifier(n)
	e.SetSiteBreaker("siteb", breaker.Config{Enabled: true, Window: 5 * time.Minute, Threshold: 1})

	failOnce := func() {
		e.notifyFailure(context.Background(),
			&Task{Stage: "siteb_catalog", Site: "siteb", URL: "https://example.com/x"}, errors.New("boom"))
	}
	failOnce()

	var trips []core.AlertEvent
	for _, ev := range n.snapshot() {
		if ev.Level == core.AlertCritical {
			trips = append(trips, ev)
		}
	}
	if len(trips) != 1 {
		t.Fatalf("应当发 1 条 critical 的熔断告警，实得 %d：%+v", len(trips), n.snapshot())
	}
	if trips[0].Site != "siteb" || trips[0].Stage != "siteb_catalog" {
		t.Fatalf("告警里应带站点与阶段：%+v", trips[0].TaskError)
	}

	// 同一条失败也要落到暂停信号上（Source=breaker + 现场），后台横幅/站点卡片读它
	sts := e.BreakerStatuses()
	if !sts["siteb"].Paused || sts["siteb"].Source != PauseSourceBreaker ||
		sts["siteb"].Stage != "siteb_catalog" || sts["siteb"].Failures == 0 {
		t.Fatalf("触发的现场没写进暂停信号：%+v", sts["siteb"])
	}

	// 再失败一次：已经停着 → **不重复报警**（"一次触发只报一次"现在由 setPause 的去重保证）
	failOnce()
	if got := len(trips); got != 1 {
		t.Fatalf("第二次失败不该再报一次熔断：%d", got)
	}
	for _, ev := range n.snapshot() {
		if ev.Level == core.AlertCritical {
			continue
		}
		// 非熔断的那条（任务失败本身）照旧每次都发 —— 那里不受暂停影响
	}
}
