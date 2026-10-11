package engine

import (
	"errors"

	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/gorm"
)

// recoverQueueQuery 启动恢复要捞的集合：**本进程启动之前就留下的**未到终态任务（pending / processing）。
//
// 那个 `updated_at < e.startedAt` 是**必须的**，而且它与当年被删掉的那条启发式不是一回事：
//
//   - 当年那条是「早于 now-6h」——**猜**某个时长之外的 processing 是卡死的，两头不讨好：
//     崩溃前几分钟认领的孤儿捞不到（`interval: 0` 时永远挂在「处理中」），
//     而真跑得久的任务（下整部剧、大文件）过了 6h 又被当成卡死重投、与仍在跑的 worker 撞车。
//   - 现在这条是**关于本进程的事实**：`updated_at` 由 gorm 在应用侧写（autoUpdateTime），
//     认领（claimTask）也会写它 —— 于是「本进程启动之后被写过的行」恰好就是「本进程碰过的行」，
//     全部排除；而上一次运行留下的孤儿，updated_at 必然早于本次启动，照捞不误。不依赖任何时长猜测。
//
// **为什么非加不可**：`ApplyRegisterStage` 里 worker 池先起来、入口回调紧接着就投起始任务，
// 那些任务毫秒内被认领成 processing（**正在本进程里跑**），而 `startRecoverQueue` 是在这之后才起的 ——
// 没有这条截止，扫描会把它们连同"正在跑"一起捞出来重投：`requeueRecoverTask` 先把行改回 pending
// （把认领守卫赖以生效的前提抹掉），第二份随即被另一个 worker 认领 → 同一行被并发执行两遍
// （content 互相覆盖、文件重复下载、业务重复写库）。触发前提只是"那一阶段当时有空闲 worker"。
//
// 诚实说清**盖不住**的：多实例同时启动时，A 实例在 T1 起、B 在 T2(>T1) 起，A 在 (T1,T2) 之间认领的行
// 落在 B 的截止之内 → B 仍会重投它们。这是恢复队列在多实例下的固有缺口（改动前也一样），
// 要彻底解决得给行加"持有者/实例号"，不在这条谓词能表达的范围内。
//
// pending 也要捞：它们可能是上次高水位溢出到 DB 的（溢出列表在内存里，随进程一起没了），
// 也可能入库了但没来得及入队 —— 不捞就永远躺在「待处理」。
//
// `site = ?` 单独一条 Where（恢复是按站点跑的，见 ProcessRecoverQueue）。
func (e *Engine) recoverQueueQuery(site string) func() *gorm.DB {
	return func() *gorm.DB {
		return e.db.Where("status IN ?", []models.TaskStatus{
			models.TaskStatusPending,
			models.TaskStatusProcessing,
		}).Where("site = ?", site).
			// 只看"本进程启动之前就写过的行"：本进程认领/改过的行 updated_at 更晚，自动排除。
			Where("updated_at < ?", e.startedAt)
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
// 按站点跑（各自的 `enabled` / `worker_count` / `batch_size` 与 `site = ?` 查询）：
// 某站声明里把 `RecoverQueue.Enabled` 设成 false，就是"这一站不做启动恢复"。
// 全局那把锁护的是"整轮恢复"（启动那次与业务手动调它不会撞车）—— 站点之间不需要互斥，
// 恢复本来就只在启动跑一次。
func (e *Engine) ProcessRecoverQueue() (int, error) {
	e.recoverQueueMu.Lock()
	defer e.recoverQueueMu.Unlock()

	total := 0
	var errs []error
	for _, site := range e.siteKeys() {
		n, err := e.processSiteRecoverQueue(site)
		total += n
		if err != nil {
			errs = append(errs, err)
		}
	}
	return total, errors.Join(errs...)
}

// processSiteRecoverQueue 只恢复本站的「未到终态」任务（不导出：启动恢复没有手动入口，
// 见 ProcessRecoverQueue 的注释）。
func (e *Engine) processSiteRecoverQueue(site string) (int, error) {
	cfg := e.recoverQueueConfig(site)
	if !cfg.Enabled {
		return 0, nil
	}
	return e.processInBatches(e.recoverQueueQuery(site), cfg.BatchSize, cfg.WorkerCount, e.requeueRecoverTask)
}

// requeueRecoverTask 将单条未到终态的任务重置为 pending 并重新投递；提交失败则标 failed。
func (e *Engine) requeueRecoverTask(t *models.CrawlerTask) bool {
	if e.stages[t.Stage] == nil {
		e.loggerSet.Engine.Warnf("recover queue: stage %s not registered, skip task %d", t.Stage, t.ID)
		return false
	}
	// Repeatable / Urgent 照抄行上的值（taskFromRecord 已带），理由与兄弟函数 requeueFailedTask 一致：
	// 丢了 Repeatable 会让轮询任务恢复后不再参与周期轮询，丢了 Urgent 则等于悄悄吃掉加急属性。
	// 同样照抄行上的 Meta —— 那正是 handler 认业务行的依据，丢了任务就永久失败。
	task := e.taskFromRecord(t)
	// 剔除去重表暂存，再按「已入库重提交」路径重新入队。
	// 不剔的话 SubmitTask 第一道 dedupCache.Get 命中、非轮询任务会直接 return nil —— 队列根本没进。
	e.DelActiveTask(task)

	// **条件重置**（照兄弟队列的写法：repeatResetScope / requeueFailedTaskIn 都查 RowsAffected）：
	// 批次 SELECT 与这一句之间有窗口 —— 运营可能把这一行删了、标了失败，或任务自己跑完落了 success。
	// 无条件 UPDATE（以前用的 task.UpdateStatus 就是 `WHERE id = ?`）会把这些行也翻回 pending：
	// 删掉的行 UPDATE 影响 0 行、返回值还被忽略 → 后面 SubmitTask 走 Insert 把删掉的任务**重新插回来**；
	// 已终态的行被翻回 pending 再跑一遍。0 行 = 这一行已经不该被恢复：跳过，不计数、也不标 failed
	//（它不是"恢复失败"，是"轮不到它了"）。
	res := e.db.Model(&models.CrawlerTask{}).
		Where("id = ? AND site = ? AND status IN ?", t.ID, t.Site, []models.TaskStatus{
			models.TaskStatusPending,
			models.TaskStatusProcessing,
		}).
		Updates(map[string]any{"status": models.TaskStatusPending})
	if res.Error != nil {
		e.loggerSet.Engine.Errorf("recover queue: reset task %d: %s", t.ID, res.Error.Error())
		return false
	}
	if res.RowsAffected == 0 {
		return false
	}

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
	// 有没有站点要恢复：逐个站点看它自己的生效配置（全关着就不必起这个 goroutine）
	anyEnabled := false
	for _, site := range e.siteKeys() {
		if e.recoverQueueConfig(site).Enabled {
			anyEnabled = true
			break
		}
	}
	if !anyEnabled {
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
