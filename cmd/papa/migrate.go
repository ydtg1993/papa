package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/ydtg1993/papa/v3/internal/database"
)

// papaModule 是框架自己的 module 路径 —— 用来区分「在业务项目里」和「在 papa 仓库里」。
const papaModule = "github.com/ydtg1993/papa/v3"

const migrateUsage = `usage: papa migrate [-c configs/config.yaml]

建 / 补表。

在业务项目目录里跑（当前目录的 go.mod 依赖了 papa）时，本命令转交项目自己的迁移入口
（go run . -migrate）—— 框架自带的表与业务注册的模型一起建。只有项目自己的进程认识
那些模型（papa.WithModels / 脚手架根 models 包的 Models()），CLI 是独立编译的，拿不到。

其余情况（不在项目里、或在 papa 仓库里）只建框架自带的表：crawler_tasks、
crawler_access_token，以及按开关启用的 crawler_operation_log、crawler_task_trace。

  -c/--config   配置文件路径；缺省读 PAPA_CONFIG 环境变量，再回退 configs/config.yaml
                转交项目时会作为 PAPA_CONFIG 传给子进程

启动时不会自动迁移 —— 建表就是显式跑这一步。AutoMigrate 只增不减（加表/加列/加索引，
不删列也不改类型），重复跑是幂等的，所以生产上可以放心执行。`

// runMigrateCmd 建/补表。
//
// 在业务项目里（cwd 的 go.mod 依赖 papa）转交 `go run . -migrate` —— 业务模型是在项目进程里
// 注册的，只有那里拿得到；其余情况只建框架自带的表。
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

	// 业务项目：交给项目自己的迁移入口（Makefile 里 `make migrate` 跑的就是它）。
	// 必须赶在下面自建连接之前 —— 那份连接只认框架表。
	if mod, ok := businessProject("."); ok {
		return delegateMigrate(mod, cfgPath)
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

// businessProject 判断 dir 是不是一个引用了 papa 的业务项目。
// 是则返回它的 module 路径；不是（没有 go.mod / 不依赖 papa / 就是 papa 仓库本身）返回 false。
//
// 判据只看 go.mod，不猜别的东西：业务项目必然直接 import papa（或本地 replace 它），
// 而 papa 仓库自己的 module 路径就是 papaModule —— 后者跑 `papa migrate` 应当是建框架表。
func businessProject(dir string) (string, bool) {
	data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
	if err != nil {
		return "", false
	}
	content := string(data)
	mod := modulePath(content)
	if mod == "" || mod == papaModule {
		return "", false
	}
	if !strings.Contains(content, papaModule) {
		return "", false
	}
	return mod, true
}

// modulePath 取 go.mod 里的 `module x` 行。
func modulePath(gomod string) string {
	for line := range strings.SplitSeq(gomod, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// delegateMigrate 把迁移转交业务项目自己跑：`go run . -migrate`。
//
// 这条路不依赖项目怎么组织模型 —— 它跑的是项目 main 里接好的 App.Migrate()，
// 走的是项目自己的配置加载与模型清单（见脚手架 main.go 的 `-migrate` 分支）。
// 代价是迁移时要能编译项目（Go 工具链 + 依赖可用）。
func delegateMigrate(mod, cfgPath string) error {
	if _, err := exec.LookPath("go"); err != nil {
		return fmt.Errorf("检测到业务项目 %s，但没找到 go 命令，转交不了；\n"+
			"装上 Go 工具链后重试，或直接在项目里跑 make migrate", mod)
	}

	fmt.Printf("检测到业务项目 %s —— 转交项目自己的迁移：go run . -migrate\n", mod)
	fmt.Println("（框架自带的表 + 业务注册的模型一起建）")

	cmd := exec.Command("go", "run", ".", "-migrate")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Env = os.Environ()
	if cfgPath != "" {
		cmd.Env = append(cmd.Env, "PAPA_CONFIG="+cfgPath)
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("项目迁移失败: %w（也可以在项目里直接跑 make migrate）", err)
	}
	return nil
}
