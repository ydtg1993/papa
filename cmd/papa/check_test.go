package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 生成的校验程序：必须在**项目内**（配置与站点声明都是项目自己的包/文件），
// import 项目的 configs/sites（让 init() 登记跑起来），只调框架的纯校验。
func TestCheckProgramContent(t *testing.T) {
	src := checkProgram("huangguo", "E:/x/huangguo/configs/config.yaml")
	for _, want := range []string{
		`_ "huangguo/configs/sites"`,          // 匿名导入：站点声明自己登记
		"papa.Sites()",                        // 收集到的声明
		"papa.CheckSites(cfg, sites...)",      // 只调纯校验（不连库）
		`"E:/x/huangguo/configs/config.yaml"`, // 配置用绝对路径（程序的工作目录是项目根）
	} {
		if !strings.Contains(src, want) {
			t.Errorf("生成的校验程序里应当有 %q：\n%s", want, src)
		}
	}
	if strings.Contains(src, "papa.New(") || strings.Contains(src, "RegisterSites") {
		t.Error("校验程序不该去建 App / 注册站点 —— 那要连库，就不是「不用起库」了")
	}
}

// --boot 那版：要真的走启动路径（New + RegisterSites），但**绝不能 Run** ——
// Run 会投入口任务、跑启动恢复、开 HTTP 服务，那就不是"确认注册那条链"而是"起爬虫"了。
func TestBootProgramContent(t *testing.T) {
	src := bootProgram("huangguo", "E:/x/huangguo/configs/config.yaml")
	for _, want := range []string{
		`_ "huangguo/configs/sites"`,
		"papa.New(papa.WithConfigPath(configPath))", // 读配置 + 连库 + 建引擎
		"app.RegisterSites(sites...)",               // 要确认的就是这条
		"app.Engine.Site(s.Key)",                    // 接线也核一遍：站点真的进了引擎
		"papa.CheckSites(cfg, sites...)",            // 连库之前先纯校验一遍
	} {
		if !strings.Contains(src, want) {
			t.Errorf("boot 程序里应当有 %q：\n%s", want, src)
		}
	}
	if strings.Contains(src, ".Run(") {
		t.Error("boot 程序绝不能 Run —— 那会投入口任务/跑启动恢复/开 HTTP 服务，等于真起爬虫")
	}
}

func TestCheckRejectsBadArgs(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := runCheckCmd([]string{"--bogus"}); err == nil || !strings.Contains(err.Error(), "未知参数") {
		t.Fatalf("未知参数应当报错，实得 %v", err)
	}
	if err := runCheckCmd([]string{"-c"}); err == nil || !strings.Contains(err.Error(), "要跟配置路径") {
		t.Fatalf("-c 少了值应当报错，实得 %v", err)
	}
	if err := runCheckCmd([]string{"nope"}); err == nil || !strings.Contains(err.Error(), "找不到目录") {
		t.Fatalf("不存在的目录应当报错，实得 %v", err)
	}
	// 没有 go.mod 的目录：站点声明要在项目里编译，得说清为什么不行
	if err := os.MkdirAll("nogomod", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runCheckCmd([]string{"nogomod"}); err == nil || !strings.Contains(err.Error(), "go.mod") {
		t.Fatalf("没有 go.mod 应当报错并提到它，实得 %v", err)
	}
}

// 端到端：真生成一个项目、真编译一次、真跑一遍 —— 这条会花几秒（首编译），
// 但钉住的是"生成的程序真的能编、能跑、能拦住写错的声明"。依赖拉不下来时跳过（离线机器）。
func TestCheckEndToEnd(t *testing.T) {
	repo, err := filepath.Abs("../..") // 测试的工作目录是 cmd/papa，先算；下面要 chdir
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(t.TempDir())
	if err := runNew("demo", []string{"--replace", filepath.ToSlash(repo)}); err != nil {
		t.Fatalf("runNew = %v", err)
	}
	// 脚手架生成的 go.mod 不带 require，靠 `go mod tidy` 落 —— 端到端这一步让它自动补
	//（只在临时项目里，不动真实项目）。
	t.Setenv("GOFLAGS", "-mod=mod")

	var checkErr error
	out := captureStderr(t, func() { checkErr = runCheckCmd([]string{"demo"}) })
	if checkErr != nil {
		for _, offline := range []string{"dial tcp", "module lookup disabled", "missing go.sum entry",
			"no required module provides package"} {
			if strings.Contains(out, offline) {
				t.Skipf("依赖拉不下来（离线/模块缓存不全），跳过端到端：%v\n%s", checkErr, out)
			}
		}
		t.Fatalf("脚手架产物应当校验通过：%v\n%s", checkErr, out)
	}

	// 把一个阶段的 WorkerCount 改成 0 → 必须拦住
	site := filepath.Join("demo", "configs", "sites", "demo.go")
	src := readFile(t, site)
	replaced := strings.Replace(src, "WorkerCount: 1,", "WorkerCount: 0,", 1)
	if replaced == src {
		t.Fatal("没找到要改的 WorkerCount: 1,")
	}
	writeFileT(t, site, replaced)

	checkErr = nil
	out = captureStderr(t, func() { checkErr = runCheckCmd([]string{"demo"}) })
	if checkErr == nil {
		t.Fatal("WorkerCount 为 0，校验应当不过")
	}
	if !strings.Contains(out, "WorkerCount") {
		t.Errorf("报错要点名 WorkerCount：\n%s", out)
	}

	// 临时目录两种结局都不该留下（不然项目的 git status 会多出一坨）
	if _, err := os.Stat(filepath.Join("demo", ".papa-check")); err == nil {
		t.Error("校验完应当删掉 .papa-check/")
	}
}
