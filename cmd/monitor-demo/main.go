// 一次性监控后台演示入口，用于肉眼看页面渲染效果。
// 运行：go run ./cmd/monitor-demo
// 浏览器打开 http://localhost:9090/monitor（Ctrl+C 退出）。
// 说明：用假阶段数据 + 真实系统指标起服务，无需 MySQL；验证完可整目录删除。
package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ydtg1993/papa/v2/crawler"
	"github.com/ydtg1993/papa/v2/internal/server"
	"github.com/ydtg1993/papa/v2/internal/sysinfo"
)

type stdLogger struct{}

func (stdLogger) Info(args ...any)                  { log.Println(args...) }
func (stdLogger) Infof(format string, args ...any)  { log.Printf(format, args...) }
func (stdLogger) Errorf(format string, args ...any) { log.Printf(format, args...) }

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 系统指标采集器：Dashboard 的 CPU/内存/磁盘 + 业务目录占用
	sc := sysinfo.NewCollector(2*time.Second, map[string]string{
		"downloads": "downloads",
		"logs":      "logs",
	})
	sc.Start(ctx)

	// 阶段统计假数据
	getter := func() map[string]crawler.StageStats { return fakeStageStats() }

	// 业务自定义指标假数据
	metrics := func() map[string]any {
		return map[string]any{
			"catalog_total": 128,
			"detail_total":  96,
			"video_total":   42,
			"success_rate":  0.972,
			"running":       true,
			"started_at":    "2026-09-29T08:00:00Z",
		}
	}

	mon := server.NewMonitor(getter, stdLogger{}, server.MonitorConfig{
		Metrics:    metrics,
		SysInfo:    sc,
		LogDir:     "logs",
		OnShutdown: func() { stop() },
	})

	mux := http.NewServeMux()
	mon.Register(mux)

	addr := ":9090"
	log.Printf("监控演示已启动，浏览器打开 http://localhost%s/monitor（Ctrl+C 退出）", addr)
	go func() { _ = http.ListenAndServe(addr, mux) }()
	<-ctx.Done()
	log.Println("bye")
}

func fakeStageStats() map[string]crawler.StageStats {
	return map[string]crawler.StageStats{
		"catalog": {
			Global: crawler.GlobalStats{TotalTasks: 8, TotalFailed: 1, TotalTime: 98955000, AvgTime: 12369375, MaxTime: 22137200, MinTime: 6222500},
			Workers: map[int]crawler.WorkerStat{
				0: {WorkerID: 0, TotalTasks: 3, FailedTasks: 0, TotalTime: 39456100, MaxTime: 19010900, MinTime: 8489000},
				1: {WorkerID: 1, TotalTasks: 3, FailedTasks: 0, TotalTime: 30861700, MaxTime: 15401900, MinTime: 6222500},
				2: {WorkerID: 2, TotalTasks: 2, FailedTasks: 1, TotalTime: 28637200, MaxTime: 22137200, MinTime: 6500000},
			},
			Queue: crawler.QueueStats{Submitted: 8, Completed: 7, Failed: 1, InProgress: 0, QueueLen: 0},
		},
		"detail": {
			Global: crawler.GlobalStats{TotalTasks: 6, TotalFailed: 0, TotalTime: 149893300, AvgTime: 24982216, MaxTime: 40192700, MinTime: 7094600},
			Workers: map[int]crawler.WorkerStat{
				0: {WorkerID: 0, TotalTasks: 2, FailedTasks: 0, TotalTime: 70649100, MaxTime: 40192700, MinTime: 30456400},
				1: {WorkerID: 1, TotalTasks: 4, FailedTasks: 0, TotalTime: 79244200, MaxTime: 33409100, MinTime: 7538800},
			},
			Queue: crawler.QueueStats{Submitted: 6, Completed: 6, Failed: 0, InProgress: 0, QueueLen: 0},
		},
		"video": {
			Global: crawler.GlobalStats{TotalTasks: 10, TotalFailed: 2, TotalTime: 365715800, AvgTime: 36571580, MaxTime: 60246100, MinTime: 10220500},
			Workers: map[int]crawler.WorkerStat{
				0: {WorkerID: 0, TotalTasks: 2, FailedTasks: 1, TotalTime: 106798900, MaxTime: 56096300, MinTime: 50702600},
				1: {WorkerID: 1, TotalTasks: 3, FailedTasks: 1, TotalTime: 71555400, MaxTime: 35336700, MinTime: 15549000},
				2: {WorkerID: 2, TotalTasks: 3, FailedTasks: 0, TotalTime: 112815500, MaxTime: 60246100, MinTime: 10220500},
				3: {WorkerID: 3, TotalTasks: 2, FailedTasks: 0, TotalTime: 74546000, MaxTime: 45605400, MinTime: 28940600},
			},
			Queue: crawler.QueueStats{Submitted: 10, Completed: 8, Failed: 2, InProgress: 0, QueueLen: 0},
		},
	}
}
