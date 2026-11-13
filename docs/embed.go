// Package docs 内嵌框架使用手册，供 papa new 复制到业务项目的 docs/framework/。
// papa upgrade 仅刷新 docs/framework/ 中的框架副本。
package docs

import "embed"

// Files 内嵌顶层 Markdown 手册和目录职责分册；issues/ 和 superpowers/ 历史记录不分发。
//
//go:embed *.md package_structure/*.md
var Files embed.FS
