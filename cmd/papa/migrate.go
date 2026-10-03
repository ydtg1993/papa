package main

import (
	"fmt"
	"strings"

	"github.com/ydtg1993/papa/v2/internal/database"
)

const migrateUsage = `usage: papa migrate [-c configs/config.yaml]

建 / 补框架自带的表（crawler_tasks、crawler_access_token，以及按开关启用的
crawler_operation_log、crawler_task_trace）。

  -c/--config   配置文件路径；缺省读 PAPA_CONFIG 环境变量，再回退 configs/config.yaml

启动时不会自动迁移 —— 建表就是显式跑这一步。AutoMigrate 只增不减（加表/加列/加索引，
不删列也不改类型），重复跑是幂等的，所以生产上可以放心执行。

脚手架生成的项目用 make migrate 更省事：那条会连业务自己的模型一起建
（这里建不了 —— 命令行拿不到 papa.WithModels）。`

// runMigrateCmd 建/补框架自带的表。
//
// 建不了业务自己的表 —— 那些模型是通过 papa.WithModels 在业务 main.go 里注册的，
// 命令行拿不到。业务要建自己的表，就在自己的启动流程里跑 AutoMigrate。
func runMigrateCmd(args []string) error {
	cfgPath := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		take := func() (string, bool) {
			if i+1 < len(args) {
				i++
				return args[i], true
			}
			return "", false
		}
		switch {
		case a == "-c" || a == "--config":
			v, ok := take()
			if !ok {
				return fmt.Errorf("-c 缺少值\n\n%s", migrateUsage)
			}
			cfgPath = v
		case strings.HasPrefix(a, "--config="):
			cfgPath = strings.TrimPrefix(a, "--config=")
		default:
			return fmt.Errorf("未知参数 %q\n\n%s", a, migrateUsage)
		}
	}

	cfg, ok := loadOptionalConfig(cfgPath)
	if !ok {
		return fmt.Errorf("读不到配置（用 -c 指定 configs/config.yaml，或设 PAPA_CONFIG）")
	}
	db, err := database.NewDB(cfg)
	if err != nil {
		return fmt.Errorf("连接数据库失败: %w", err)
	}

	// gorm 只在 info 级打 DDL，而生产默认 warn（见 db.log_level），所以自己报一份清单
	fmt.Println("将确保以下表存在（只增不减，不会删列 / 改类型）：")
	for _, m := range database.FrameworkModels(cfg) {
		fmt.Printf("  - %s\n", m.Name)
	}
	if !cfg.Server.OperationLog {
		fmt.Println("提示：server.operation_log 关着，这次不建 crawler_operation_log；开启后重跑本命令即可。")
	}
	if !cfg.Crawler.Trace.Enabled {
		fmt.Println("提示：crawler.trace.enabled 关着，这次不建 crawler_task_trace；开启后重跑本命令即可。")
	}

	if err := database.Migrate(db, cfg); err != nil {
		return fmt.Errorf("迁移失败: %w", err)
	}
	fmt.Println("完成。")
	return nil
}
