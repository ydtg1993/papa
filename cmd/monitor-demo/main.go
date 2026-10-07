// 一次性监控后台演示入口，用于肉眼看页面渲染效果。
// 运行：go run ./cmd/monitor-demo
// 浏览器打开 http://localhost:9090/monitor（Ctrl+C 退出）。
// 说明：用假阶段数据 + 真实系统指标起服务，无需 MySQL；验证完可整目录删除。
package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ydtg1993/oao"
	"github.com/ydtg1993/papa/v2/core"
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
	getter := func() map[string]core.StageStats { return fakeStageStats() }

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

	// 访问令牌：内存存储，登录校验与「访问令牌」页共用它（后台新建的令牌当场就能登录）
	tokens := newDemoTokens()

	mon := server.NewMonitor(getter, stdLogger{}, server.MonitorConfig{
		VerifyToken: tokens.verify,
		// 关停要求把令牌再输一遍（与 internal/app 的装配一致）
		VerifyTokenValue: tokens.confirm,
		Metrics:          metrics,
		QueueStats:       fakeQueueStats,
		SysInfo:          sc,
		LogDir:           "logs",
		OnShutdown:       func() { stop() },
	})

	mux := http.NewServeMux()
	mon.Register(mux)

	// 表格组件：与 internal/app 的装配方式一致（含 /static/oao/ 静态资源），
	// 用来验证 template.html + mo.js 与 oao 的联动。
	o, err := oao.New(oao.Config{
		Logger: stdLogger{},
		Auth:   mon.Auth,
		Tables: []oao.Table{
			fakeTaskTable(), fakeStageTable(),
			fakeOrderTable(), // 4 个动作 → 看「更多 ▾」折叠
			fakeOplogTable(), // 「操作日志」页，看「操作人」列
		},
	})
	if err != nil {
		log.Fatalf("init oao: %v", err)
	}
	o.Mount(mux)
	static, err := o.StaticFS()
	if err != nil {
		log.Fatalf("oao static: %v", err)
	}
	mux.Handle("/static/oao/", http.StripPrefix("/static/oao/",
		server.NoCache(http.FileServer(http.FS(static)))))

	// 「访问令牌」是后台自带的模块（不是 oao 表格页），接口单独挂
	tokens.register(mux, mon)

	demoCustomPage(mux, mon)

	addr := ":9090"
	log.Printf("监控演示已启动：http://localhost%s/monitor（Ctrl+C 退出）", addr)
	log.Printf("  登录令牌：%s（登录框里填它；在「访问令牌」页新建的令牌也能登录）", demoToken)
	log.Printf("  值得看：侧边栏 General 下的「访问令牌」（新增 → 明文只显示一次 → 拿它登录）")
	log.Printf("  以及「演示」分组：订单（4 个动作 → 更多 ▾）/ 操作日志（操作人列）/ 审核（自定义页）")
	go func() { _ = http.ListenAndServe(addr, mux) }()
	<-ctx.Done()
	log.Println("bye")
}

func fakeStageStats() map[string]core.StageStats {
	return map[string]core.StageStats{
		"catalog": {
			Global: core.GlobalStats{TotalTasks: 8, TotalFailed: 1, TotalTime: 98955000, AvgTime: 12369375, MaxTime: 22137200, MinTime: 6222500},
			Workers: map[int]core.WorkerStat{
				0: {WorkerID: 0, TotalTasks: 3, FailedTasks: 0, TotalTime: 39456100, MaxTime: 19010900, MinTime: 8489000},
				1: {WorkerID: 1, TotalTasks: 3, FailedTasks: 0, TotalTime: 30861700, MaxTime: 15401900, MinTime: 6222500},
				2: {WorkerID: 2, TotalTasks: 2, FailedTasks: 1, TotalTime: 28637200, MaxTime: 22137200, MinTime: 6500000},
			},
			Queue: core.QueueStats{Submitted: 8, Completed: 7, Failed: 1, InProgress: 0, QueueLen: 0},
		},
		"detail": {
			Global: core.GlobalStats{TotalTasks: 6, TotalFailed: 0, TotalTime: 149893300, AvgTime: 24982216, MaxTime: 40192700, MinTime: 7094600},
			Workers: map[int]core.WorkerStat{
				0: {WorkerID: 0, TotalTasks: 2, FailedTasks: 0, TotalTime: 70649100, MaxTime: 40192700, MinTime: 30456400},
				1: {WorkerID: 1, TotalTasks: 4, FailedTasks: 0, TotalTime: 79244200, MaxTime: 33409100, MinTime: 7538800},
			},
			Queue: core.QueueStats{Submitted: 6, Completed: 6, Failed: 0, InProgress: 0, QueueLen: 0},
		},
		"video": {
			Global: core.GlobalStats{TotalTasks: 10, TotalFailed: 2, TotalTime: 365715800, AvgTime: 36571580, MaxTime: 60246100, MinTime: 10220500},
			Workers: map[int]core.WorkerStat{
				0: {WorkerID: 0, TotalTasks: 2, FailedTasks: 1, TotalTime: 106798900, MaxTime: 56096300, MinTime: 50702600},
				1: {WorkerID: 1, TotalTasks: 3, FailedTasks: 1, TotalTime: 71555400, MaxTime: 35336700, MinTime: 15549000},
				2: {WorkerID: 2, TotalTasks: 3, FailedTasks: 0, TotalTime: 112815500, MaxTime: 60246100, MinTime: 10220500},
				3: {WorkerID: 3, TotalTasks: 2, FailedTasks: 0, TotalTime: 74546000, MaxTime: 45605400, MinTime: 28940600},
			},
			Queue: core.QueueStats{Submitted: 10, Completed: 8, Failed: 2, InProgress: 0, QueueLen: 0},
		},
	}
}

// fakeQueueStats 治理队列假数据：覆盖「运行中 / 空闲待执行 / 已停用且有错误」三种状态。
func fakeQueueStats() map[string]core.QueueStat {
	now := time.Now()
	return map[string]core.QueueStat{
		crawler.QueueError: {
			Name: crawler.QueueError, Enabled: true, Runs: 12,
			StartedAt: now.Add(-2 * time.Minute), LastFinishAt: now.Add(-90 * time.Second),
			LastDuration: 4 * time.Second, LastProcessed: 37, TotalProcessed: 421,
			Backlog: 128, BacklogAt: now.Add(-20 * time.Second),
		},
		crawler.QueueRepeat: {
			Name: crawler.QueueRepeat, Runs: 0,
			Backlog: 2048, BacklogAt: now.Add(-20 * time.Second),
			LastError: "submit task 991: queue full",
		},
	}
}

// fakeRowsSource 内存数据源：演示业务侧怎么实现 oao.Source（真项目里换成查库）。
// 组件只把 Query 透传过来，筛选/搜索/排序/分页怎么落地全由这里决定。
type fakeRowsSource struct {
	rows  []map[string]any
	delay time.Duration
}

func (s fakeRowsSource) List(ctx context.Context, q oao.Query) ([]map[string]any, int64, error) {
	if s.delay > 0 {
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-time.After(s.delay):
		}
	}

	rows := make([]map[string]any, 0, len(s.rows))
	for _, r := range s.rows {
		if matchRow(r, q) {
			rows = append(rows, r)
		}
	}
	sortRows(rows, q.Sort)

	total := int64(len(rows))
	from := (q.Page - 1) * q.Size
	if from > len(rows) {
		from = len(rows)
	}
	to := from + q.Size
	if to > len(rows) {
		to = len(rows)
	}
	return rows[from:to], total, nil
}

// matchRow 解释 q.Filter / q.Search 的语义 —— 这部分永远属于业务层。
func matchRow(r map[string]any, q oao.Query) bool {
	if kw := q.Search; kw != "" {
		hit := false
		for _, f := range []string{"url", "title"} {
			if v, _ := r[f].(string); v != "" && strings.Contains(v, kw) {
				hit = true
			}
		}
		if !hit {
			return false
		}
	}
	for name, val := range q.Filter {
		if val == "" {
			continue
		}
		switch name {
		case "url": // OpLike
			if v, _ := r["url"].(string); !strings.Contains(v, val) {
				return false
			}
		case "stage": // OpEq
			if v, _ := r["stage"].(string); v != val {
				return false
			}
		case "status": // OpIn，"0,1,2"
			n, _ := r["status"].(int)
			hit := false
			for _, part := range strings.Split(val, ",") {
				if part == strconv.Itoa(n) {
					hit = true
				}
			}
			if !hit {
				return false
			}
		}
	}
	return true
}

func sortRows(rows []map[string]any, sortKey string) {
	parts := strings.Split(sortKey, ",")
	// 从优先级最低的键开始排：稳定排序保证后一轮不打乱前一轮
	for i := len(parts) - 1; i >= 0; i-- {
		key := strings.TrimSpace(parts[i])
		if key == "" {
			continue
		}
		desc := strings.HasPrefix(key, "-")
		name := strings.TrimPrefix(key, "-")
		sort.SliceStable(rows, func(a, b int) bool {
			c := compareCell(rows[a][name], rows[b][name])
			if desc {
				return c > 0
			}
			return c < 0
		})
	}
}

// compareCell 比大小；演示用，真项目里这活交给数据库。
func compareCell(a, b any) int {
	switch x := a.(type) {
	case int:
		y, _ := b.(int)
		switch {
		case x < y:
			return -1
		case x > y:
			return 1
		}
		return 0
	case time.Time:
		y, _ := b.(time.Time)
		switch {
		case x.Before(y):
			return -1
		case x.After(y):
			return 1
		}
		return 0
	default:
		return strings.Compare(fmt.Sprint(a), fmt.Sprint(b))
	}
}

// fakeTaskTable 内置「任务」表的样子，用来验证动态菜单与各种渲染器。
func fakeTaskTable() oao.Table {
	rows := make([]map[string]any, 0, 60)
	stages := []string{"catalog", "detail", "video"}
	for i := 1; i <= 60; i++ {
		rows = append(rows, map[string]any{
			"id":         i,
			"stage":      stages[i%3],
			"url":        fmt.Sprintf("https://example.com/%s/%d", stages[i%3], i),
			"title":      fmt.Sprintf("第 %d 条：标题可能比较长，用来验证省略号与悬停展开", i),
			"status":     i % 4,
			"retry":      i % 3,
			"reprocess":  i % 2,
			"repeat":     i % 5,
			"error":      "第 " + strconv.Itoa(i) + " 条：错误信息可能很长，用来验证只读输入框的横向滚动展示效果，一直写下去看看会不会撑破表格",
			"updated_at": time.Now().Add(-time.Duration(i) * time.Minute),
		})
	}
	return oao.Table{
		Key: "task", Label: "任务", Group: "数据",
		Source: fakeRowsSource{rows: rows, delay: 150 * time.Millisecond},
		Columns: []oao.Column{
			{Field: "id", Label: "ID", Kind: oao.KindNumber, Width: "70px"},
			{Field: "stage", Label: "阶段"},
			{Field: "url", Label: "URL", Render: oao.RenderLink, Href: "{url}"},
			{Field: "title", Label: "标题"},
			{Field: "status", Label: "状态", Kind: oao.KindNumber, Render: oao.RenderEnum,
				Enum: map[string]string{"0": "待处理", "1": "处理中", "2": "成功", "3": "失败"},
				Tone: map[string]string{"0": "info", "1": "warn", "2": "ok", "3": "err"}},
			{Field: "retry", Label: "重试", Kind: oao.KindNumber, Width: "70px"},
			{Field: "reprocess", Label: "重投", Kind: oao.KindNumber, Width: "70px"},
			{Field: "error", Label: "错误", Render: oao.RenderInput, MaxLen: 40},
			{Field: "updated_at", Label: "更新时间", Kind: oao.KindTime},
		},
		Filters: []oao.Filter{
			{Field: "url", Label: "URL", Op: oao.OpLike},
			{Field: "stage", Label: "阶段"},
			{Field: "status", Label: "状态", Kind: oao.KindNumber, Op: oao.OpIn,
				Options: map[string]string{"0": "待处理", "1": "处理中", "2": "成功", "3": "失败"}},
		},
		DefaultSort: "-updated_at",
		PageSize:    20,
	}
}

// fakeStageTable 业务自定义的表，验证动态菜单的分组。
func fakeStageTable() oao.Table {
	return oao.Table{
		Key: "series", Label: "剧集", Group: "业务",
		Source: fakeRowsSource{rows: []map[string]any{
			{"id": 1, "name": "示例剧集 A", "episodes": 24, "cover": "https://picsum.photos/seed/a/120/120"},
			{"id": 2, "name": "示例剧集 B", "episodes": 12, "cover": "https://picsum.photos/seed/b/120/120"},
			{"id": 3, "name": "示例剧集 C", "episodes": 36, "cover": "https://picsum.photos/seed/c/120/120"},
		}},
		Columns: []oao.Column{
			{Field: "id", Kind: oao.KindNumber, Width: "70px"},
			{Field: "name", Label: "名称"},
			{Field: "episodes", Label: "集数", Kind: oao.KindNumber},
			{Field: "cover", Label: "封面", Render: oao.RenderImage, Size: 56},
		},
		DefaultSort: "id",
	}
}
