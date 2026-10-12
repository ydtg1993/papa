package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v3/docs"
)

func writeFileT(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写 %s: %v", path, err)
	}
}

// upgrade 的检查：docs 漂移（内容变了 / 缺了 / 自己加的）、config.yaml 读不读得通、
// 站点声明里的旧类型名与三条队列缺项 —— 这些都不用起库，正是"一个个检查"最花时间的部分。
func TestUpgradePlanDetectsDrift(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := runNew("demo", nil); err != nil {
		t.Fatalf("runNew = %v", err)
	}

	// ① 框架副本：改一份（内容漂了）、删一份（缺了）、再加一份自己的
	writeFileT(t, filepath.Join("demo", "docs", "CORE_CONFIG.md"), "# 本地乱改过的\n")
	if err := os.Remove(filepath.Join("demo", "docs", "MONITOR.md")); err != nil {
		t.Fatal(err)
	}
	writeFileT(t, filepath.Join("demo", "docs", "notes.md"), "# 我自己的笔记\n")
	writeFileT(t, filepath.Join("demo", "docs", "samples", "keep.md"), "# 子目录不动\n")

	// ② config.yaml：脚手架自带的三个队列段是合法的（队列已回 config.yaml，不是"已退休"的旧段）

	// ③ 站点声明写成升级前的样子：还带着旧类型名
	writeFileT(t, filepath.Join("demo", "configs", "sites", "demo.go"), `package sites

import "github.com/ydtg1993/papa/v3"

func init() {
	papa.RegisterSite(papa.SiteSpec{
		Key:        "demo",
		ErrorQueue: &papa.QueueSpec{WorkerCount: 1},
	})
}
`)

	var plan *upgradePlan
	var buildErr error
	captureStdout(t, func() {})
	plan, buildErr = buildUpgradePlan("demo")
	if buildErr != nil {
		t.Fatalf("buildUpgradePlan = %v", buildErr)
	}

	if !contains(plan.DocUpdates, "CORE_CONFIG.md") {
		t.Errorf("改过的副本应当算「更新」：%v", plan.DocUpdates)
	}
	if !contains(plan.DocAdds, "MONITOR.md") {
		t.Errorf("删掉的副本应当算「新增」：%v", plan.DocAdds)
	}
	if !contains(plan.DocStale, "notes.md") {
		t.Errorf("自己加的文档应当列出来（只报告不删）：%v", plan.DocStale)
	}
	if contains(plan.DocStale, "keep.md") {
		t.Errorf("子目录（docs/samples）不该被当成散落的 .md：%v", plan.DocStale)
	}
	if !plan.ConfigOK {
		t.Error("脚手架自带的三个队列段是合法的，config.yaml 应当读得通")
	}
	if len(plan.SiteNotes) != 1 {
		t.Fatalf("站点声明应当报一条（旧类型名 QueueSpec）：%v", plan.SiteNotes)
	}
	if !strings.Contains(strings.Join(plan.SiteNotes, "\n"), "QueueSpec") {
		t.Errorf("旧类型名要报出来：%v", plan.SiteNotes)
	}
}

// 默认只报告：一个字节都不落盘；--write 才写，而且**只写 docs/**。
func TestUpgradeWriteOnlyTouchesDocs(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := runNew("demo", nil); err != nil {
		t.Fatalf("runNew = %v", err)
	}

	coredoc := filepath.Join("demo", "docs", "CORE_CONFIG.md")
	writeFileT(t, coredoc, "# 本地乱改过的\n")
	if err := os.Remove(filepath.Join("demo", "docs", "MONITOR.md")); err != nil {
		t.Fatal(err)
	}
	writeFileT(t, filepath.Join("demo", "docs", "notes.md"), "# 我自己的笔记\n")

	mainBefore := readFile(t, filepath.Join("demo", "main.go"))
	cfgBefore := readFile(t, filepath.Join("demo", "configs", "config.yaml"))
	siteBefore := readFile(t, filepath.Join("demo", "configs", "sites", "demo.go"))

	// ① 不带 --write：报告要列出要改的副本，但什么都不落盘
	out := captureStdout(t, func() {
		if err := runUpgradeCmd([]string{"demo"}); err != nil {
			t.Fatalf("upgrade = %v", err)
		}
	})
	if !strings.Contains(out, "更新  docs/CORE_CONFIG.md") || !strings.Contains(out, "新增  docs/MONITOR.md") {
		t.Fatalf("报告里应当列出要改的副本：\n%s", out)
	}
	if got := readFile(t, coredoc); got != "# 本地乱改过的\n" {
		t.Error("dry-run 不该改文件")
	}

	// ② --write：刷回框架那份，缺的补上，自己加的留着；业务文件一个字节不动
	captureStdout(t, func() {
		if err := runUpgradeCmd([]string{"demo", "--write"}); err != nil {
			t.Fatalf("upgrade --write = %v", err)
		}
	})
	want, err := docs.Files.ReadFile("CORE_CONFIG.md")
	if err != nil {
		t.Fatal(err)
	}
	if got := readFile(t, coredoc); got != string(want) {
		t.Error("--write 之后副本应当与框架逐字一致")
	}
	if _, err := os.Stat(filepath.Join("demo", "docs", "MONITOR.md")); err != nil {
		t.Errorf("缺的副本应当补上：%v", err)
	}
	if got := readFile(t, filepath.Join("demo", "docs", "notes.md")); got != "# 我自己的笔记\n" {
		t.Error("自己加的文档不该被删或改")
	}
	for _, c := range []struct{ name, before string }{
		{"main.go", mainBefore},
		{"configs/config.yaml", cfgBefore},
		{"configs/sites/demo.go", siteBefore},
	} {
		if got := readFile(t, filepath.Join("demo", filepath.FromSlash(c.name))); got != c.before {
			t.Errorf("业务文件 %s 不该被动过", c.name)
		}
	}
}

// 目录传错时说人话（不甩一个 GetFileAttributesEx）。
func TestUpgradeRejectsBadDir(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("empty", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := runUpgradeCmd([]string{"empty"}); err == nil || !strings.Contains(err.Error(), "不像脚手架生成的项目") {
		t.Fatalf("空目录应当报「不像脚手架生成的项目」，实得 %v", err)
	}
	if err := runUpgradeCmd([]string{"nope"}); err == nil || !strings.Contains(err.Error(), "找不到目录") {
		t.Fatalf("不存在的目录应当报「找不到目录」，实得 %v", err)
	}
	if err := runUpgradeCmd([]string{"--bogus"}); err == nil || !strings.Contains(err.Error(), "未知参数") {
		t.Fatalf("不认识的参数应当报错，实得 %v", err)
	}
}

// go.mod 里 papa 的 require / replace 要读出来 —— 升级时首先要看的就是它。
func TestUpgradeReadsPapaVersion(t *testing.T) {
	t.Chdir(t.TempDir())
	// 一份读得通的 config.yaml（log.dir 与 db 连接池是校验层要求的，不写会报）
	writeFileT(t, filepath.Join("p", "configs", "config.yaml"),
		"log:\n  dir: ./logs\ndb:\n  max_idle_conns: 10\n  max_open_conns: 100\n")
	writeFileT(t, filepath.Join("p", "go.mod"), `module p

go 1.25.0

require (
	github.com/ydtg1993/oao v1.3.0
	github.com/ydtg1993/papa/v3 v3.1.0
)

replace github.com/ydtg1993/papa/v3 => E:/go-project/test_papa/papa
`)
	p, err := buildUpgradePlan("p")
	if err != nil {
		t.Fatalf("buildUpgradePlan = %v", err)
	}
	if p.Required != "v3.1.0" || p.ReplacedTo != "E:/go-project/test_papa/papa" {
		t.Fatalf("require/replace 解析 = %q / %q", p.Required, p.ReplacedTo)
	}
	// 新脚手架的 go.mod 不带 require（靠 go mod tidy 落），那是正常的，不该报错
	if !p.ConfigOK {
		t.Errorf("这份 config.yaml 读得通才对，实得：%s", p.ConfigReport)
	}
}

// 没有 configs/sites/ 时不该凭空报"缺三条队列声明"。
func TestUpgradeNoSiteDir(t *testing.T) {
	t.Chdir(t.TempDir())
	writeFileT(t, filepath.Join("p", "configs", "config.yaml"),
		"log:\n  dir: ./logs\ndb:\n  max_idle_conns: 10\n  max_open_conns: 100\n")
	writeFileT(t, filepath.Join("p", "go.mod"), "module p\n\ngo 1.25.0\n")
	p, err := buildUpgradePlan("p")
	if err != nil {
		t.Fatalf("buildUpgradePlan = %v", err)
	}
	if len(p.SiteNotes) != 0 || len(p.SiteFiles) != 0 {
		t.Errorf("没有 configs/sites/ 时不该报站点声明的问题：%v / %v", p.SiteFiles, p.SiteNotes)
	}
	if !p.ConfigOK {
		t.Errorf("这份 config.yaml 读得通才对，实得：%s", p.ConfigReport)
	}
}
