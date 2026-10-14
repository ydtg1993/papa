package main

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"strings"

	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/docs"
)

// 本文件是 `papa upgrade`：把一个**已经生成过的**业务目录升到当前脚手架。
//
// 为什么不能直接照模板覆盖：生成物一旦落地就归项目所有 —— main.go / monitor/ / models/ / fetcher/ /
// sites/ / config.yaml / go.mod 都会被业务改，覆盖等于把人家写的东西抹掉（而它恰恰是项目的全部价值）。
// 所以这里只做两件框架**有把握**的事：
//
//  1. 刷新**框架副本**：`docs/`（生成时逐字复制的使用手册，以框架为准）。默认只报告，`--write` 才落盘。
//  2. 跑几项**不用起库**就能做的检查：`config.yaml` 能不能被框架读（升级最常见的坑：yaml 里留了框架
//     不再认的键，报错自带"搬去哪儿"的指路）；站点声明里有没有旧类型名 / 缺三条队列。
//
// 业务文件一律不碰，最后打一张"要你自己看"的清单。

// upgradePlan 一次升级检查的结果（先算出来、再打印/落盘 —— 打印与写入都不藏逻辑，便于单测）。
type upgradePlan struct {
	Root string
	// PapaVersion 本 CLI 自带的手册/模板对应哪个 papa 版本（`go install pkg@vX` 时是 vX，
	// 本地 `go build` 出来的是 (devel) —— 那种情况以你手上这份源码为准）。
	PapaVersion string
	// Required 项目 go.mod 里 require 的 papa 版本；ReplacedTo 非空表示它被 replace 到了本地路径。
	Required   string
	ReplacedTo string

	DocUpdates []string // docs/ 下内容与框架不同的（要更新）
	DocAdds    []string // 框架有、项目没有的
	DocStale   []string // 项目 docs/ 下有、框架没有的（可能是你自己加的文档，不删）

	// 随项目携带的开发技能（`.claude/skills/<skillName>/`）。与 docs/ 的区别只有一条：
	// **项目所有的那个文件（project-conventions.md）不刷新**，只在缺失时播种。
	SkillRefresh []string // 通用文件：与框架不一致（或缺失）→ --write 时刷新
	SkillSeed    []string // 项目所有文件：缺失 → 这一次播种（此后归项目）
	SkillKept    []string // 项目所有文件：已存在 → 不碰（哪怕内容与框架不同）

	ConfigPath   string // 相对 Root
	ConfigReport string // 读不了时的报错原文（框架的 panic 文案，自带指路）；空 = 读通了
	ConfigOK     bool

	SiteNotes []string // 站点声明里的可疑点（文本级检查，只是提醒）
	SiteFiles []string
}

// buildUpgradePlan 把一次检查算出来；不打印、不写盘。
func buildUpgradePlan(dir string) (*upgradePlan, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(root)
	if err != nil {
		return nil, fmt.Errorf("找不到目录 %s", root)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s 不是目录", root)
	}
	// 认一下这是不是一个脚手架生成的项目：两样里至少要有一样，否则多半是传错了目录
	if !exists(filepath.Join(root, "configs")) && !exists(filepath.Join(root, "go.mod")) {
		return nil, fmt.Errorf("%s 里既没有 configs/ 也没有 go.mod —— 这不像脚手架生成的项目", root)
	}

	p := &upgradePlan{Root: root, PapaVersion: cliVersion()}
	p.Required, p.ReplacedTo = readPapaRequire(root)
	p.DocAdds, p.DocUpdates, p.DocStale = planDocs(root)
	p.SkillRefresh, p.SkillSeed, p.SkillKept = planSkill(root)

	p.ConfigPath = filepath.Join("configs", "config.yaml")
	if exists(filepath.Join(root, p.ConfigPath)) {
		p.ConfigReport, p.ConfigOK = checkConfig(filepath.Join(root, p.ConfigPath))
	}
	p.SiteFiles, p.SiteNotes = checkSiteDeclarations(root)
	return p, nil
}

// runUpgradeCmd `papa upgrade [dir] [--write]`。
func runUpgradeCmd(args []string) error {
	dir, write := ".", false
	for _, a := range args {
		switch {
		case a == "--write" || a == "-write":
			write = true
		case strings.HasPrefix(a, "-"):
			return fmt.Errorf("未知参数 %q（用法：papa upgrade [目录] [--write]）", a)
		default:
			dir = a
		}
	}

	p, err := buildUpgradePlan(dir)
	if err != nil {
		return err
	}
	printUpgradePlan(p)

	skillPending := len(p.SkillRefresh) + len(p.SkillSeed)
	if write && len(p.DocAdds)+len(p.DocUpdates)+skillPending > 0 {
		if err := writeDocs(p); err != nil {
			return err
		}
		if err := writeSkill(p); err != nil {
			return err
		}
		var parts []string
		if n := len(p.DocAdds) + len(p.DocUpdates); n > 0 {
			parts = append(parts, fmt.Sprintf("%d 份手册", n))
		}
		if skillPending > 0 {
			parts = append(parts, fmt.Sprintf("%d 个技能文件", skillPending))
		}
		fmt.Printf("\n已更新 %s（%s）\n", strings.Join(parts, " + "), p.Root)
	}
	return nil
}

func printUpgradePlan(p *upgradePlan) {
	fmt.Printf("升级检查：%s\n", p.Root)
	fmt.Printf("  本 CLI 的脚手架副本：%s\n", p.PapaVersion)
	switch {
	case p.ReplacedTo != "":
		fmt.Printf("  项目依赖：%s（replace → %s）\n", orDash(p.Required), p.ReplacedTo)
	case p.Required != "":
		fmt.Printf("  项目依赖：%s\n", p.Required)
	default:
		fmt.Printf("  项目依赖：go.mod 里还没有 papa 的 require（`go mod tidy` 之后就位）\n")
	}

	fmt.Printf("\n【框架副本：docs/】\n")
	if len(p.DocAdds)+len(p.DocUpdates) == 0 {
		fmt.Printf("  已是最新（%d 份手册与框架一致）\n", len(embeddedDocs()))
	} else {
		for _, f := range p.DocAdds {
			fmt.Printf("  新增  docs/%s\n", f)
		}
		for _, f := range p.DocUpdates {
			fmt.Printf("  更新  docs/%s\n", f)
		}
		fmt.Printf("  （加 --write 落盘；这些是框架的逐字副本，以框架为准）\n")
	}
	for _, f := range p.DocStale {
		fmt.Printf("  保留  docs/%s（框架里没有这份了 —— 你自己加的，不动）\n", f)
	}

	fmt.Printf("\n【开发技能：.claude/skills/%s/】\n", skillName)
	if len(p.SkillRefresh)+len(p.SkillSeed) == 0 {
		fmt.Printf("  已是最新（%d 个通用文件与框架一致；%d 个项目所有文件不刷新）\n",
			len(skillFiles())-len(p.SkillKept), len(p.SkillKept))
	} else {
		for _, f := range p.SkillRefresh {
			fmt.Printf("  更新  %s\n", filepath.ToSlash(f))
		}
		for _, f := range p.SkillSeed {
			fmt.Printf("  新增  %s（此后归项目所有，升级不再动它）\n", filepath.ToSlash(f))
		}
		fmt.Printf("  （加 --write 落盘；通用文件以框架为准，项目所有的文件只播种不覆盖）\n")
	}

	fmt.Printf("\n【config.yaml】\n")
	switch {
	case !exists(filepath.Join(p.Root, p.ConfigPath)):
		fmt.Printf("  没有 %s（用 -c 指定别的路径？）\n", p.ConfigPath)
	case p.ConfigOK:
		fmt.Printf("  %s 读得通（框架的键与值域校验都过）\n", p.ConfigPath)
	default:
		fmt.Printf("  读不了 —— 这就是启动时会 panic 的那条：\n")
		for _, line := range strings.Split(p.ConfigReport, "\n") {
			fmt.Printf("    %s\n", line)
		}
	}

	fmt.Printf("\n【站点声明】\n")
	if len(p.SiteFiles) == 0 {
		fmt.Printf("  sites/ 下没有 .go（站点与阶段声明都在这儿）\n")
	}
	for _, n := range p.SiteNotes {
		fmt.Printf("  ！%s\n", n)
	}
	if len(p.SiteNotes) == 0 {
		fmt.Printf("  %d 份声明文件，没看到旧的队列类型名，三条队列声明也都在\n", len(p.SiteFiles))
	}

	fmt.Printf("\n【要你自己看（框架不碰业务文件）】\n")
	for _, line := range []string{
		"sites/*.go：三队列必写的值（从 config.yaml 那三段抄过去）；本轮规则见 papa 仓库的 docs/issues/changes.md",
		"go.mod：框架版本。还没发布时，用 `replace github.com/ydtg1993/papa/v3 => <本地 papa 仓库>` 先跑起来",
		"main.go / monitor/ / models/ / fetcher/：模板未动的不必改；动过的看 docs/issues/changes.md 里对应条目",
		"升级后跑一次 `go build ./...` 与 `papa migrate`，再起服务确认",
	} {
		fmt.Printf("  - %s\n", line)
	}
}

// writeDocs 把框架手册写进项目 docs/（只写框架有的那份；项目自己加的文件不动）。
func writeDocs(p *upgradePlan) error {
	dir := filepath.Join(p.Root, "docs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, name := range append(append([]string{}, p.DocAdds...), p.DocUpdates...) {
		data, err := docs.Files.ReadFile(name)
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// planSkill 比对随项目携带的开发技能与框架那份：**通用文件**不一致（或缺失）就刷新；
// **项目所有**的文件只在缺失时播种一次，之后不碰。
func planSkill(root string) (refresh, seed, kept []string) {
	data := skillData(root)
	for _, sf := range skillFiles() {
		cur, err := os.ReadFile(filepath.Join(root, sf.rel))
		missing := err != nil
		want, rerr := skillContent(sf.tmpl, data)
		switch {
		case sf.refreshed:
			if missing || rerr != nil || !sameContent(cur, want) {
				refresh = append(refresh, sf.rel)
			}
		case missing:
			seed = append(seed, sf.rel)
		default:
			kept = append(kept, sf.rel)
		}
	}
	return refresh, seed, kept
}

// writeSkill 落盘技能文件：刷新通用文件 + 播种缺失的项目所有文件。
// **不覆盖已存在的项目所有文件** —— 那正是它和 `docs/` 的区别（见 skillFile 的注释）。
func writeSkill(p *upgradePlan) error {
	data := skillData(p.Root)
	pending := map[string]bool{}
	for _, rel := range append(append([]string{}, p.SkillRefresh...), p.SkillSeed...) {
		pending[rel] = true
	}
	for _, sf := range skillFiles() {
		if !pending[sf.rel] {
			continue
		}
		content, err := skillContent(sf.tmpl, data)
		if err != nil {
			return err
		}
		dst := filepath.Join(p.Root, sf.rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(dst, content, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// planDocs 比对框架手册与项目 docs/：返回 要新增的 / 要更新的 / 项目独有的（只报告，不删）。
func planDocs(root string) (adds, updates, stale []string) {
	dir := filepath.Join(root, "docs")
	embedded := embeddedDocs()
	for _, name := range embedded {
		data, err := docs.Files.ReadFile(name)
		if err != nil {
			continue
		}
		cur, err := os.ReadFile(filepath.Join(dir, name))
		switch {
		case err != nil:
			adds = append(adds, name)
		case !sameContent(cur, data):
			updates = append(updates, name)
		}
	}
	// 项目 docs/ 里框架没有的 .md：可能是自己加的，只列出来（子目录如 docs/samples 不看）
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			if !contains(embedded, e.Name()) {
				stale = append(stale, e.Name())
			}
		}
	}
	sort.Strings(adds)
	sort.Strings(updates)
	sort.Strings(stale)
	return adds, updates, stale
}

// sameContent 比内容时忽略行尾：生成物在不同 git 配置下会是 CRLF，那不是"用户改过"。
func sameContent(a, b []byte) bool {
	return normalizeEOL(string(a)) == normalizeEOL(string(b))
}

func normalizeEOL(s string) string {
	return strings.ReplaceAll(s, "\r\n", "\n")
}

func embeddedDocs() []string {
	entries, err := docs.Files.ReadDir(".")
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// checkConfig 跑一次框架的配置加载（**不需要连库**），把 panic 文案原样带回来。
// 升级时最常见的坑就是这条：yaml 里留着框架不再认的键 —— 那条报错自带"搬去哪儿"的指路。
func checkConfig(path string) (report string, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			report, ok = fmt.Sprint(r), false
		}
	}()
	if _, err := config.Load(path); err != nil {
		return err.Error(), false
	}
	return "", true
}

// checkSiteDeclarations 对 sites/*.go 做**文本级**检查（不改不解析）：
// 旧类型名还在不在、三条队列声明有没有出现过。只是提醒 —— 真正的校验在启动时（RegisterSites）。
func checkSiteDeclarations(root string) (files []string, notes []string) {
	paths, err := filepath.Glob(filepath.Join(root, "sites", "*.go"))
	if err != nil {
		return nil, nil
	}
	sort.Strings(paths)
	all := ""
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		files = append(files, filepath.Base(p))
		all += string(data)
	}
	if len(files) == 0 {
		return nil, nil
	}
	// 三个治理队列现在回到了 config.yaml 的全局段：站点声明里**不该再出现**那几类 *QueueSpec。
	var stale []string
	for _, want := range []string{"papa.QueueSpec", "papa.ErrorQueueSpec", "papa.RecoverQueueSpec", "papa.RepeatQueueSpec"} {
		if strings.Contains(all, want) {
			stale = append(stale, strings.TrimPrefix(want, "papa."))
		}
	}
	if len(stale) > 0 {
		notes = append(notes, "站点声明里还有 "+strings.Join(stale, " / ")+
			" —— 三个治理队列已回 config.yaml 的全局段（error_queue / recover_queue / repeat_queue），"+
			"删掉站点声明里的这些旧类型；见 docs/CORE_CONFIG.md")
	}
	return files, notes
}

// readPapaRequire 从 go.mod 里读 papa 的 require 版本与 replace 目标（纯文本扫，够用且不引依赖）。
func readPapaRequire(root string) (required, replacedTo string) {
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return "", ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(line, "replace github.com/ydtg1993/papa/v3"):
			if i := strings.Index(line, "=>"); i >= 0 {
				replacedTo = strings.TrimSpace(line[i+2:])
			}
		case strings.HasPrefix(line, "github.com/ydtg1993/papa/v3 "):
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				required = fields[1]
			}
		}
	}
	return required, replacedTo
}

// cliVersion 本 CLI 的版本：`go install .../cmd/papa@vX` 时是 vX；本地 go build 出来是 (devel)。
func cliVersion() string {
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" {
		return "(未知)"
	}
	if info.Main.Version == "(devel)" {
		return "(devel —— 本地构建，脚手架以你手上这份源码为准)"
	}
	return info.Main.Version
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
