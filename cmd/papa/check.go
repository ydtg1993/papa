package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// 本文件是 `papa check`：**不用起库**把站点与阶段声明验一遍。
//
// 为什么要在项目里生成一个小程序再跑：站点与阶段声明是**项目自己的 Go 代码**
//（`configs/sites/*.go` 在 init() 里自登记），只有把它编译进去才读得到 —— CLI 是另一个模块，
// 看不见项目的包。而 `RegisterSites` 那套校验平时要连库、建池才会跑到。
// 生成的那个程序只调框架的**纯校验** `papa.CheckSites`（阶段名重复、三个治理队列漏写/缺项、
// 熔断阈值、Delay 与 Backoff 的写法），所以不连库、不建池、不改任何状态。
//
// 生成的目录是项目里的 `.papa-check/`（点开头，工具链与编辑器默认不当源码看），跑完立刻删。

// 两个模板都用 strings.ReplaceAll 填占位符（不走 Sprintf）：模板里的 % 与反引号都不必转义。
// 模板内容也刻意**不写换行转义** —— 单行输出用 Println，多行用反引号原串，这样生成出来的
// 代码里没有一处字符转义，改模板时不会因为一个反斜杠写坏（本文件就踩过这个坑）。
const checkProgramTmpl = `// 由 papa check 生成的一次性校验程序：跑完就删，别提交。
package main

import (
	"fmt"
	"os"

	"github.com/ydtg1993/papa/v3"
	"github.com/ydtg1993/papa/v3/config"

	// 匿名导入：让各站点文件的 init() 跑起来（它们自己登记到框架）。
	_ __SITES_IMPORT__
)

func main() {
	cfg, warn, err := loadConfig(__CONFIG__)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if warn != "" {
		fmt.Println("提示：" + warn)
	}
	sites := papa.Sites()
	if len(sites) == 0 {
		fmt.Fprintln(os.Stderr, "没有收集到任何站点声明 —— configs/sites/ 里的 init() 登记了吗（papa.RegisterSite）？")
		os.Exit(1)
	}
	if err := papa.CheckSites(cfg, sites...); err != nil {
		fmt.Fprintln(os.Stderr, "站点与阶段声明有问题：")
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println(fmt.Sprintf("✓ %d 个站点声明都合法（阶段名、三个治理队列、熔断阈值、Delay / Backoff 写法都查过）", len(sites)))
}

// loadConfig 读配置：框架的配置校验是 panic 式的（"启动即失败"那条），这里转成 error 交给人看。
// 文件不存在不算失败（配置可能放在别处/由 -c 指定），给一句提示，继续只验声明。
func loadConfig(path string) (cfg *config.Config, warn string, err error) {
	defer func() {
		if r := recover(); r != nil {
			cfg, err = nil, fmt.Errorf("读 %s 失败：%v", path, r)
		}
	}()
	c, err := config.Load(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Sprintf("%s 不存在，这次只验声明（配置的键与值域没查）", path), nil
		}
		return nil, "", fmt.Errorf("读 %s 失败：%w", path, err)
	}
	return c, "", nil
}
`

// checkProgram 组装那个一次性校验程序的源码。
func checkProgram(module, configPath string) string {
	return fillProgram(checkProgramTmpl, module, configPath)
}

// fillProgram 填占位符：两个占位符都落在**字符串字面量**的位置上，所以先 Quote
// （Windows 路径里的反斜杠、模块名里的怪字符都不会写坏生成物）。
func fillProgram(tmpl, module, configPath string) string {
	src := strings.ReplaceAll(tmpl, "__SITES_IMPORT__", strconv.Quote(module+"/configs/sites"))
	return strings.ReplaceAll(src, "__CONFIG__", strconv.Quote(configPath))
}

// runCheckCmd `papa check [目录] [-c <配置路径>]`。
func runCheckCmd(args []string) error {
	dir := "."
	cfgPath := "" // 空 = 用 <目标目录>/configs/config.yaml（**不是**当前目录的）
	boot := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--boot":
			// 再往前一步：连库把启动路径跑到 Run 之前（不投入口任务、不起池、不抓页面）
			boot = true
		case a == "-c" || a == "--config":
			if i+1 >= len(args) {
				return fmt.Errorf("%s 后面要跟配置路径", a)
			}
			cfgPath = args[i+1]
			i++
		case strings.HasPrefix(a, "--config="):
			cfgPath = strings.TrimPrefix(a, "--config=")
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("未知参数 %q（用法：papa check [目录] [--boot] [-c <配置路径>]）", a)
		default:
			dir = a
		}
	}

	root, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if info, err := os.Stat(root); err != nil || !info.IsDir() {
		return fmt.Errorf("找不到目录 %s", root)
	}
	module, err := readModulePath(root)
	if err != nil {
		return err
	}
	// 生成的程序工作目录是**项目根**，所以配置路径先定死：
	// 没给 -c 就用项目自己的 configs/config.yaml；给了相对路径则按**当前目录**解析成绝对路径。
	switch {
	case cfgPath == "":
		cfgPath = filepath.Join(root, "configs", "config.yaml")
	case !filepath.IsAbs(cfgPath):
		abs, err := filepath.Abs(cfgPath)
		if err != nil {
			return err
		}
		cfgPath = abs
	}

	tmp := filepath.Join(root, ".papa-check")
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	// 跑完就删（含出错时）；删不掉也只提示一句 —— 那是项目的目录，不该在这儿失败
	defer func() {
		if err := os.RemoveAll(tmp); err != nil {
			fmt.Fprintf(os.Stderr, "提示：临时目录没删掉，手动删掉即可：%s（%v）\n", tmp, err)
		}
	}()
	src := checkProgram(module, cfgPath)
	if boot {
		src = bootProgram(module, cfgPath)
	}
	if err := os.WriteFile(filepath.Join(tmp, "main.go"), []byte(src), 0o644); err != nil {
		return err
	}

	if boot {
		fmt.Printf("正在连库跑一遍 %s 的启动路径（读到 RegisterSites 为止，不 Run）…\n", root)
	} else {
		fmt.Printf("正在校验 %s 的站点与阶段声明（临时编译一次，不连库）…\n", root)
	}

	// 输出原样透给用户，同时留一份用来判定"是不是还没 go mod tidy" —— 那种失败 go 的原话
	//（"replaced but not required; to add it: go get …"）对刚生成的脚手架不够直白。
	var said bytes.Buffer
	cmd := exec.Command("go", "run", "./"+filepath.Base(tmp))
	cmd.Dir = root
	cmd.Stdout = io.MultiWriter(os.Stdout, &said)
	cmd.Stderr = io.MultiWriter(os.Stderr, &said)
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			if out := said.String(); strings.Contains(out, "not required") ||
				strings.Contains(out, "no required module provides package") {
				fmt.Fprintln(os.Stderr, "\n提示：项目的 go.mod 里还没有这些依赖的 require（脚手架生成的 go.mod 本来就不带）"+
					" —— 先跑一次 `go mod tidy` 再来 `papa check`。")
			}
			return fmt.Errorf("没通过（原因见上面）")
		}
		return fmt.Errorf("跑校验程序失败：%w", err)
	}
	return nil
}

// readModulePath 从 go.mod 读 module 路径：生成的程序要 import <module>/configs/sites。
func readModulePath(root string) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", fmt.Errorf("读 %s：%w（站点声明要在项目里编译，得有 go.mod）", filepath.Join(root, "go.mod"), err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, "module "); ok {
			if p := strings.TrimSpace(after); p != "" {
				return p, nil
			}
		}
	}
	return "", fmt.Errorf("%s 里没有 module 行", filepath.Join(root, "go.mod"))
}

// bootProgramTmpl 是 `--boot` 那版：**连库**把启动路径跑到 Run 之前 ——
// 就是 main.go 里的 papa.New() + app.RegisterSites(papa.Sites()...)，**不 Run**
// （不投入口任务、不建池、不启 HTTP 服务、不抓任何页面）。声明没问题、库也通，这一步才会过。
const bootProgramTmpl = `// 由 papa check --boot 生成的一次性启动自检：跑完就删，别提交。
//
// 走的是真实的启动路径（读配置 → 连库 → RegisterSites），但**到此为止**：不 Run，
// 所以不会投入口任务、不会跑启动恢复、不会开 HTTP 服务、也不会抓任何页面。
package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/ydtg1993/papa/v3"
	"github.com/ydtg1993/papa/v3/config"

	// 匿名导入：让各站点文件的 init() 跑起来（它们自己登记到框架）。
	_ __SITES_IMPORT__
)

const configPath = __CONFIG__

func main() {
	// ① 先验声明（纯校验，不碰库）——有问题就不必连库了，报错也最直白
	cfg, err := loadConfig(configPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	sites := papa.Sites()
	if len(sites) == 0 {
		fmt.Fprintln(os.Stderr, "没有收集到任何站点声明 —— configs/sites/ 里的 init() 登记了吗（papa.RegisterSite）？")
		os.Exit(1)
	}
	if err := papa.CheckSites(cfg, sites...); err != nil {
		fmt.Fprintln(os.Stderr, "站点与阶段声明有问题：")
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// ② 真跑一遍启动路径的前半段：读配置 + 连库 + 建引擎
	app, err := papa.New(papa.WithConfigPath(configPath))
	if err != nil {
		fmt.Fprintln(os.Stderr, "papa.New 失败（配置 / 连库）：", err)
		os.Exit(1)
	}

	// ③ 注册：声明有问题、或声明与引擎接线对不上，都会在这条链上暴露
	if err := register(app, sites); err != nil {
		fmt.Fprintln(os.Stderr, "注册失败（真启动时 panic 的就是这一步）：")
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	for _, s := range sites {
		if s.Key == "" {
			continue // 未归属（默认 scope）没有站点快照
		}
		if _, ok := app.Engine.Site(s.Key); !ok {
			fmt.Println("站点", s.Key, "注册完却没进引擎（SetSite 没跑到？）")
			os.Exit(1)
		}
	}

	fmt.Printf("✓ 注册通过：%d 个站点、%d 个阶段（连库跑的是启动路径前半段，没有 Run：", len(sites), countStages(sites))
	fmt.Println("不投入口任务、不跑启动恢复、不开 HTTP 服务、不抓任何页面）")
	for _, s := range sites {
		fmt.Println("    " + siteName(s) + "：" + strings.Join(stageNames(s), " / "))
	}
}

// register 就是 main.go 里那一行；RegisterSites 是 panic 式的，这里转成 error。
func register(app *papa.App, sites []papa.SiteSpec) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	app.RegisterSites(sites...)
	return nil
}

func countStages(sites []papa.SiteSpec) int {
	n := 0
	for _, s := range sites {
		n += len(s.Stages)
	}
	return n
}

func stageNames(s papa.SiteSpec) []string {
	out := make([]string, 0, len(s.Stages))
	for _, st := range s.Stages {
		if st.Fetcher == nil {
			out = append(out, "(fetcher 为 nil)")
			continue
		}
		out = append(out, st.Fetcher.GetStage())
	}
	return out
}

func siteName(s papa.SiteSpec) string {
	if s.Key == "" {
		return "(未归属 / 默认 scope)"
	}
	return s.Key
}

// loadConfig 同 papa check 那版：框架的配置校验是 panic 式的，这里转成 error。
func loadConfig(path string) (cfg *config.Config, err error) {
	defer func() {
		if r := recover(); r != nil {
			cfg, err = nil, fmt.Errorf("读 %s 失败：%v", path, r)
		}
	}()
	return config.Load(path)
}
`

func bootProgram(module, configPath string) string {
	return fillProgram(bootProgramTmpl, module, configPath)
}
