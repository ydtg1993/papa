package app

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"fmt"
	"github.com/ydtg1993/papa/v4/admin/configstore"
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/engine"
	"github.com/ydtg1993/papa/v4/internal/database"
	"github.com/ydtg1993/papa/v4/pkg/events"
	"github.com/ydtg1993/papa/v4/pkg/loggers"
	"gopkg.in/natefinch/lumberjack.v2"
	"os"
	"path/filepath"
	"time"
)

// NewApp 统一初始化所有组件，并完成依赖注入
func NewApp(opts ...Option) (*App, error) {
	a := &App{}
	for _, opt := range opts {
		if err := opt(a); err != nil {
			return nil, err
		}
	}

	// 1. 加载配置
	cfgPath := a.configPath
	if cfgPath == "" {
		cfgPath = os.Getenv("PAPA_CONFIG")
	}
	if cfgPath == "" {
		cfgPath = "configs/config.yaml"
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("load config: %w", err)
	}

	// 2. 初始化日志（传入配置，以便控制日志级别、输出等）
	loggerSet := loggers.NewLoggerSet(loggers.LoggerConfig{
		Dir:        cfg.Log.Dir,
		MaxSize:    cfg.Log.MaxSize,
		MaxAge:     cfg.Log.MaxDays,
		MaxBackups: cfg.Log.MaxBackups,
		LocalTime:  cfg.Log.LocalTime,
		Compress:   cfg.Log.Compress,
		Level:      cfg.Log.Level,
	})

	// 3. 初始化数据库
	db, err := database.NewDB(cfg)
	if err != nil {
		return nil, fmt.Errorf("connect to database: %w", err)
	}

	// 4. 建表不在这里做。迁移只有一条路：显式跑一次（`papa migrate`，或项目里 `make migrate`
	//    走 App.Migrate）。原来 dev 会随启动自动迁一次 —— 那让"迁移"变成一件不用学的事，
	//    而生产上没人会想起来跑它；两边跑同一件事，才谈得上"dev 练过的就是生产要做的"。
	//    这里只检查：该有的表在不在。

	// 4.1 表不在 = 对应功能看着启用、实际什么都写不进去。必须显式喊一声 ——
	// 审计表那条「开关开着、实际什么都没记」的静默丢失就是这么来的。
	//
	// 一次 information_schema 查询查全部（原来逐表 db.Migrator().HasTable：gorm 的 MySQL
	// 迁移器每次都要先猜库名，一张表 3 条 SQL，启动日志被十几条「查表在不在」刷屏）。
	missingTables, missingIndexes, schemaErr := checkFrameworkSchema(db, cfg)
	if schemaErr != nil {
		loggerSet.Sys.Errorf("检查框架表/唯一索引失败：%s（无法确认表结构是否就绪，请检查数据库连接/权限）", schemaErr.Error())
	} else {
		for _, name := range missingTables {
			loggerSet.Sys.Errorf("表 %s 不存在 —— 相关功能不会写入任何数据。请先跑迁移：`papa migrate`（或项目 Makefile 里的 migrate）", name)
		}
		// 4.2 该有的索引不在也要喊：表在、索引名不对 = 迁移没跑到那一步（或表被手工改过），
		// 与上面"表不存在"是同一类事 —— 都是"看着在跑、实际不对"。
		for _, idx := range missingIndexes {
			hint := ""
			for _, c := range frameworkIndexHints {
				if c.index == idx {
					hint = c.hint
				}
			}
			if hint != "" {
				hint = "（" + hint + "）"
			}
			loggerSet.Sys.Errorf("crawler_tasks 缺索引 %s%s —— 请先跑迁移：`papa migrate`（大表加索引建议低峰期）", idx, hint)
		}
	}

	// 5. 创建爬虫引擎
	// 局部变量叫 eng 而不是 engine：包名已经是 engine，`engine := engine.NewEngine(...)`
	// 虽然合法（RHS 在声明语句结束前仍解析到包），但读起来像自己给自己赋值。
	eng := engine.NewEngine(db, cfg, &loggerSet)

	// 运行事件流（obs.events，默认关）：与人类日志同一套轮转参数，写 log.dir/events.jsonl。
	// 关闭时用 NopEmitter —— Emit 是空调用，热路径不做分配。
	a.runID = newRunID()
	if cfg.Obs.Events.Enabled {
		em := newEventEmitter(cfg, a.runID)
		eng.SetEventsEmitter(em)
		a.events = em
	} else {
		a.events = events.NopEmitter{}
	}

	a.Config = cfg
	a.Logger = &loggerSet
	a.DB = db
	a.Engine = eng
	a.configStore = configstore.New(db)
	// 站点快照的落点：引擎按声明重建、暂停/恢复时整块回写，读走内存
	//（`crawler_config` 的 `sites` 键，见 admin/configstore/sites.go 与 engine/sitestat.go）。
	a.Engine.SetSiteStore(a.configStore)
	return a, nil
}

// Migrate 建 / 补表：框架自带的（按配置开关）+ 用 papa.WithModels 注册的业务模型。
//
// 迁移只有这一条路 —— 启动时不自动迁。`papa new` 生成的 main.go 把它接在 `-migrate` 参数上
// （Makefile 里就是 `make migrate`），于是"建表"是显式的一步，dev 和生产跑的是同一件事。
// AutoMigrate 只增不减、幂等，重复跑是安全的。
func (a *App) Migrate() error {
	return database.Migrate(a.DB, a.Config, a.extraModels...)
}

// Events 返回运行事件流；未开启时是 NopEmitter（可安全调用 Emit）。
//
// 业务层用它与框架写同一个 events.jsonl（同一 run_id / seq 序列）：
//
//	app.Events().Emit(events.Event{Type: "your_event", Site: "...", Counts: ...})
//
// 注意：业务事件类型是业务侧约定，框架只保证承载与落盘，不做语义解释。
func (a *App) Events() events.Emitter {
	if a.events == nil {
		return events.NopEmitter{}
	}
	return a.events
}

// eventLifecycle 有生命周期的 emitter（框架内置的 bufferedEmitter 实现）。
// NopEmitter 不实现它，于是"关闭时什么都不做"不需要额外分支。
type eventLifecycle interface {
	events.Emitter
	Close(ctx context.Context) error
	Dropped() uint64
}

// newEventEmitter 按配置组装 JSONL 事件流；轮转沿用 log 段参数（10 个人类日志同一套）。
func newEventEmitter(cfg *config.Config, runID string) events.Emitter {
	file := cfg.Obs.Events.File
	if file == "" {
		file = "events.jsonl"
	}
	lj := &lumberjack.Logger{
		Filename:   filepath.Join(cfg.Log.Dir, file),
		MaxSize:    cfg.Log.MaxSize,
		MaxAge:     cfg.Log.MaxDays,
		MaxBackups: cfg.Log.MaxBackups,
		Compress:   cfg.Log.Compress,
		LocalTime:  cfg.Log.LocalTime,
	}
	return events.NewEmitter(runID, events.NewJSONLSink(lj), cfg.Obs.Events.Buffer, time.Now)
}

// newRunID 生成一次进程运行的身份：毫秒时间戳 + 4 字节随机，同机多次启动不冲突。
func newRunID() string {
	var b [4]byte
	if _, err := crand.Read(b[:]); err != nil {
		return fmt.Sprintf("r-%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("r-%d-%s", time.Now().UnixMilli(), hex.EncodeToString(b[:]))
}

// emitRunStarted 写 run 开始事件。
func (a *App) emitRunStarted() {
	a.Events().Emit(events.Event{Type: events.TypeRunStarted, Source: "app",
		Counts: map[string]int{"pid": os.Getpid()}})
}

// emitRunStopped 写 run 结束事件（drained 与存留量来自 Engine.Stop）。
func (a *App) emitRunStopped(drained bool, stats map[string]engine.StopStats) {
	var unfinished, queued int
	for _, st := range stats {
		unfinished += st.Unfinished
		queued += st.Queued
	}
	outcome := "drained"
	if !drained {
		outcome = "partial"
	}
	a.Events().Emit(events.Event{Type: events.TypeRunStopped, Source: "app", Outcome: outcome,
		Counts: map[string]int{"unfinished": unfinished, "queued": queued}})
}

// closeEvents 关闭事件流并限时排空（事件流关闭时是 no-op）。
func (a *App) closeEvents() {
	life, ok := a.events.(eventLifecycle)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := life.Close(ctx); err != nil {
		a.Logger.Sys.Warnf("close event stream: %s", err.Error())
	}
	if d := life.Dropped(); d > 0 {
		a.Logger.Sys.Warnf("event stream dropped %d events (buffer full or write failed)", d)
	}
}
