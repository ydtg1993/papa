package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Refuse before writes or database setup: a v3 business must not silently
// fall through to framework-only migration or receive v4 documentation.
func rejectLegacyProject(root string) error {
	b, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(b), "\n") {
		line, _, _ = strings.Cut(line, "//")
		for _, field := range strings.Fields(line) {
			if strings.Trim(field, `"`) == "github.com/ydtg1993/papa/"+"v3" {
				return fmt.Errorf("检测到 Papa /v3 模块：请使用 v3 CLI，或先迁移业务 imports、require 和 replace 到 /v4；v4 CLI 未执行写入或数据库迁移")
			}
		}
	}
	return nil
}
