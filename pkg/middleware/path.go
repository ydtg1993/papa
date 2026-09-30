package middleware

import (
	"path/filepath"
	"strings"
)

// SanitizeOutputPath 净化下载输出目录与文件名，防止 `..` 或绝对路径逃逸出输出根目录。
// 返回净化后的 (dir, file)；非法时返回 ok=false。
func SanitizeOutputPath(dir, file string) (string, string, bool) {
	name := filepath.Base(file)
	if name == "" || name == "." || name == ".." || name == string(filepath.Separator) {
		return "", "", false
	}
	cleaned := filepath.Clean(dir)
	if filepath.IsAbs(cleaned) || cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return "", "", false
	}
	if cleaned == "." {
		cleaned = ""
	}
	return cleaned, name, true
}
