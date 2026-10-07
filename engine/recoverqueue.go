package engine

import (
	"github.com/ydtg1993/papa/v2/models"
	"gorm.io/gorm"
)

// recoverQueueQuery 启动恢复要捞的集合：**未到终态**的任务（pending / processing）。
//
// 判据里**没有** updated_at，这是这次改动的要害：进程刚起，processing 全是上一次运行留下的孤儿，
// 这是**确定**的，不需要靠时间推测。原来那个「updated_at 早于 now-6h」反而两头不讨好 ——
//  1. 崩溃前几分钟认领的任务不满足超时条件，启动那一次根本捞不到它；
//     而 recover_queue.interval 配 0（配置注释里明确允许「仅启动时+手动触发」）时连定时轮询都没有，
//     那批任务会永远挂在「处理中」；
//  2. 真跑得久的任务（下整部剧、大文件）过了 6h 就被当成卡死重投，
//     与仍在跑的 worker 撞车写同一行 content。
//
// pending 也要捞：它们可能是上次高水位溢出到 DB 的（溢出列表在内存里，随进程一起没了），
// 也可能入库了但没来得及入队 —— 不捞就永远躺在「待处理」。
func (e *Engine) recoverQueueQuery() func() *gorm.DB {
	return func() *gorm.DB {
		return e.db.Where("status IN ?", []models.TaskStatus{
			models.TaskStatusPending,
			models.TaskStatusProcessing,
		})
	}
}

// ProcessRecoverQueue 把「未到终态」的任务重新投递回各自的阶段队列，返回实际投递成功的数量。
//
// **只该在启动时调用。** 运行期的 processing 是正在跑的任务，重投它们就是让同一行被两个 worker
// 同时写（这正是它当年按 updated_at 猜卡死时踩的坑）—— 所以它不再是「定时轮询的队列」，
// 也没有后台手动入口：启动恢复不需要按钮。
//
// 重复投递 pending 是安全的：worker 认领走的是条件更新（pending → processing），
// 两份里只有一份能认领成功，另一份拿到 0 行直接跳过。
func (e *Engine) ProcessRecoverQueue() (int, error) {
	e.recoverQueueMu.Lock()
	defer e.recoverQueueMu.Unlock()

	cfg := e.recoverQueueConfig()
	return e.processInBatches(e.recoverQueueQuery(), cfg.BatchSize, cfg.WorkerCount, e.requeueRecoverTask)
}

// requeueRecoverTask 将单条未到终态的任务重置为 pending 并重新投递；提交失败则标 failed。
func (e *Engine) requeueRecoverTask(t *models.CrawlerTask) bool {
	if e.stages[t.Stage] == nil {
		e.loggerSet.Engine.Warnf("recover queue: stage %s not registered, skip task %d", t.Stage, t.ID)
		return false
	}
	// Repeatable / Urgent 照抄行上的值（与兄弟函数 requeueFailedTask 一致）：
	// 丢了 Repeatable 会让轮询任务恢复后不再参与周期轮询，丢了 Urgent 则等于悄悄吃掉加急属性。
	task := &Task{
		ID:             int(t.ID),
		PID:            int(t.PID),
		URL:            t.URL,
		Stage:          t.Stage,
		Retry:          t.Retry,
		Repeatable:     t.Repeatable == models.RepeatableYes,
		Urgent:         t.Urgent,
		IdempotencyKey: t.IdempotencyKey,
	}
	// 剔除去重表暂存，再按「已入库重提交」路径重新入队。
	// 不剔的话 SubmitTask 第一道 dedupCache.Get 命中、非轮询任务会直接 return nil —— 队列根本没进。
	e.DelActiveTask(task)
	task.UpdateStatus(e.db, models.TaskStatusPending, nil)
	if err := e.SubmitTask(task); err != nil {
		e.loggerSet.Engine.Errorf("recover queue: submit task %d: %s", t.ID, err.Error())
		e.markRequeueFailed(recoverQueueName, t.ID, err)
		return false
	}
	e.recoveredCount.Add(1)
	return true
}

// startRecoverQueue 启动时恢复一次：把上一次运行留下的「未到终态」任务重新入队。
//
// 没有定时轮询，也没有后台手动触发 —— 理由见 ProcessRecoverQueue。异步跑，不阻塞启动。
func (e *Engine) startRecoverQueue() {
	if !e.recoverQueueConfig().Enabled {
		return
	}
	go func() {
		if n, err := e.ProcessRecoverQueue(); err != nil {
			e.loggerSet.Engine.Errorf("recover queue: startup recover: %s", err.Error())
		} else if n > 0 {
			e.loggerSet.Engine.Infof("recover queue: startup recovered %d tasks", n)
		}
	}()
}
