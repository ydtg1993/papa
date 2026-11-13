package engine

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/internal/tracestore"
)

const (
	// defaultTraceRetention 未配置保留期时的默认值。
	defaultTraceRetention = 7 * 24 * time.Hour
	// traceCleanupInterval 保留期清理的巡检间隔。
	traceCleanupInterval = time.Hour
	// maxTraceSteps ListTrace 单次返回的步骤数上限。
	maxTraceSteps = 500
	// traceUrgentStep 加急任务在 trace 里记的步骤名（引擎在第一次尝试时写，见 runAttempt）。
	traceUrgentStep = "加急执行"
	// failureTraceStep 失败尝试在 trace 里记的最后一步（引擎在 runAttempt 里写，带错误分类与消息）。
	// 没有它，抽屉里就只剩"前几步 ✔"，这次为什么死得去翻引擎日志 —— 而"为什么失败"
	// 恰恰是点开追踪最想知道的事。
	failureTraceStep = "任务失败"
)

// Trace 一次尝试（一次 FetchHandler 调用）的步骤记录器。
//
// worker 在调用 handler 前把它挂到 Task.Trace 上，handler 用 Step/Fail 逐步骤上报；
// 一次尝试结束时仅异常或手动选中的执行写入文件。
//
// **空指针安全**：trace 未开启、或测试里直接调 FetchHandler 时 Task.Trace 为 nil，
// 此时 Step/Fail/setResult/finish 都是 no-op —— 可选协作者就该是可选的。
type Trace struct {
	store        *traceService
	identityHash string
	manual       bool
	abnormal     bool
	truncated    bool
	bytes        int
	seq          int
	taskID       int
	attempt      int
	executionID  string

	mu      sync.Mutex
	steps   []core.TraceStep
	lastAt  time.Time
	succeed bool // 本次尝试的结局；只有 handler 返回 nil 才置 true
	done    bool // finish 幂等标记

}

// newTrace 构造一次尝试的记录器。
func newTrace(store *traceService, taskID, attempt int) *Trace {
	return &Trace{store: store, taskID: taskID, attempt: attempt, executionID: uuid.NewString(), lastAt: time.Now()}
}

// traceCtxKey 把本次尝试的 Trace 挂到 ctx 上 —— 与 archiveCtxKey 同一个手法。
//
// 为什么不让调用方直接读 task.Trace：`engine.render` / `engine.CaptureRequest` 这类
// 「拿了 ctx 就开始干活」的方法签名里没有 task（前者本来就不带），而它们的失败
// 恰恰最该写进 trace。ctx 是它们唯一能拿到「这次尝试属于谁」的途径。
type traceCtxKey struct{}

// withTrace 把 Trace 挂上 ctx；tr 为 nil（追踪未开启 / 没有任务行）时原样返回。
func withTrace(ctx context.Context, tr *Trace) context.Context {
	if tr == nil {
		return ctx
	}
	return context.WithValue(ctx, traceCtxKey{}, tr)
}

// traceFromCtx 取出本次尝试的 Trace；没有（未开启 / 不是从 worker 里调的）时返回 nil。
// Trace 的方法全是 nil 安全的 —— 可选协作者就该是可选的。
func traceFromCtx(ctx context.Context) *Trace {
	if tr, ok := ctx.Value(traceCtxKey{}).(*Trace); ok {
		return tr
	}
	return nil
}

// Step 记录一个**已完成**的步骤，data 可为 nil。
// 调用时机是"这一步的工作做完之后"，所以耗时 = 距上一个 Step（首步距尝试开始）的间隔。
func (t *Trace) Step(name string, data any) {
	if t == nil {
		return
	}
	payload, clipped := prepareTraceData(data)
	t.mu.Lock()
	defer t.mu.Unlock()
	t.append(name, "ok", "", "", payload, clipped)
}

// Fail 记录一个失败的步骤，并带上它的错误分类与上下文数据。
func (t *Trace) Fail(name string, err error, data any) {
	if t == nil {
		return
	}
	payload, clipped := prepareTraceData(data)
	t.mu.Lock()
	defer t.mu.Unlock()
	var msg, kind string
	if err != nil {
		msg, kind = err.Error(), ErrorKind(err)
	}
	t.append(name, "failed", kind, msg, payload, clipped)
}

// Warn 记录一个**非致命**的步骤：出了点事，但不该把任务判失败。
//
// 典型场景是"附带产物"：封面下载失败、附件没抓到、某个可选字段没解析出来 ——
// 任务本身是成功的（用 Fail 语义不对，那会把"任务失败了"的信号发出去，还会带上错误分类），
// 但"这次没拿到封面"必须留下痕迹，否则它只存在于业务自己的日志里，后台追踪上看不见。
//
// **不影响任务终态**：引擎只看 FetchHandler 的返回值，warn 只写进 trace。
// err 可为 nil（只想标一下"这一步有降级"），data 可为 nil。
func (t *Trace) Warn(name string, err error, data any) {
	if t == nil {
		return
	}
	payload, clipped := prepareTraceData(data)
	t.mu.Lock()
	defer t.mu.Unlock()
	var msg, kind string
	if err != nil {
		msg, kind = err.Error(), ErrorKind(err)
	}
	t.append(name, "warn", kind, msg, payload, clipped)
}

// append 追加一个步骤；调用方需已持锁。
const maxTraceBufferBytes = 512 << 10
const maxTraceDataBytes = 16 << 10

func traceClip(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + " [truncated]"
}

// append owns the bounded tail; callers hold mu. JSON data is bounded before
// storage; traceData bounds traversal and avoids custom marshalers.
func (t *Trace) append(name string, status string, kind, msg string, payload string, clipped bool) {
	if t.done {
		return
	}
	if status != "ok" {
		t.abnormal = true
	}
	now := time.Now()
	statusName := "ok"
	if status == "warn" {
		statusName = "warn"
	}
	if status == "failed" {
		statusName = "failed"
	}
	rec := core.TraceStep{Attempt: t.attempt, Seq: t.seq, Step: traceClip(name, 1024), Status: statusName, Kind: traceClip(kind, 256), Message: traceClip(msg, 4096), Duration: now.Sub(t.lastAt), CreatedAt: now}
	t.seq++
	t.lastAt = now
	if len(name) > 1024 || len(kind) > 256 || len(msg) > 4096 {
		t.truncated = true
	}
	rec.Data = payload
	t.truncated = t.truncated || clipped
	size := traceStepBytes(rec)
	for len(t.steps) > 0 && (len(t.steps) >= maxTraceSteps || t.bytes+size > maxTraceBufferBytes) {
		t.bytes -= traceStepBytes(t.steps[0])
		copy(t.steps, t.steps[1:])
		t.steps[len(t.steps)-1] = core.TraceStep{}
		t.steps = t.steps[:len(t.steps)-1]
		t.truncated = true
	}
	t.steps = append(t.steps, rec)
	t.bytes += size
}
func prepareTraceData(data any) (string, bool) {
	if data == nil {
		return "", false
	}
	return traceData(data)
}
func traceStepBytes(s core.TraceStep) int {
	return len(s.Step) + len(s.Kind) + len(s.Message) + len(s.Data) + 256
}

// setResult 记下本次尝试的结局：err == nil 视为成功，普通成功尝试丢弃缓冲。
// 只在 handler 正常返回时被调用 —— handler panic 时不会走到，那次尝试按失败处理，
// data 保留，正好是排查 panic 现场要看的。
func (t *Trace) setResult(err error) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.succeed = err == nil
	t.mu.Unlock()
}

// flush consumes the buffer once; clean successes are discarded.
func (t *Trace) flush() []core.TraceStep {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return nil
	}
	if len(t.steps) == 0 && t.manual {
		t.append("执行完成", "ok", "", "", "", false)
	}
	t.done = true
	records := t.steps
	t.steps = nil
	t.bytes = 0
	if t.succeed && !t.abnormal && !t.manual {
		return nil
	}
	return records
}

// finish writes a bounded attempt to files once; storage failures only log.
func (t *Trace) finish() {
	if t == nil {
		return
	}
	records := t.flush()
	if len(records) == 0 {
		return
	}
	if t.store != nil {
		t.store.write(t, records)
	}
}
func (e *Engine) traceService() *traceService {
	e.traceOnce.Do(func() {
		options := e.cfg.Crawler.TraceFiles
		if options.Dir == "" {
			options.Dir = "traces"
		}
		e.traces = &traceService{files: tracestore.New(tracestore.Options{Dir: options.Dir, MaxFileBytes: options.MaxFileBytes, MaxTotalBytes: options.MaxTotalBytes, MaxFiles: options.MaxFiles}), armed: make(map[int]time.Time)}
		if e.loggerSet != nil {
			e.traces.logger = e.loggerSet.Engine
		}
	})
	return e.traces
}
func (e *Engine) traceEnabled() bool { return e.cfg.Crawler.Trace.Enabled }
func (e *Engine) newTrace(task *Task, attempt int) *Trace {
	if !e.traceEnabled() || task == nil || task.ID <= 0 {
		return nil
	}
	tr := newTrace(e.traceService(), task.ID, attempt)
	tr.identityHash = task.Unique()
	if tr.identityHash == "" {
		tr.identityHash = fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%d:%s:%s:%s", task.ID, task.Site, task.Stage, task.URL))))
	}
	return tr
}

// ListTrace returns ordered steps for the monitoring drawer.
func (e *Engine) ListTrace(taskID int) ([]TraceStep, error) {
	detailed, err := e.ListTraceExecution(taskID)
	if err != nil {
		return nil, err
	}
	steps := make([]TraceStep, len(detailed))
	for i := range detailed {
		steps[i] = detailed[i].TraceStep
	}
	return steps, nil
}

// ListTraceExecution returns the latest persisted execution tail with identity
// and truncation metadata. ListTrace retains its original value layout.
func (e *Engine) ListTraceExecution(taskID int) ([]TraceExecutionStep, error) {
	if !e.traceEnabled() {
		return nil, fmt.Errorf("步骤追踪未开启（crawler.trace.enabled）")
	}
	return e.traceService().files.List(context.Background(), taskID, maxTraceSteps)
}
func (e *Engine) traceRetention() time.Duration {
	r := e.cfg.Crawler.Trace.Retention
	if r < 0 {
		return 0
	}
	if r == 0 {
		return defaultTraceRetention
	}
	return r
}
func (e *Engine) startTraceCleanup() {
	if !e.traceEnabled() {
		return
	}
	retention := e.traceRetention()
	if retention <= 0 {
		e.loggerSet.Engine.Infof("trace cleanup disabled: retention is negative (keep forever)")
		return
	}
	startEvidenceMaintenance(e.ctx, &e.tickerWG, traceCleanupInterval, func(ctx context.Context) { e.traceService().cleanup(ctx, retention) })
}
func (e *Engine) cleanupTrace(retention time.Duration) { e.traceService().cleanup(e.ctx, retention) }

// Automatic transport diagnostics do not promote orderly cancellation into an
// anomaly. Explicit handler Fail/Warn calls always retain their evidence.
func traceFrameworkFailure(ctx context.Context, tr *Trace, cause error, name string, display error, data any) {
	if ctx.Err() == context.Canceled && cancellationOnly(cause) {
		return
	}
	tr.Fail(name, display, data)
}
