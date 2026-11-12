package engine

import (
	"context"
	"sync"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/v4/models"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

const (
	// defaultTraceRetention 未配置保留期时的默认值。
	defaultTraceRetention = 7 * 24 * time.Hour
	// traceCleanupInterval 保留期清理的巡检间隔。
	traceCleanupInterval = time.Hour
	// traceDeleteBatch 每次 DELETE 的行数上限，避免长事务锁表。
	traceDeleteBatch = 10000
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
// 一次尝试结束时一次性批量落库（一条多行 INSERT），不是每步一条 INSERT。
//
// **空指针安全**：trace 未开启、或测试里直接调 FetchHandler 时 Task.Trace 为 nil，
// 此时 Step/Fail/setResult/finish 都是 no-op —— 可选协作者就该是可选的。
type Trace struct {
	store   traceService
	taskID  int
	attempt int

	mu           sync.Mutex
	steps        []models.TaskTrace
	lastAt       time.Time
	succeed      bool         // 本次尝试的结局；只有 handler 返回 nil 才置 true
	done         bool         // finish 幂等标记
	referenceSeq map[int]bool // small framework-owned evidence references survive success
}

// newTrace 构造一次尝试的记录器。
func newTrace(db *gorm.DB, logger *logrus.Logger, taskID, attempt int) *Trace {
	return &Trace{store: traceService{db: db, dbLogger: logger}, taskID: taskID, attempt: attempt, lastAt: time.Now()}
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
	t.mu.Lock()
	defer t.mu.Unlock()
	t.append(name, models.TraceOK, "", "", data)
}

// Fail 记录一个失败的步骤，并带上它的错误分类与上下文数据。
func (t *Trace) Fail(name string, err error, data any) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var msg, kind string
	if err != nil {
		msg, kind = err.Error(), ErrorKind(err)
	}
	t.append(name, models.TraceFailed, kind, msg, data)
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
	t.mu.Lock()
	defer t.mu.Unlock()
	var msg, kind string
	if err != nil {
		msg, kind = err.Error(), ErrorKind(err)
	}
	t.append(name, models.TraceWarn, kind, msg, data)
}

// append 追加一个步骤；调用方需已持锁。
func (t *Trace) append(name string, status models.TraceStatus, kind, msg string, data any) {
	now := time.Now()
	rec := models.TaskTrace{
		TaskID:   uint(t.taskID),
		Attempt:  t.attempt,
		Seq:      len(t.steps),
		Step:     name,
		Status:   status,
		Kind:     kind,
		Message:  msg,
		Duration: now.Sub(t.lastAt),
	}
	t.lastAt = now
	if data != nil {
		// 序列化失败不静默丢：写一条可读的标记进去，免得排查时以为 handler 没上报 data。
		// 与 content 列同一条约定：JSON 列不做 HTML 转义，否则步骤里的 HTML 片段是一片 <。
		if b, err := marshalNoHTMLEscape(data); err == nil {
			rec.Data = datatypes.JSON(b)
		} else {
			marker, _ := marshalNoHTMLEscape(map[string]string{"_marshal_error": err.Error()})
			rec.Data = datatypes.JSON(marker)
		}
	}
	t.steps = append(t.steps, rec)
}

// setResult 记下本次尝试的结局：err == nil 视为成功，成功尝试落库时不写 data。
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

// flush 取出本次尝试要落库的记录并清空缓冲：成功尝试剥离 data（写入量按失败率走）。
// 幂等 —— 已 flush 过、或本次尝试一个步骤都没上报时返回 nil。
func (t *Trace) flush() []models.TaskTrace {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.done || len(t.steps) == 0 {
		t.done = true
		return nil
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
			if records[i].Status == models.TraceWarn || t.referenceSeq[records[i].Seq] {
				continue
			}
			records[i].Data = nil
		}
	}
	return records
}

// finish 把本次尝试的步骤一次性落库（一条多行 INSERT）。幂等：正常路径调一次、
// defer 兜底再调一次也不会重复写（所以 defer 可以无脑挂）。
// 写库失败只记日志，不影响任务成败 —— 最常见的原因是开关开着但表没建，
// 那种情况由 app.NewApp 启动时的缺表告警负责喊出来。
func (t *Trace) finish() {
	if t == nil {
		return
	}
	records := t.flush()
	if len(records) == 0 {
		return
	}
	t.store.write(records, t.taskID, t.attempt)
}
func (e *Engine) traceService() traceService {
	service := traceService{options: e.cfg.Crawler.Trace, db: e.db}
	if e.loggerSet != nil {
		service.dbLogger, service.logger = e.loggerSet.DB, e.loggerSet.Engine
	}
	return service
}
func (e *Engine) traceEnabled() bool { return e.cfg.Crawler.Trace.Enabled }
func (e *Engine) newTrace(task *Task, attempt int) *Trace {
	if !e.traceEnabled() {
		return nil
	}
	return e.traceService().newTrace(task.ID, attempt)
}

// ListTrace returns ordered steps for the monitoring drawer.
func (e *Engine) ListTrace(taskID int) ([]TraceStep, error) { return e.traceService().list(taskID) }
func (e *Engine) traceRetention() time.Duration {
	return (traceService{options: e.cfg.Crawler.Trace}).retention()
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
