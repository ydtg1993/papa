// Package docs 内嵌框架使用手册，供脚手架（papa new）复制到业务项目的 docs/ 目录。
package docs

import "embed"

// Files 内嵌 docs 目录下所有 markdown 文件。
//
//go:embed *.md
var Files embed.FS
