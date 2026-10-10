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

// 站点级队列配置：**没登记过的站点回退全局那份** —— 默认 scope 与手搓引擎的测试都靠这条。
func TestSiteQueueConfigFallsBackToGlobal(t *testing.T) {
	f := newFakeTaskDB()
	e := siteEngine(t, f)
	e.cfg.ErrorQueue = config.ErrorQueueConfig{Enabled: true, WorkerCount: 3, Interval: time.Hour, MaxRetry: 5, BatchSize: 100}
	e.cfg.RepeatQueue = config.RepeatQueueConfig{Enabled: true, WorkerCount: 1, Interval: 2 * time.Hour, BatchSize: 200}

	if got := e.errorQueueConfig("a"); got != e.cfg.ErrorQueue {
		t.Fatalf("没登记的站点应回退全局：%+v", got)
	}
	if got := e.errorQueueConfig(""); got != e.cfg.ErrorQueue {
		t.Fatalf("默认 scope 永远用全局那份：%+v", got)
	}

	own := config.ErrorQueueConfig{Enabled: false, WorkerCount: 9, Interval: 30 * time.Minute, MaxRetry: 1, BatchSize: 10}
	e.SetSiteQueues("a", SiteQueues{Error: own, Repeat: e.cfg.RepeatQueue})
	if got := e.errorQueueConfig("a"); got != own {
		t.Fatalf("登记过的站点用自己的那份：%+v", got)
	}
	if got := e.errorQueueConfig("b"); got != e.cfg.ErrorQueue {
		t.Fatalf("别的站点不受影响：%+v", got)
	}

	// 后台那一行的开关也跟着走：a 站的错误队列关着 → 已停用
	if e.queueEnabled("error_queue:a") {
		t.Fatal("站点 a 的错误队列配成关，后台那行应当是已停用")
	}
	if !e.queueEnabled(QueueError) || !e.queueEnabled("error_queue:b") {
		t.Fatal("全局开着、b 站没配 → 那两行应当是启用")
	}
}

// 错误队列按站点拆：每站自己的锁、累计计数与运行快照；未知站点报 ErrUnknownSite。
func TestProcessSiteErrorQueuePerSite(t *testing.T) {
	f := newFakeTaskDB()
	e := siteEngine(t, f)
	e.cfg.ErrorQueue = config.ErrorQueueConfig{Enabled: true, BatchSize: 10, WorkerCount: 1}
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
	e.cfg.ErrorQueue = config.ErrorQueueConfig{Enabled: true, MaxRetry: 3}

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

	e.cfg.ErrorQueue = config.ErrorQueueConfig{Enabled: false} // 全局关着
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
		t.Fatal("全局关着的默认 scope 不该跑")
	}
}
