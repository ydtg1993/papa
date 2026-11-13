// Package docs 内嵌框架使用手册，供 papa new 复制到业务项目的 docs/framework/。
// papa upgrade 仅刷新 docs/framework/ 中的框架副本。
package docs

import "embed"

// Files 只嵌入当前手册、目录职责分册及配套图源/图像，保留分类层级。
// history/ 是仓库审查、设计和计划记录，不随 CLI 分发。
//
//go:embed README.md architecture/*.md configuration/*.md guides/*.md accounts/*.md runtime/*.md operations/*.md package_structure/*.md
//go:embed assets/diagrams/*.mmd assets/diagrams/*.svg assets/diagrams/*.png assets/diagrams/manifest.json
var Files embed.FS
