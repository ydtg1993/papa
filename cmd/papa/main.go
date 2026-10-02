package main

import (
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/template"

	"github.com/ydtg1993/papa/v2/docs"
)

//go:embed templates/*
var tmplFS embed.FS

type scaffoldData struct {
	Module string
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}
	switch os.Args[1] {
	case "new":
		if len(os.Args) < 3 {
			fmt.Fprintln(os.Stderr, "usage: papa new <project-name> [--replace <local-papa-path>]")
			os.Exit(1)
		}
		if err := runNew(os.Args[2], os.Args[3:]); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
	case "token":
		if err := runTokenCmd(os.Args[2:]); err != nil {
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
	fmt.Fprintln(os.Stderr, "  papa new <project-name> [--replace <local-papa-path>]")
	fmt.Fprintln(os.Stderr, "    生成一个新爬虫项目骨架")
	fmt.Fprintln(os.Stderr, "    --replace  在 go.mod 加 replace 指向本地 papa 仓库（本地验证用，无需先发布）")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "  papa token add --operator <名字>   创建一把后台访问令牌（明文只打印一次）")
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

	// 解析 --replace / -replace 或 --replace=<path>
	replacePath := ""
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--replace" || a == "-replace":
			if i+1 < len(args) {
				replacePath = args[i+1]
				i++
			}
		case strings.HasPrefix(a, "--replace="):
			replacePath = strings.TrimPrefix(a, "--replace=")
		}
	}
	if replacePath != "" {
		replacePath = filepath.ToSlash(replacePath) // 兼容 Windows 反斜杠路径
	}

	data := scaffoldData{Module: name}

	files := []struct {
		tmpl string
		dest string
	}{
		{"go.mod.tmpl", "go.mod"},
		{"gitignore.tmpl", ".gitignore"},
		{"main.go.tmpl", "main.go"},
		{"config.yaml.tmpl", filepath.Join("configs", "config.yaml")},
		{"fetch_catalog.go.tmpl", filepath.Join("fetcher", "fetch_catalog.go")},
		{"content.go.tmpl", filepath.Join("models", "content.go")},
		{"Dockerfile.tmpl", filepath.Join("docker", "Dockerfile")},
		{"docker-compose.yml.tmpl", filepath.Join("docker", "docker-compose.yml")},
		{"Makefile.tmpl", "Makefile"},
	}

	for _, f := range files {
		if err := render(f.tmpl, filepath.Join(name, f.dest), data); err != nil {
			return err
		}
	}

	// docs：复制使用手册，方便查用法 / 喂给 Claude 写 fetcher、model
	if err := copyDocs(filepath.Join(name, "docs")); err != nil {
		return err
	}

	// 本地验证：把依赖指向本地 papa 仓库，避免先发布版本
	if replacePath != "" {
		if err := appendReplace(filepath.Join(name, "go.mod"), replacePath); err != nil {
			return err
		}
	}

	// logs 目录：日志由框架托管，这里只占位保证目录存在
	if err := os.MkdirAll(filepath.Join(name, "logs"), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(name, "logs", ".gitkeep"), nil, 0o644); err != nil {
		return err
	}

	// 生成白名单持久化文件，供 config.yaml 的 server.whitelist_file 引用
	if err := os.WriteFile(filepath.Join(name, "configs", "whitelist"), []byte("# 每行一个 IP 或 CIDR，留空 = 不限制\n"), 0o600); err != nil {
		return err
	}

	fmt.Printf("project %q generated.\n", name)
	fmt.Printf("next: cd %s && go mod tidy\n", name)
	fmt.Printf("      登录后台要先建访问令牌：papa token add --operator <名字>\n")
	return nil
}

func appendReplace(goModPath, replacePath string) error {
	f, err := os.OpenFile(goModPath, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "\nreplace github.com/ydtg1993/papa/v2 => %s\n", replacePath)
	return err
}

func render(tmplName, dest string, data scaffoldData) error {
	tmpl, err := template.ParseFS(tmplFS, "templates/"+tmplName)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer f.Close()
	return tmpl.Execute(f, data)
}

func copyDocs(destDir string) error {
	entries, err := docs.Files.ReadDir(".")
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := docs.Files.ReadFile(e.Name())
		if err != nil {
			return err
		}
		dst := filepath.Join(destDir, e.Name())
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}
