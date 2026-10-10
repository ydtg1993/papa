package engine

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/gorm"
)

// errorQueueState 一个站点（或默认 scope）的错误队列运行态：自己的锁与累计计数。
//
// 与轮询队列那份同构（见 repeatQueueState），只差**没有唤醒通道** —— 队列参数现在只在启动时
// 读一次（改配置 = 改代码 + 重启），没有"数据变了要立刻重算节拍"这回事。
type errorQueueState struct {
	mu      sync.Mutex
	retried atomic.Int64 // 本站累计重新投递数（同时就是监控里那一行的"处理量"）
}

// errorQueueKey 按站点的错误队列名 —— 它同时是监控快照的 key（后台一行一个站）。
// 默认 scope 保持历史名字 `error_queue`，命名站点是 `error_queue:<站点>`。
func errorQueueKey(site string) string {
	if site == "" {
		return QueueError
	}
	return QueueError + ":" + site
}

// errorQueueSite 从队列名反解站点。站点名必须是**登记过的** —— 与 repeatQueueSite 同一约定
// （打错的站名不该在后台多出一行"看起来开着"的假队列）。
func (e *Engine) errorQueueSite(name string) (string, bool) {
	if name == QueueError {
		return "", true
	}
	site, found := strings.CutPrefix(name, QueueError+":")
	if !found || site == "" {
		return "", false
	}
	if _, ok := e.Site(site); !ok {
		return "", false
	}
	return site, true
}

// errorQueue 取某队列名对应的运行态（没登记返回 nil）。
func (e *Engine) errorQueue(name string) *errorQueueState { return e.errorQueues[name] }

// errorQueueSites 返回所有已登记的错误队列站点：默认 scope 在最前，其余按站点名字典序。
func (e *Engine) errorQueueSites() []string {
	sites := make([]string, 0, len(e.errorQueues))
	for name := range e.errorQueues {
		if site, ok := e.errorQueueSite(name); ok {
			sites = append(sites, site)
		}
	}
	sort.Slice(sites, func(i, j int) bool {
		if sites[i] == "" || sites[j] == "" {
			return sites[i] == "" // 默认 scope 永远排在最前
		}
		return sites[i] < sites[j]
	})
	return sites
}

// ensureErrorQueue 预建某站点的错误队列运行态（启动期单线程调用，理由同 ensureRepeatQueue：
// 运行期往那几张 map 里塞键会与监控接口的裸 range 撞成进程级 fatal）。
func (e *Engine) ensureErrorQueue(site string) *errorQueueState {
	name := errorQueueKey(site)
	if st := e.errorQueues[name]; st != nil {
		return st
	}
	st := &errorQueueState{}
	if e.errorQueues == nil { // 手工构造的 Engine（测试/业务自己拼）没有这一步初始化
		e.errorQueues = make(map[string]*errorQueueState)
	}
	if e.queueRuns == nil {
		e.queueRuns = make(map[string]*queueRunState)
	}
	if e.queueCounters == nil {
		e.queueCounters = make(map[string]*atomic.Int64)
	}
	e.errorQueues[name] = st
	if e.queueRuns[name] == nil {
		e.queueRuns[name] = &queueRunState{}
	}
	e.queueCounters[name] = &st.retried
	return st
}

// ensureErrorQueues 备齐所有站点的错误队列键（默认 scope 永远建一份，理由同轮询队列）。
func (e *Engine) ensureErrorQueues() {
	e.ensureErrorQueue("")
	for key := range e.sites {
		e.ensureErrorQueue(key)
	}
}

// errorQueueQuery 该站点失败任务的查询条件：status=failed，配了上限时再处理代数未超 max_retry。
//
// **必须带上 Model**：这个构造器有两个消费方 —— `processInBatches` 的 `Find(&tasks)`
// （切片元素类型能推出表名，带不带都行）和 `sampleQueueBacklogOne` 的 `Count(&n)`
// （**gorm 的 Count 推不出表名**，没有 Model 就直接报 "Table not set"）。
// 漏掉 Model 的后果不是报错，而是积压数恒为 0 + 每轮采样往日志写一条 WARN。
//
// `site = ?` **单独一条 Where**：与上面那条别揉进同一个字符串（gorm 只在"多个表达式"时
// 才给带 AND/OR 的子句加括号）。
func (e *Engine) errorQueueQuery(site string) func() *gorm.DB {
	cfg := e.errorQueueConfig(site)
	return func() *gorm.DB {
		q := e.db.Model(&models.CrawlerTask{}).
			Where("status = ?", models.TaskStatusFailed).
			Where("site = ?", site)
		if cfg.MaxRetry > 0 {
			q = q.Where("reprocess < ?", cfg.MaxRetry)
		}
		return q
	}
}

// ProcessErrorQueue 把**每个站点**各跑一遍（向后兼容业务 cron 里的既有用法）。
// 单站的耗时是 Σ 各站，站点多的时候用 ProcessSiteErrorQueue 只跑要的那一个。
func (e *Engine) ProcessErrorQueue() (int, error) {
	total := 0
	var errs []error
	for _, site := range e.errorQueueSites() {
		n, err := e.ProcessSiteErrorQueue(site)
		total += n
		if err != nil {
			errs = append(errs, err)
		}
	}
	return total, errors.Join(errs...)
}

// ProcessSiteErrorQueue 查询**本站**失败任务并重新投递到各自阶段，返回实际重新投递的数量。
func (e *Engine) ProcessSiteErrorQueue(site string) (int, error) {
	name := errorQueueKey(site)
	st := e.errorQueue(name)
	if st == nil {
		return 0, fmt.Errorf("%w: %q", ErrUnknownSite, site)
	}
	// 本站的锁：自动那一跳与后台手动点击不会互相插队（跨站点不互斥，各跑各的）
	st.mu.Lock()
	defer st.mu.Unlock()

	cfg := e.errorQueueConfig(site)
	query := e.errorQueueQuery(site)
	e.beginQueueRun(name)
	n, err := e.processInBatches(query, cfg.BatchSize, cfg.WorkerCount, e.requeueFailedTask(site))
	e.endQueueRun(name, n, err)
	// 本轮到点即采样一次积压，监控页无需等下一轮采样周期
	e.sampleQueueBacklogOne(name, query)
	return n, err
}

// errorRetriedTotal 所有站点累计重新投递数之和（监控埋点 `error_retry_total`）。
func (e *Engine) errorRetriedTotal() int64 {
	var total int64
	for _, st := range e.errorQueues {
		total += st.retried.Load()
	}
	return total
}

// requeueFailedTask 绑定本站的错误队列重投（site 是**队列**的站点，见下面那条守卫的注释）。
func (e *Engine) requeueFailedTask(site string) func(*models.CrawlerTask) bool {
	return func(t *models.CrawlerTask) bool { return e.requeueFailedTaskIn(site, t) }
}

// requeueFailedTaskIn 将单条失败任务重置为 pending 并重新投递到其阶段工作池。
func (e *Engine) requeueFailedTaskIn(site string, t *models.CrawlerTask) bool {
	name := errorQueueKey(site)
	info := e.stages[t.Stage]
	if info == nil {
		e.loggerSet.Engine.Warnf("error queue: stage %s not registered, skip task %d", t.Stage, t.ID)
		return false
	}
	// 守卫拿**队列的**站点比对（不是行上那个）：批次 SELECT 与这次重置之间，运营可能把这一行
	// 改到了别的站 —— 那时 0 行正好把它让给新站点的队列，而不是被本站再投一次。
	res := e.db.Model(&models.CrawlerTask{}).
		Where("id = ? AND site = ?", t.ID, site).
		Updates(map[string]any{
			"status":    models.TaskStatusPending,
			"retry":     0,
			"reprocess": gorm.Expr("reprocess + 1"),
		})
	if res.Error != nil {
		e.loggerSet.Engine.Errorf("error queue: reset task %d: %s", t.ID, res.Error.Error())
		return false
	}
	if res.RowsAffected == 0 {
		return false // 行没了 / 已不属于本站：跳过，不计数也不标失败
	}

	task := e.taskFromRecord(t)
	task.Retry = 0 // 上面刚把行里的 retry 归零，内存副本跟上，别让 handler 读到已经作废的重试次数
	e.dedupCache.Add(task.Unique())
	if err := e.submitTo(info, task); err != nil {
		e.dedupCache.Delete(task.Unique())
		e.loggerSet.Engine.Errorf("error queue: submit task %d: %s", t.ID, err.Error())
		// 上面刚把这行重置成 pending，失败不管的话它就再也没人捞了（本队列只捞 failed）
		e.markRequeueFailed(name, t.ID, err)
		return false
	}
	if st := e.errorQueue(name); st != nil {
		st.retried.Add(1)
	}
	return true
}

// startErrorQueues 每站起一个错误队列 ticker（间隔取自该站的生效配置）。
func (e *Engine) startErrorQueues() {
	for _, site := range e.errorQueueSites() {
		e.startErrorQueue(site)
	}
}

// startErrorQueue 启动某一站的错误队列定时自动处理。
func (e *Engine) startErrorQueue(site string) {
	e.runDynamicTicker(func() time.Duration {
		cfg := e.errorQueueConfig(site)
		if !cfg.Enabled || cfg.Interval <= 0 {
			return 0
		}
		return cfg.Interval
	}, nil /* 数据驱动的唤醒只有轮询队列用得上 */, func() {
		if n, err := e.ProcessSiteErrorQueue(site); err != nil {
			e.loggerSet.Engine.Errorf("error queue[%s]: auto process: %s", errorQueueKey(site), err.Error())
		} else if n > 0 {
			e.loggerSet.Engine.Infof("error queue[%s]: auto processed %d failed tasks", errorQueueKey(site), n)
		}
	})
}
