package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// validateProjectModule rejects unsupported Papa modules before writes or database setup.
// It never rewrites imports, dependencies, configuration, or stored data.
func validateProjectModule(root string) error {
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
			module := strings.Trim(field, `"`)
			if module == "github.com/ydtg1993/papa" || module == "github.com/ydtg1993/papa/v1" || module == "github.com/ydtg1993/papa/v2" || module == "github.com/ydtg1993/papa/v3" {
				return fmt.Errorf("不支持 Papa 模块 %s：此 CLI 仅面向全新 /v4 项目；未执行写入或数据库迁移", module)
			}
		}
	}
	return nil
}
