package engine

import (
	"context"
	"crypto/sha256"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/datatypes"
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
// 一次尝试结束时一次性批量写文件（一次原子发布），不逐步骤执行文件 I/O。
//
// **空指针安全**：trace 未开启、或测试里直接调 FetchHandler 时 Task.Trace 为 nil，
// 此时 Step/Fail/setResult/finish 都是 no-op —— 可选协作者就该是可选的。
type Trace struct {
	store        *traceService
	executionID  string
	identityHash string
	truncated    bool
	bytes        int
	seq          int
	logger       *logrus.Logger
	taskID       int
	attempt      int

	mu      sync.Mutex
	steps   []models.TaskTrace
	lastAt  time.Time
	succeed bool // 本次尝试的结局；只有 handler 返回 nil 才置 true
	done    bool // finish 幂等标记
}

// newTrace 构造一次尝试的记录器。
func newTrace(store *traceService, logger *logrus.Logger, taskID, attempt int) *Trace {
	return &Trace{store: store, logger: logger, taskID: taskID, attempt: attempt, executionID: uuid.NewString(), identityHash: fmt.Sprint(taskID), lastAt: time.Now()}
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
	payload := tracePayload{}
	if data != nil {
		payload.data, payload.truncated = traceData(data)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.append(name, models.TraceOK, "", "", payload)
}

// Fail 记录一个失败的步骤，并带上它的错误分类与上下文数据。
func (t *Trace) Fail(name string, err error, data any) {
	if t == nil {
		return
	}
	payload := tracePayload{}
	if data != nil {
		payload.data, payload.truncated = traceData(data)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var msg, kind string
	if err != nil {
		msg, kind = err.Error(), ErrorKind(err)
	}
	t.append(name, models.TraceFailed, kind, msg, payload)
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
	payload := tracePayload{}
	if data != nil {
		payload.data, payload.truncated = traceData(data)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var msg, kind string
	if err != nil {
		msg, kind = err.Error(), ErrorKind(err)
	}
	t.append(name, models.TraceWarn, kind, msg, payload)
}

// append 追加一个步骤；调用方需已持锁。
func (t *Trace) append(name string, status models.TraceStatus, kind, msg string, data any) {
	if t.done {
		return
	}
	now := time.Now()
	rec := models.TaskTrace{
		TaskID:    uint(t.taskID),
		Attempt:   t.attempt,
		Seq:       t.seq,
		CreatedAt: now,
		Step:      traceClip(name, 1024),
		Status:    status,
		Kind:      traceClip(kind, 256),
		Message:   traceClip(msg, 4096),
		Duration:  now.Sub(t.lastAt),
	}
	t.lastAt = now
	if payload, ok := data.(tracePayload); ok {
		rec.Data = datatypes.JSON(payload.data)
		t.truncated = t.truncated || payload.truncated
	}
	t.seq++
	t.truncated = t.truncated || len(name) > 1024 || len(kind) > 256 || len(msg) > 4096
	size := traceRecordBytes(rec)
	for len(t.steps) > 0 && (len(t.steps) >= maxTraceSteps || t.bytes+size > maxTraceBufferBytes) {
		t.bytes -= traceRecordBytes(t.steps[0])
		copy(t.steps, t.steps[1:])
		t.steps[len(t.steps)-1] = models.TaskTrace{}
		t.steps = t.steps[:len(t.steps)-1]
		t.truncated = true
	}
	t.steps = append(t.steps, rec)
	t.bytes += size
}

// setResult 记下本次尝试的结局：err == nil 视为成功，成功尝试写文件时不写 data。
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

// flush 取出本次尝试要写文件的记录并清空缓冲：成功尝试剥离 data（写入量按失败率走）。
// 幂等 —— 已 flush 过时返回 nil；空尝试记录完成标记，避免展示上一轮旧记录。
func (t *Trace) flush() []models.TaskTrace {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done {
		return nil
	}
	if len(t.steps) == 0 {
		t.append("执行完成", models.TraceOK, "", "", nil)
	}
	t.done = true
	records := t.steps
	t.steps = nil
	if t.succeed {
		for i := range records {
			// 例外：非致命步骤的 data **保留**。它记的正是"任务成功了、但这一步降级了"这件事，
			// 而这条信息没有第二次机会 —— 下一个成功的尝试里它照样只是 warn，不会变成一次
			// 失败的尝试来把 data 带上（剥掉它等于把 warn 的 payload 永远丢掉）。
			// 量不成问题：warn 按定义是异常，不是每步都有。
			if records[i].Status == models.TraceWarn {
				continue
			}
			records[i].Data = nil
		}
	}
	return records
}

// finish publishes one immutable attempt file; file errors do not change task outcome.
func (t *Trace) finish() {
	if t == nil {
		return
	}
	records := t.flush()
	if len(records) > 0 && t.store != nil {
		t.store.write(t, records)
	}
}

// traceEnabled 返回步骤追踪是否开启。
func (e *Engine) traceEnabled() bool {
	return e.cfg.Crawler.Trace.Enabled
}

// newTrace 为一次尝试构造记录器；追踪关闭或任务未写文件（没有任务行可挂）时返回 nil。
func (e *Engine) newTrace(task *Task, attempt int) *Trace {
	if !e.traceEnabled() || task.ID == 0 {
		return nil
	}
	if attempt == 0 || task.traceExecutionID == "" {
		task.traceExecutionID = uuid.NewString()
	}
	tr := newTrace(e.traceService(), e.loggerSet.Engine, task.ID, attempt)
	tr.executionID = task.traceExecutionID
	tr.identityHash = fmt.Sprintf("%x", sha256.Sum256([]byte(task.Unique())))
	return tr
}

// ListTrace returns the latest persisted execution, across its retries.
func (e *Engine) ListTrace(taskID int) ([]TraceStep, error) {
	rows, err := e.ListTraceExecution(taskID)
	if err != nil {
		return nil, err
	}
	out := make([]TraceStep, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.TraceStep)
	}
	return out, nil
}
func (e *Engine) ListTraceExecution(taskID int) ([]core.TraceExecutionStep, error) {
	if !e.traceEnabled() {
		return nil, fmt.Errorf("步骤追踪未开启（crawler.trace.enabled）")
	}
	if taskID <= 0 {
		return nil, fmt.Errorf("invalid trace task ID")
	}
	return e.traceService().files.List(context.Background(), taskID, maxTraceSteps)
}

// traceRetention 返回生效的保留期；<=0（未配置）用默认值，显式负数表示不自动清理。
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

// startTraceCleanup 启动保留期清理巡检：删除过期追踪文件。
// 追踪关闭或保留期为负（显式要求永久保留）时不启动。
func (e *Engine) startTraceCleanup() {
	if !e.traceEnabled() {
		return
	}
	retention := e.traceRetention()
	if retention <= 0 {
		e.loggerSet.Engine.Infof("trace cleanup disabled: retention is negative (keep forever)")
		return
	}
	// 进 tickerWG：清理访问文件存储，关停等待其结束。
	e.tickerWG.Add(1)
	go func() {
		defer e.tickerWG.Done()
		e.cleanupTrace(retention)
		ticker := time.NewTicker(traceCleanupInterval)
		defer ticker.Stop()
		for {
			select {
			case <-e.ctx.Done():
				return
			case <-ticker.C:
				e.cleanupTrace(retention)
			}
		}
	}()
}

// cleanupTrace removes expired files, never task rows.
func (e *Engine) cleanupTrace(retention time.Duration) {
	e.traceService().cleanup(e.lifecycleContext(), retention)
}

const maxTraceBufferBytes = 512 << 10
const maxTraceDataBytes = 16 << 10

func traceClip(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	return s[:limit] + " [truncated]"
}
func traceRecordBytes(r models.TaskTrace) int {
	return len(r.Step) + len(r.Kind) + len(r.Message) + len(r.Data) + 256
}

type tracePayload struct {
	data      string
	truncated bool
}
