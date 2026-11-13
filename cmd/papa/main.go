package main

import (
	"bytes"
	"embed"
	"fmt"
	"go/format"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"

	"github.com/ydtg1993/papa/v4/docs"
)

//go:embed templates/*
var tmplFS embed.FS

type scaffoldData struct {
	Replace string // New go.mod only; never appended to an existing module.
	CodeDir string
	DocsDir string
	Module  string // 完整 Go module 路径，仅用于 module 声明、import 与展示。
	Project string // module 最后一段，用于文件名、构建产物和默认站点/容器名。
	// SiteFunc 站点声明函数名（sites/<项目名>.go 里的 `func xxx() papa.SiteSpec`）。
	// 模块名可能带连字符/大写，这里归一成合法标识符。
	SiteFunc string
}

// sanitizeIdent 把模块名归一成合法的 Go 标识符（只用于脚手架生成的函数名）。
func sanitizeIdent(s string) string {
	out := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			out = append(out, r)
		default:
			out = append(out, '_')
		}
	}
	id := string(out)
	if id == "" {
		return "site"
	}
	if id[0] >= '0' && id[0] <= '9' {
		return "site" + id
	}
	// 字符合法还不够，有三类名字会让生成的 `func <名字>() papa.SiteSpec` 编不过：
	// Go 关键字（`func type()` 语法错）· 预处理标识符 `_`（`func _()` 合法，但 `RegisterSite(_())`
	// 用不了它）· `init`（Go 规定 init 函数不能有参数和返回值）。撞上就加个 Site 后缀 ——
	// 不这样的话，报出来的是 format.Source 那句"生成的 xxx 不是合法 Go（模板改坏了？）"，真错因被盖掉。
	if token.IsKeyword(id) || id == "_" || id == "init" {
		return id + "Site"
	}
	return id
}

// validateModuleName 校验项目名（它就是 go.mod 里的 module 路径）。
//
// 同一个名字还会变成项目目录路径和生成代码里的 import 前缀，最后一段用于文件名 ——
// 所以规则比 Go 的模块路径再严一点：空格、引号、冒号、问号这些在文件名里非法的字符一律不收。
//
// **必须在写任何文件之前拦**：不拦的话，名字里的空格要到 `format.Source` 那一步才炸，报出来的是
// "生成的 xxx 不是合法 Go（模板改坏了？）" —— 真错因（名字）被掩盖，项目目录里还留下半成品。
func validateModuleName(name string) error {
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("项目名不能为空")
	}
	if strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") || strings.Contains(name, "//") {
		return fmt.Errorf("项目名 %q 不合法：不能以 / 开头或结尾，也不能有连续的 //", name)
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "." || seg == ".." {
			return fmt.Errorf("项目名 %q 不合法：路径段不能是 %q", name, seg)
		}
		for _, r := range seg {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
				r == '.' || r == '_' || r == '~' || r == '+' || r == '-':
			default:
				return fmt.Errorf("项目名 %q 里有不能用作模块路径与文件名的字符 %q："+
					"只允许字母、数字与 . _ ~ + - ，路径分隔用 /（例如 mycrawler 或 example.com/mycrawler）",
					name, string(r))
			}
		}
	}
	return nil
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	switch os.Args[1] {
	case "new":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: papa new <project-name> [--replace <local-papa-path>] [--missing-only]")
			os.Exit(1)
		}
		if err := runNew(os.Args[2], os.Args[3:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "check":
		if err := runCheckCmd(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "upgrade":
		if err := runUpgradeCmd(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "migrate":
		if err := runMigrateCmd(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "token":
		if err := runTokenCmd(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "report":
		if err := runReportCmd(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "html", "rod", "diff", "select":
		if err := runDebugCmd(os.Args[1], os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	default:
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  papa new <project-name> [--replace <local-papa-path>] [--missing-only]")
	fmt.Fprintln(os.Stderr, "    生成一个新爬虫项目骨架")
	fmt.Fprintln(os.Stderr, "    --replace  在 go.mod 加 replace 指向本地 papa 仓库（本地验证用，无需先发布）")
	fmt.Fprintln(os.Stderr, "    --missing-only  显式补齐已有项目缺失文件；不覆盖配置、业务代码或 go.mod")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  papa check [目录] [--boot] [-c <配置路径>]")
	fmt.Fprintln(os.Stderr, "    校验站点与阶段声明（不用起库）：阶段名重复、熔断阈值、")
	fmt.Fprintln(os.Stderr, "    Delay 与 Backoff 的写法。跑完删掉本次生成的 .papa-check-<随机后缀>/")
	fmt.Fprintln(os.Stderr, "    --boot 再往前一步：连库把启动路径跑到 RegisterSites（不 Run，不抓取）")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  papa upgrade [目录] [--write]")
	fmt.Fprintln(os.Stderr, "    把**已经生成过的**项目升到当前脚手架：刷新框架副本（docs/framework/）+ 几项不用起库的检查")
	fmt.Fprintln(os.Stderr, "    默认只报告不落盘；业务文件（main.go / internal/monitor / internal/models / internal/fetcher /")
	fmt.Fprintln(os.Stderr, "    sites / config.yaml / go.mod）一律不碰 —— 照模板覆盖会抹掉你的改动")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  papa migrate [-c configs/config.yaml]")
	fmt.Fprintln(os.Stderr, "    建/补表。在业务项目目录里跑会转交 `go run . -migrate`（连业务模型一起建）；")
	fmt.Fprintln(os.Stderr, "    其余情况只建框架自带的表。启动时不会自动迁移")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  papa token add --operator <名字>   创建一把后台访问令牌（明文只打印一次）")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  papa report generate [flags]   生成一次运行报告（只读：事件流 + 日志证据 + 框架状态）")
	fmt.Fprintln(os.Stderr, "  papa report list [flags]       列出既有报告目录")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  papa html <url> [flags]   静态 HTML 抓取")
	fmt.Fprintln(os.Stderr, "  papa rod  <url> [flags]   浏览器渲染后抓取")
	fmt.Fprintln(os.Stderr, "  papa diff <url> [flags]   对比 html 与 rod 抓取结果")
	fmt.Fprintln(os.Stderr, "  papa select <css> <url|文件> [flags]  选择器测试")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  通用 flags: -c/--config, --proxy, --timeout, --ua, --header k=v, --json, -o/--output")
	fmt.Fprintln(os.Stderr, "  输出形态: --links, --text, --select <css>")
	fmt.Fprintln(os.Stderr, "  rod 专属: --screenshot <file>, --full-page, --wait <duration>")
	fmt.Fprintln(os.Stderr, "            --act <动作>:<参数>  可重复，按序执行；支持 scroll/click/input/hover/wait/eval")
	fmt.Fprintln(os.Stderr, "            --show             有头浏览器+人工操作，回车后导出结果；--devtools 附带开 DevTools")
}

func runNew(name string, args []string) error {
	if name == "" {
		return fmt.Errorf("project name is empty")
	}
	// 先校验名字再落任何文件：它同时是 module 路径、目录名与文件名，写坏了要到生成到一半才现形。
	if err := validateModuleName(name); err != nil {
		return err
	}

	// 解析 --replace / -replace 或 --replace=<path>
	replacePath := ""
	missingOnly := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--missing-only":
			missingOnly = true
		case a == "--replace" || a == "-replace":
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" || strings.HasPrefix(args[i+1], "--") {
				return fmt.Errorf("--replace 后要跟本地 papa 仓库路径")
			}
			replacePath = args[i+1]
			i++
		case strings.HasPrefix(a, "--replace="):
			replacePath = strings.TrimPrefix(a, "--replace=")
			if strings.TrimSpace(replacePath) == "" {
				return fmt.Errorf("--replace 后要跟本地 papa 仓库路径")
			}
		default:
			// 未识别的参数：拼错选项、多传位置参数都该报错，别静默忽略还报"生成成功"
			return fmt.Errorf("未知参数 %q（支持 --replace <本地 papa 仓库路径> 与 --missing-only）", a)
		}
	}
	if replacePath != "" {
		if strings.ContainsAny(replacePath, "\x00\r\n") {
			return fmt.Errorf("--replace 路径不能包含换行或 NUL")
		}
		replacePath = filepath.ToSlash(replacePath) // 兼容 Windows 反斜杠路径
		// go.mod requires quoting a local path containing whitespace or quotes.
		if strings.ContainsAny(replacePath, " \t\"") {
			replacePath = strconv.Quote(replacePath)
		}
	}
	if err := checkScaffoldPath(name); err != nil {
		return err
	}
	if _, err := os.Stat(name); err == nil && !missingOnly {
		return fmt.Errorf("目标 %s 已存在；拒绝覆盖。补齐缺失骨架请显式使用 --missing-only，升级通用文档请用 papa upgrade", name)
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	modPath := filepath.Join(name, "go.mod")
	_, modErr := os.Stat(modPath)
	if missingOnly && modErr == nil {
		if err := validateProjectModule(name); err != nil {
			return err
		}
		module, err := readModulePath(name)
		if err != nil {
			return err
		}
		if module != name {
			return fmt.Errorf("已有 module %q 与项目名 %q 不一致，拒绝混合生成", module, name)
		}
	}

	project := path.Base(name)
	data := scaffoldData{Module: name, Project: project, SiteFunc: sanitizeIdent(project), CodeDir: "internal/", DocsDir: "docs/framework", Replace: replacePath}

	files := []fileSpec{
		{"go.mod.tmpl", "go.mod"},
		{"gitignore.tmpl", ".gitignore"},
		{"main.go.tmpl", "main.go"},
		{"app_run.go.tmpl", filepath.Join("internal", "app", "run.go")},
		{"README.md.tmpl", "README.md"},
		{"docs_README.md.tmpl", filepath.Join("docs", "README.md")},
		{"docs_DEVELOPMENT.md.tmpl", filepath.Join("docs", "operations", "DEVELOPMENT.md")},
		{"config.yaml.tmpl", filepath.Join("configs", "config.yaml")},
		{"config.yaml.tmpl", filepath.Join("configs", "config.yaml.example")},
		{"configs_sites_site.go.tmpl", filepath.Join("internal", "sites", "{{.Project}}.go")},
		{"fetch_catalog.go.tmpl", filepath.Join("internal", "fetcher", "fetch_catalog.go")},
		{"models_models.go.tmpl", filepath.Join("internal", "models", "models.go")},
		{"monitor_register.go.tmpl", filepath.Join("internal", "monitor", "register.go")},
		{"monitor_router.go.tmpl", filepath.Join("internal", "monitor", "router.go")},
		{"monitor_middleware.go.tmpl", filepath.Join("internal", "monitor", "middleware.go")},
		{"monitor_tables.go.tmpl", filepath.Join("internal", "monitor", "tables.go")},
		{"monitor_controller_ping.go.tmpl", filepath.Join("internal", "monitor", "controller", "ping.go")},
		{"monitor_view.go.tmpl", filepath.Join("internal", "monitor", "view", "view.go")},
		{"Dockerfile.tmpl", filepath.Join("deploy", "docker", "Dockerfile")},
		{"docker-compose.yml.tmpl", filepath.Join("deploy", "docker", "docker-compose.yml")},
		{"Makefile.tmpl", "Makefile"},
	}
	// 随项目携带的开发技能（Claude Code 从 <项目>/.claude/skills/ 自动发现 —— 见 docs/CLI.md 的生成树）：
	// 让每个新业务项目开箱就有一份"怎么写、怎么审"的 skill。清单与 `papa upgrade` 共用一份
	// （cmd/papa/skill.go），免得出现"模板加了文件、升级那条路却不知道"的漂移。
	for _, sf := range skillFiles() {
		files = append(files, fileSpec{sf.tmpl, sf.rel})
	}

	for _, f := range files {
		dest, err := renderStr(f.dest, data) // 文件名也走一遍模板（sites/<模块名>.go）
		if err != nil {
			return err
		}
		if err := render(f.tmpl, filepath.Join(name, dest), data, missingOnly); err != nil {
			return err
		}
	}

	// docs：复制使用手册，方便查用法 / 喂给 Claude 写 fetcher、model
	if err := copyDocs(filepath.Join(name, "docs", "framework"), missingOnly); err != nil {
		return err
	}

	// logs 目录：日志由框架托管，这里只占位保证目录存在
	if err := writeScaffoldFile(filepath.Join(name, "logs", ".gitkeep"), nil, missingOnly); err != nil {
		return err
	}

	fmt.Printf("project %q generated.\n", name)
	if missingOnly {
		fmt.Println("只补缺失文件；已有文件（含 go.mod、配置与业务代码）保持原样。请检查新补的站点声明与入口后再启动。")
	}
	fmt.Printf("next: cd %s && go mod tidy\n", name)
	fmt.Printf("      建表：papa migrate（在项目目录里跑，会连业务模型一起建；等价于 make migrate）\n")
	fmt.Printf("      要建令牌得先有表；后台登录：papa token add --operator <名字>\n")
	return nil
}

// renderStr 渲染一段（用于带 {{.Module}} 的文件名）。
func renderStr(s string, data scaffoldData) (string, error) {
	t, err := template.New("str").Parse(s)
	if err != nil {
		return "", err
	}
	var buf strings.Builder
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

func render(tmplName, dest string, data scaffoldData, missingOnly bool) error {
	tmpl, err := template.ParseFS(tmplFS, "templates/"+tmplName)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return err
	}

	// 模板里的注释对齐不可能手写准，生成时过一遍 gofmt：脚手架产物是用户照抄的范本，
	// 出来就该是格式正确的（go/format 在进程内，没有外部依赖）。
	if strings.HasSuffix(dest, ".go") {
		src, err := format.Source(buf.Bytes())
		if err != nil {
			return fmt.Errorf("生成的 %s 不是合法 Go（模板改坏了？）：%w", dest, err)
		}
		return writeScaffoldFile(dest, src, missingOnly)
	}
	return writeScaffoldFile(dest, buf.Bytes(), missingOnly)
}

func copyDocs(destDir string, missingOnly bool) error {
	for _, name := range embeddedDocs() {
		data, err := docs.Files.ReadFile(name)
		if err != nil {
			return err
		}
		dst := filepath.Join(destDir, name)
		if err := writeScaffoldFile(dst, data, missingOnly); err != nil {
			return err
		}
	}
	return nil
}
