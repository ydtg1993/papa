package engine

import (
	"context"
	"errors"
	"fmt"
	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/engine/internal/fetchruntime"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"time"
)

// attemptExecution owns exactly one invocation's trace/archive finalization,
// panic propagation, account feedback and timing/journal settlement. It never
// holds Engine. The sole callback is the public business handler invocation;
// state retries cannot invoke it again. Construction belongs to Engine.
type attemptExecution struct {
	task        *Task
	attempt     int
	trace       *Trace
	archive     *archiveBuffer
	headers     map[string]string
	persistence attemptPersistence
	resources   *fetchruntime.Resources
}

// runAttempt 执行一次 FetchHandler，并负责这一次尝试的步骤追踪与页面归档：
// 挂记录器 → 跑 handler → 记下结局 → 落盘（归档与异常 trace）。
//
// 两件事都放在 defer 里，所以 panic 展开时也会执行 —— 闭包这一层的 defer 先于 workerpool
// 的 recover 跑，panic 之前已上报的步骤因此不会丢；panic 路径上 setResult 没被调用，
// trace 按失败处理（data 保留）、归档也按失败落盘，正好是排查现场要看的。
//
// 这里 recover 一次**只是为了拿"是不是 panic"这个信号**（panic 时命名返回值 err 还是 nil），
// 拿到之后原样抛回去：workerpool 那头的 debug.Stack() 与 failed 计数才是既有行为，
// 吞掉 panic 或者重新造一个都会丢掉 handler 的栈帧。
func (a *attemptExecution) run(ctx context.Context, handler func(context.Context) error) (err error) {
	task, attempt := a.task, a.attempt
	tr := a.trace
	task.Trace = tr
	defer tr.finish() // finish 幂等，正常返回与 panic 展开都走这里

	// 归档缓冲（未开归档时是 nil，所有方法都 nil 安全）。这个 defer 注册在 tr.finish 之后，
	// 于是**先**执行（LIFO）——"这次归档了哪些文件"因此能作为一条步骤写进尚未落盘的 trace。
	ar := a.archive
	defer func() {
		r := recover()
		if files := ar.finish(r != nil || err != nil); len(files) > 0 {
			// OA 追踪抽屉里"失败"与"那一页"的连接点：光有文件、不知道属于哪条任务等于没留。
			// Trace 独立按异常/手动选择策略保留；归档始终按自己的模式执行。
			tr.Step(archiveTraceStep, map[string]any{"files": files})
		}
		// 失败原因写进 trace：handler 返回错误、或直接 panic，都在这里落一条带分类与消息的
		// 步骤。放在归档步骤之后 —— 现场在前、"为什么死"在后，读起来就是这次尝试的顺序。
		switch {
		case r != nil:
			tr.Fail(failureTraceStep, fmt.Errorf("panic: %v", r), nil)
			panic(r) // 原样抛回：workerpool 那头的栈与计数是既有行为
		case err != nil:
			if ctx.Err() == context.Canceled && cancellationOnly(err) {
				tr.setResult(nil)
			} else {
				tr.Fail(failureTraceStep, err, nil)
			}
		}
	}()
	// 缓冲挂在 ctx 上：FetchHTML 只拿得到 ctx，这是它知道"这一页属于哪次尝试"的唯一途径。
	// Trace 同样挂一份：render / CaptureRequest 的签名里没有 task，失败步骤从 ctx 取
	// （见 trace.go 的 traceCtxKey）。
	ctx = withTrace(ctx, tr)
	if identity, ok := account.AttemptFromContext(ctx); ok {
		tr.Step("账号池身份", map[string]any{"pool": identity.Pool.Name, "account": identity.Account.ID, "selection_version": identity.SelectionVersion, "credential_version": identity.CredentialVersion})
	}
	ctx = withArchive(ctx, ar)
	// 站点请求头也挂在 ctx 上：这个站的任务抓任何页面都自动带上它的 UA/Cookie/Referer，
	// handler 不用写代码；要对单次请求再改，用 `papa.WithHeaders(ctx, …)` 叠加（它只会覆盖给到的键）。
	if h := a.headers; len(h) > 0 {
		ctx = core.WithHeaders(ctx, h)
	}

	// 加急记在 trace 的第一步，事后还能看出"这条曾经加急跑过"：
	// claimTask 认领时会把 urgent 列归零（加急是「排队位置」的概念，跑过一次即完成使命），
	// 那张表上就再也看不出它加急过了。只记第一次尝试 —— 同一次执行里的后续重试不是新的加急。
	if attempt == 0 && task.Urgent {
		tr.Step(traceUrgentStep, nil)
	}

	if err = a.persistence.recordTime(ctx, task, "last_started_at"); err != nil {
		return err
	}

	defer func() {
		// Keep persistence responsibility here; failure must never replay business.
		r := recover()
		cause := err
		if r != nil {
			cause = fmt.Errorf("panic: %v", r)
		}
		_, compensation := core.CompensationOf(cause)
		terminal := cause != nil && (r != nil || ctx.Err() != nil || task.breakerFinalAttempt || !core.Retryable(cause) || account.IsAttemptFailure(cause) || compensation)
		if version, claimed := taskRunVersionFrom(ctx); claimed && task.ID != 0 && !task.breakerSampleDeadline.IsZero() && terminal && !cancellationOnly(cause) {
			// Save a known terminal result before timing/trace/archive retries.
			// Intermediate retryable attempts must never become terminal intents.
			retainCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
			writeErr := a.persistence.breakers.retainFailure(retainCtx, task, version, cause, a.persistence.state)
			cancel()
			if writeErr != nil {
				if r != nil {
					panic(r)
				}
				err = &attemptTimingError{cause: errors.Join(cause, errAttemptTimingPersistence, writeErr), handlerErr: cause}
				return
			}
		}
		// Preserve prompt account feedback, but journal terminal failures first.
		if identity, ok := account.AttemptFromContext(ctx); ok && err != nil {
			outcome := account.Unknown
			if errors.Is(err, account.ErrNeedsVerify) {
				outcome = account.NeedsVerify
			} else if errors.Is(err, account.ErrExpired) {
				outcome = account.Expired
			} else if errors.Is(err, account.ErrChallenge) {
				outcome = account.Challenge
			}
			if outcome != account.Unknown {
				if m := a.resources.Account(); m != nil && m.Pools() != nil {
					m.Pools().ReportFailure(identity, outcome, uint64(task.ID))
				}
			}
		}
		finishCtx, finishCancel := ctx, func() {}
		if !task.breakerSampleDeadline.IsZero() && ctx.Err() != nil {
			finishCtx, finishCancel = context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
		}
		writeErr := a.persistence.recordTime(finishCtx, task, "last_finished_at")
		finishCancel()
		if r != nil {
			panic(r)
		}
		if writeErr != nil {
			err = &attemptTimingError{cause: errors.Join(err, writeErr), handlerSucceeded: err == nil, handlerErr: err}
		}
	}()
	err = handler(ctx)

	tr.setResult(err)
	return err
}
