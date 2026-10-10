package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/models"
)

// setSiteQueues 直接登记某站的队列生效配置。生产路径只有一条（App.RegisterSites 按站点声明解析好
// 交给 SetSiteQueues），测试要造的正是"已登记"这个状态；而 SetSiteQueues 拒收空站点（未归属没有
// 队列），所以默认 scope 的那几份配置在这儿直接写进 map。
func setSiteQueues(e *Engine, site string, q SiteQueues) {
	e.queueCfgMu.Lock()
	defer e.queueCfgMu.Unlock()
	if e.siteQueues == nil {
		e.siteQueues = make(map[string]SiteQueues)
	}
	e.siteQueues[site] = q
}

// 站点级队列配置：**没登记过的站点 = 零值 = 三队列都不跑** —— 默认 scope（未归属）与手搓引擎的
// 测试都靠这条（没声明的站点不会凭空拿到一份"默认配置"）。
func TestSiteQueueConfigUnregisteredMeansNotRunning(t *testing.T) {
	f := newFakeTaskDB()
	e := siteEngine(t, f)

	// 没登记：三份生效值都是零值（Enabled = false → 后台那几行显示已停用）
	if got := e.errorQueueConfig("a"); got != (config.ErrorQueueConfig{}) {
		t.Fatalf("没登记的站点应当是零值：%+v", got)
	}
	if got := e.errorQueueConfig(""); got != (config.ErrorQueueConfig{}) {
		t.Fatalf("默认 scope 没有声明可挂，也应当是零值：%+v", got)
	}
	if e.queueEnabled(QueueError) || e.queueEnabled("error_queue:a") {
		t.Fatal("没登记的站点，后台那行应当是已停用")
	}

	// 登记过的站点用自己的那份；别的站点不受影响
	own := config.ErrorQueueConfig{Enabled: true, WorkerCount: 9, Interval: 30 * time.Minute, MaxRetry: 1, BatchSize: 10}
	e.SetSiteQueues("a", SiteQueues{Error: own, Repeat: config.RepeatQueueConfig{Enabled: true, Interval: time.Hour}})
	if got := e.errorQueueConfig("a"); got != own {
		t.Fatalf("登记过的站点用自己的那份：%+v", got)
	}
	if got := e.errorQueueConfig("b"); got != (config.ErrorQueueConfig{}) {
		t.Fatalf("别的站点不受影响（仍是零值）：%+v", got)
	}
	if !e.queueEnabled("error_queue:a") || e.queueEnabled("error_queue:b") {
		t.Fatal("后台那一行跟着各站自己的配置走")
	}
}

// SetSiteQueues 拒收空站点：未归属的声明不能配队列（要治理那些任务，先给这一站一个 Key）。
func TestSetSiteQueuesRejectsDefaultScope(t *testing.T) {
	e := siteEngine(t, newFakeTaskDB())
	e.SetSiteQueues("", SiteQueues{Error: config.ErrorQueueConfig{Enabled: true}})
	if got := e.errorQueueConfig(""); got != (config.ErrorQueueConfig{}) {
		t.Fatalf("空站点不该被登记：%+v", got)
	}
}

// 错误队列按站点拆：每站自己的锁、累计计数与运行快照；未知站点报 ErrUnknownSite。
func TestProcessSiteErrorQueuePerSite(t *testing.T) {
	f := newFakeTaskDB()
	e := siteEngine(t, f)
	setSiteQueues(e, "a", SiteQueues{Error: config.ErrorQueueConfig{Enabled: true, BatchSize: 10, WorkerCount: 1}})
	setRows(f, 1, "stub", models.TaskStatusFailed)

	if n, err := e.ProcessSiteErrorQueue("a"); err != nil || n != 1 {
		t.Fatalf("ProcessSiteErrorQueue(a) = %d, %v, want 1, nil", n, err)
	}
	stats := e.GetQueueStats()
	if got := stats["error_queue:a"]; got.Runs != 1 || got.TotalProcessed != 1 {
		t.Fatalf("站点 a 的快照 = %+v, want Runs=1 TotalProcessed=1", got)
	}
	if got := stats["error_queue:b"]; got.Runs != 0 || got.TotalProcessed != 0 {
		t.Fatalf("站点 b 不该被站点 a 的执影响到：%+v", got)
	}
	if _, err := e.ProcessSiteErrorQueue("ghost"); !errors.Is(err, ErrUnknownSite) {
		t.Fatalf("未知站点应报 ErrUnknownSite，实得 %v", err)
	}
}

// error_queue 的查询按站点过滤（假库不解析 WHERE，只能钉 SQL 形态）。
func TestErrorQueueQueryCarriesSite(t *testing.T) {
	f := newFakeTaskDB()
	e := siteEngine(t, f)
	setSiteQueues(e, "a", SiteQueues{Error: config.ErrorQueueConfig{Enabled: true, MaxRetry: 3}})

	e.errorQueueQuery("a")().Find(&[]models.CrawlerTask{})
	read := f.readSQL()
	for _, want := range []string{"status = ?", "site = ?", "reprocess < ?"} {
		if !strings.Contains(read, want) {
			t.Fatalf("error_queue 的查询缺少 %q：\n%s", want, read)
		}
	}
}

// 每站一个错误队列 ticker：只按**该站**的生效配置决定跑不跑。
func TestStartErrorQueuesFollowsPerSiteInterval(t *testing.T) {
	f := newFakeTaskDB()
	e := siteEngine(t, f)
	pool := e.stages["stub"].workerPool
	pool.Start(e.ctx, func(context.Context, *Task) error { return nil })

	// 默认 scope 没登记 → 不跑；站点 a 自己配了 interval → 自动跑
	e.SetSiteQueues("a", SiteQueues{Error: config.ErrorQueueConfig{
		Enabled: true, Interval: 20 * time.Millisecond, BatchSize: 10, WorkerCount: 1,
	}})
	e.startErrorQueues()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if e.GetQueueStats()["error_queue:a"].Runs > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	stats := e.GetQueueStats()
	if stats["error_queue:a"].Runs == 0 {
		t.Fatal("站点 a 配了自己的 interval，应当自动跑")
	}
	if stats[QueueError].Runs != 0 {
		t.Fatal("没声明的默认 scope 不该跑")
	}
}
