package main

import (
	"os"
	"path"
	"path/filepath"
	"strings"
)

// 本文件是「随项目携带的开发技能」的唯一清单：`papa new` 按它写入、`papa upgrade` 按它刷新。
// 两处共用一份，是为了不再出现"模板加了文件、升级那条路却不知道"的漂移。
//
// 技能落在 `<项目>/.claude/skills/<skillName>/` —— Claude Code 从项目级 `.claude/skills/` 自动发现
// （见 docs/CLI.md 的生成树）。所以生成的 `.gitignore` 特意放行这一目录。

const skillName = "papa-crawler-development"

// fileSpec 一个要写出的文件：模板名 → 项目里的相对路径。
type fileSpec struct {
	tmpl string
	dest string
}

// skillFile 一项技能文件。**两类，upgrade 对它们的处置不同**：
//
//   - refreshed=true（通用文件）：框架所有，与框架不一致就刷新 —— 和 `docs/` 同一待遇。
//   - refreshed=false（项目所有）：只在**缺失**时播种一次，之后一律不碰。
//     `project-conventions.md` 记的是「本项目自己」的布局与已知漂移，框架去覆盖就是
//     "控制面吃掉数据面"（与 `configs/sites/*.go`、`main.go` 属于业务那类同一个道理）。
type skillFile struct {
	tmpl      string
	rel       string
	refreshed bool
}

// skillFiles 技能的全部文件（顺序即写入顺序）。
func skillFiles() []skillFile {
	dir := filepath.Join(".claude", "skills", skillName)
	refs := filepath.Join(dir, "references")
	return []skillFile{
		{"skill_SKILL.md.tmpl", filepath.Join(dir, "SKILL.md"), true},
		{"skill_doc-map.md.tmpl", filepath.Join(refs, "doc-map.md"), true},
		{"skill_implementation-patterns.md.tmpl", filepath.Join(refs, "implementation-patterns.md"), true},
		{"skill_review-checklist.md.tmpl", filepath.Join(refs, "review-checklist.md"), true},
		{"skill_project-conventions.md.tmpl", filepath.Join(refs, "project-conventions.md"), false},
	}
}

// skillContent 渲染一份技能文件的内容（**不落盘**）：升级要先比对再决定写不写。
func skillContent(tmplName string, data scaffoldData) ([]byte, error) {
	raw, err := tmplFS.ReadFile("templates/" + tmplName)
	if err != nil {
		return nil, err
	}
	s, err := renderStr(string(raw), data)
	if err != nil {
		return nil, err
	}
	return []byte(s), nil
}

// skillData 渲染技能模板用的数据：`project-conventions.md` 里要写进项目自己的阶段前缀（`{{.Module}}`）。
func skillData(root string) scaffoldData {
	m := moduleName(root)
	return scaffoldData{Module: m, SiteFunc: sanitizeIdent(m)}
}

// moduleName 从项目 go.mod 的 `module` 行取项目名；读不到就退回目录名。
// 取 module 路径的**最后一段**（`github.com/you/mycrawler` → `mycrawler`）——
// 与脚手架用 `papa new <name>` 生成时的那个 name 对得上。
func moduleName(root string) string {
	if data, err := os.ReadFile(filepath.Join(root, "go.mod")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module ")
			if !ok {
				continue
			}
			if m := path.Base(strings.TrimSpace(rest)); m != "" && m != "." && m != "/" {
				return m
			}
		}
	}
	return filepath.Base(root)
}
