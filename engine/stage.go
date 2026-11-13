package engine

import (
	"context"
	"errors"
	"fmt"
	"github.com/ydtg1993/papa/v4/internal/taskstore"
	"time"

	"github.com/ydtg1993/papa/v4/core"
	"github.com/ydtg1993/papa/v4/engine/internal/execution"
	"github.com/ydtg1993/papa/v4/internal/workerpool"
	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/events"
)

// finishTask 任务落终态后的统一收尾：非轮询任务清活动去重键，轮询任务唤醒轮询队列。
//
// 两条不能合并成"总是清理 + 总是唤醒"：非轮询任务不该占着去重表（否则同一 URL 再也进不来），
// 轮询任务则要留在去重表里等 repeat_queue 重新捞；唤醒只对轮询任务有意义（它有下次到点，
// 落终态后 soonestRepeatIn 要立刻把它算进节拍，否则 ticker 还睡在上一次算出的全局 interval 上）。
func (e *Engine) finishTask(task *Task) {
	if task.FlowID != "" {
		e.wakeDrainNow()
	}
	if task.Repeatable {
		e.wakeRepeatQueue()
		return
	}
	e.DelActiveTask(task)
}

// releaseClaim 把「本进程认领过、但没跑完就遇上下线」的任务退回「待处理」。
//
// 停机路径上 WorkerPool.Stop 会把两条队列的存量也交给 handler 排水
// （workerpool.run 的注释解释了 worker 循环里为什么不能 select ctx.Done）——
// 这些任务一旦被认领，后面的每一步（fetch / 退避 / 落终态）都会被已取消的 ctx 打断，
// 而 stageHandler 在那些分支上只能直接返回；不在这里回退，行就永久停在「处理中」。
// （2026-10-29 实测：14:02 停机后库里留下 1004 条 status=1，trace 全是
// retryable/context canceled；重启后虽有启动恢复兜底，但那一小时里报告与后台
// 都会先把它们当「卡死」。）
//
// 条件更新 `id=? AND status=processing`：只回退「还停在处理中」的这一行 ——
// 这中间被运营标成功/标失败/删除、或已被重启后的启动恢复重投过的行，一律不覆盖；
// RowsAffected=0 不是错误（它本来就是"轮不到回退"）。
// 写库失败记录 Warn 和未确认任务：停机不无限等待，但 Stop 不能谎报已排空。
// 行回到 pending 就够了 —— 下次启动的启动恢复（recoverqueue.go）会重新入队。
func (e *Engine) releaseClaim(task *Task) {
	// A cancellation racing terminal cleanup cannot turn a journaled failure
	// back into pending. The report writer owns its remaining settlement.
	e.breakers.breakerMu.Lock()
	pending := e.breakers.ownsFailureSettlementLocked(task)
	e.breakers.breakerMu.Unlock()
	if pending {
		return
	}

	if task.ID == 0 {
		return
	}
	res := taskstore.Release(e.db, uint(task.ID))
	if res.Error != nil {
		e.loggerSet.Engine.Warnf("release claim task %d: %s", task.ID, res.Error.Error())
		e.rememberUnsettled(task)
	} else if res.RowsAffected == 1 {
		e.forgetUnsettled(task)
	}
}

// stageHandler 一个阶段 worker 的任务处理函数：认领 → 带重试地跑 handler → 落终态。
// 从 ApplyRegisterStage 的第一趟提出来：池子懒建（首次 submit 才建），建的时候绑上同一个 handler。
func (e *Engine) stageHandler(info *stageInfo) workerpool.TaskHandler[*Task] {
	cfg := info.config
	// 尝试次数兜底为 1：声明层（planStage）已经保证 > 0，但直接调 AddStage 的用法可以传 0 ——
	// 那一档的循环一次都不跑，然后拿着**还是 nil** 的 lastErr 去落 failed、进 notifyFailure
	//（那里会 err.Error()），当场空指针 panic。只兜"会崩"的这一类，不动其它字段的语义
	//（与下载器那边 max(MaxConcurrent, 1) 同一路数）。
	attempts := max(cfg.MaxAttempts, 1)
	return func(ctx context.Context, task *Task) error {
		e.residents.move(task, "running")
		defer e.residents.release(task, "running")
		// 停机时的排水（WorkerPool.Stop → drainAndExit）会把队列里**还没开始**的存量任务
		// 也原样交给 handler，而它拿到的 ctx 在进 Stop 那一刻就取消了
		//（Engine.Stop 先 e.cancel() 再 pool.Stop，所以 workerpool.run 才故意不 select ctx.Done）。
		// 这时候认领没有任何意义：fetch 一出手就被 ctx 打断，回头只会在库里留一行「处理中」。
		// 所以**不认领、直接放弃**：行仍是 pending，下次启动的启动恢复会重新入队
		//（recoverqueue.go 的 recoverQueueQuery 捞的正是 pending/processing）。
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil
		}
		if e.residents != nil && e.isPaused(pauseScope{Site: task.Site, Stage: task.Stage}) {
			e.spillTask(task)
			return nil
		}
		breakerRelease, admitted := e.breakers.admitBreakerTask(task)
		if !admitted {
			e.spillTask(task)
			return nil
		}
		defer breakerRelease()
		if !task.breakerSampleDeadline.IsZero() {
			var cancel context.CancelFunc
			ctx, cancel = context.WithDeadline(ctx, task.breakerSampleDeadline)
			defer cancel()
		}
		// Serial ownership and its deadline include resource/governor waits.
		var release func()
		var admissionErr error
		ctx, release, admissionErr = e.prepareRuntimeTask(ctx, task, cfg.WorkerCount)
		if admissionErr != nil {
			if ctx.Err() == nil {
				e.spillTask(task)
			}
			return admissionErr
		}
		defer release()
		// A task can spend time waiting after the worker's original pause gate.
		// Recheck before claiming so a pause during that wait keeps it pending.
		if e.isPaused(pauseScope{Site: task.Site, Stage: task.Stage}) {
			release()
			e.spillTask(task)
			return nil
		}
		// claimedAt 认领时刻：事件里的 duration_ms 用它算整段在途时间（成功/失败都算）。
		var claimedAt time.Time
		var runVersion taskRunVersion
		// panic 收尾：FetchHandler 一次尝试里 panic 会展开跳过下面整段「重试 + 落终态」。
		// runAttempt 只负责 trace/归档（记完原样抛回），workerpool 只负责计数 —— 谁都不碰任务行，
		// 于是行会永远停在 processing、错误队列与失败通知都不触发、活动任务也没清掉。
		// 这里补上终态落库 + 失败通知 + 清活动任务，栈照旧抛回给 workerpool（见 runAttempt 的注释）。
		defer func() {
			if r := recover(); r != nil {
				err := fmt.Errorf("panic: %v", r)
				if !claimedAt.IsZero() {
					if writeErr := e.persistTerminal(ctx, task, runVersion, models.TaskStatusFailed, err); writeErr != nil {
						e.loggerSet.Engine.Errorf("panic task %d terminal write unconfirmed: %s", task.ID, writeErr)
					} else {
						e.emitTaskFailed(task, err, claimedAt)
						e.notifyFailure(ctx, task, err)
						e.finishTask(task)
					}
				}
				panic(r)
			}
		}()
		// 认领：把「待处理」置为「处理中」，让运营侧能看出这条归谁管，
		// 也让三个后台动作的状态守卫成立。拿不到行就跳过执行 —— 原因不止一种：
		// 运营已经动过它（标失败/删除），或同一行的另一份副本（后台「加急」会另投一份）先认领了。
		claimed, version, err := e.claimForExecution(ctx, task)
		if err != nil {
			return err
		} else if !claimed {
			e.loggerSet.Engine.Debugf("task %d 已被认领或改动（不再是待处理），跳过本次执行", task.ID)
			return nil
		}
		claimedAt = time.Now()
		runVersion = version
		task.breakerRound = fmt.Sprintf("%d:%d", version.Repeat, version.Reprocess)
		ctx = withTaskRunVersion(ctx, version)
		ctx = withEvidenceExecution(ctx)
		if err := e.persistAccountAttempt(ctx, task, version); err != nil {
			e.releaseClaim(task)
			return err
		}
		e.emit(events.Event{
			Type: events.TypeTaskClaimed, Site: task.Site, Stage: task.Stage,
			TaskID: uint(task.ID), URL: task.URL, Attempt: 0,
		})
		// 重试FetchHandler
		var lastErr error
		for attempt := 0; attempt < attempts; attempt++ {
			// 首次尝试之前、以及每次重试之前再看一眼 ctx：认领与 fetch 之间还夹着
			// 事件、退避、追踪这些步骤，停机就可能落在这一段里。此时同样不能把行留在
			// processing —— 先回退成 pending，再如实返回 ctx 的错误。
			if ctxErr := ctx.Err(); ctxErr != nil {
				e.releaseClaim(task)
				return ctxErr
			}
			if attempt > 0 {
				// 退避放在**下一次尝试之前**（与 m3u8 / filedown 的退避同一写法）。
				// 它不能写在上一轮的末尾：那样最后一次尝试失败之后还要再睡满一个周期才落 failed，
				// 而那时已经不会再试了 —— worker 只是抱着一个并发位白等
				//（3 次尝试、30s 退避就是 120s；实测一个注定失败的 detail 任务从认领到终态 210s）。
				select {
				case <-ctx.Done():
					// 退避被停机打断：不是任务失败，回退认领后退出。
					e.releaseClaim(task)
					return ctx.Err()
				case <-time.After(cfg.Backoff * (1 << uint(attempt-1))):
				}
				if err := e.incrementExecutionRetry(ctx, task); err != nil {
					return err
				}
				e.emit(events.Event{
					Type: events.TypeTaskRetry, Site: task.Site, Stage: task.Stage,
					TaskID: uint(task.ID), Attempt: attempt,
				})
			}
			task.breakerFinalAttempt = attempt == attempts-1
			err := e.runAttempt(ctx, info.fetcher, task, attempt)
			if errors.Is(err, errAttemptTimingPersistence) {
				var timingErr *attemptTimingError
				if !task.breakerSampleDeadline.IsZero() && errors.As(err, &timingErr) && timingErr.handlerErr != nil && !cancellationOnly(timingErr.handlerErr) {
					terminalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
					defer cancel()
					if writeErr := e.persistTerminal(terminalCtx, task, runVersion, models.TaskStatusFailed, timingErr.handlerErr); writeErr != nil {
						return errors.Join(err, writeErr)
					}
					e.emitTaskFailed(task, timingErr.handlerErr, claimedAt)
					e.notifyFailure(terminalCtx, task, timingErr.handlerErr)
					e.finishTask(task)
					return err
				}
				if ctx.Err() != nil && (!errors.As(err, &timingErr) || !timingErr.handlerSucceeded) {
					e.releaseClaim(task)
					return ctx.Err()
				}
				return err
			}
			if err == nil {
				// Preserve confirmed business success across shutdown cancellation.
				// Version guards still reject stale rounds; late breaker proof is
				// independently rejected by the breaker coordinator.
				writeErr := execution.ConfirmSuccess(ctx, func(settleCtx context.Context) error {
					return e.persistTerminal(settleCtx, task, runVersion, models.TaskStatusSuccess, nil)
				})
				if writeErr != nil {
					return writeErr
				}
				e.recordSmartOutcome(task, nil)
				breakerRelease()
				e.emit(events.Event{
					Type: events.TypeTaskFinished, Site: task.Site, Stage: task.Stage,
					TaskID: uint(task.ID), Outcome: "success", Attempt: attempt,
					DurationMS: elapsedMS(claimedAt),
				})
				e.finishTask(task)
				release()
				// 任务间隔延迟：让 worker 在两次抓取之间歇一下，别把目标站打急。
				// **必须可被 ctx 打断** —— 任务到这里已经成功、状态也已经落库，
				// 再让 worker 抱着这个并发位睡满一个 delay（模板里 catalog 是 5m），
				// 只会让 Engine.Stop（默认只等 stop_timeout=5s）报"未排空"、
				// 连带跳过 app.Run 里的关库收尾。停机时直接跳过这段休息。
				// 与上面重试退避那段同一个写法（Engine.Stop 的注释承诺的就是这个）。
				select {
				case <-ctx.Done():
				case <-time.After(e.taskDelay(task, cfg.Delay.Random())):
				}
				return nil
			}
			// 抓取返回错误后先看 ctx、再判错误：停机打断的那一次尝试不是「任务失败」——
			// 它的错就是 context canceled，按 core.Retryable 会判真、随后在退避 select 里退出，
			// 行就永远停在 processing。这里先回退成 pending，再如实返回 ctx 的错误；
			// 不可重试分支也必须在它之后，否则一次停机取消会被当成「页面结构变了」记进 error 列。
			if ctxErr := ctx.Err(); ctxErr != nil {
				if !task.breakerSampleDeadline.IsZero() && !cancellationOnly(err) {
					// The handler returned a real error after cancellation. Persist
					// that terminal round without replaying business or releasing the
					// serial slot before persistence and notification have finished.
					terminalCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Second)
					defer cancel()
					if writeErr := e.persistTerminal(terminalCtx, task, runVersion, models.TaskStatusFailed, err); writeErr != nil {
						return errors.Join(err, writeErr)
					}
					e.emitTaskFailed(task, err, claimedAt)
					e.notifyFailure(terminalCtx, task, err)
					e.finishTask(task)
					return err
				}
				e.releaseClaim(task)
				return ctxErr
			}
			// 不可重试的错误：直接标 failed，不再空转重试
			_, hasCompensationPolicy := core.CompensationOf(err)
			if !core.Retryable(err) || account.IsAttemptFailure(err) || hasCompensationPolicy {
				if writeErr := e.persistTerminal(ctx, task, runVersion, models.TaskStatusFailed, err); writeErr != nil {
					return errors.Join(err, writeErr)
				}
				e.emitTaskFailed(task, err, claimedAt)
				e.notifyFailure(ctx, task, err)
				e.finishTask(task)
				return fmt.Errorf("任务处理失败 task ID:%d	,error: %w", task.ID, err)
			}
			lastErr = err
		}
		// 重试耗尽的那一刻也可能正好赶上停机：这时落 failed 会把「没跑完」错记成
		// 「任务失败」（error 列还会多一条 context canceled）。同样回退成 pending。
		if ctxErr := ctx.Err(); ctxErr != nil {
			e.releaseClaim(task)
			return ctxErr
		}
		// 所有重试失败：记录错误并更新状态为 failed
		if writeErr := e.persistTerminal(ctx, task, runVersion, models.TaskStatusFailed, lastErr); writeErr != nil {
			return errors.Join(lastErr, writeErr)
		}
		e.emitTaskFailed(task, lastErr, claimedAt)
		e.notifyFailure(ctx, task, lastErr)
		e.finishTask(task)
		return fmt.Errorf("任务处理失败 task ID:%d	,error: %w", task.ID, lastErr)
	}
}

// runAttempt assembles one execution session; the public Fetcher API still
// receives Engine, but no runtime component retains it as a dependency.
func (e *Engine) runAttempt(ctx context.Context, fetcher Fetcher, task *Task, attempt int) error {
	executionID := evidenceExecutionID(ctx)
	tr, ar := e.newTrace(task, attempt), e.newArchiveBuffer(task, attempt)
	if tr != nil {
		tr.executionID = executionID
	}
	if ar != nil {
		ar.executionID = executionID
	}
	session := attemptExecution{task: task, attempt: attempt, trace: tr, archive: ar, headers: e.siteHeaders(task.Site), resources: &e.fetchResources,
		persistence: attemptPersistence{db: e.db, state: &e.execution, breakers: &e.breakers, logger: e.loggerSet.Engine}}
	return session.run(ctx, func(ctx context.Context) error { return fetcher.FetchHandler(ctx, task, e) })
}

// A joined cancellation and business error is still a real failure. Wrapping
// cancellation alone (including HTTP/url wrappers) must not inflate failures.
func cancellationOnly(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		found := false
		for _, child := range joined.Unwrap() {
			if child != nil {
				found = true
				if !cancellationOnly(child) {
					return false
				}
			}
		}
		return found
	}
	if wrapped := errors.Unwrap(err); wrapped != nil {
		return cancellationOnly(wrapped)
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
