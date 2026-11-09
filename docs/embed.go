// Package docs 内嵌框架使用手册，供 papa new 复制到业务项目的 docs/framework/。
// papa upgrade 同时兼容旧项目的 docs/ 布局。
package docs

import "embed"

// Files 仅内嵌顶层 Markdown 手册；issues/ 和 superpowers/ 历史记录不分发。
//
//go:embed *.md
var Files embed.FS
