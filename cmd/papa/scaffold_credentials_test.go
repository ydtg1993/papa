package main

import (
	"strings"
	"testing"
)

func TestNewConfigUsesPlaceholderCredentials(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := runNew("demo", nil); err != nil {
		t.Fatal(err)
	}
	config := readFile(t, "demo/configs/config.yaml")
	if strings.Contains(config, "root:123456") || !strings.Contains(config, "user:CHANGE_ME@tcp(") {
		t.Error("generated DSN must use user:CHANGE_ME rather than usable example credentials")
	}
	if !strings.Contains(config, "env: dev") || !strings.Contains(config, "SQL 日志可能包含敏感参数") {
		t.Error("development environment must retain its SQL logging semantics and explain sensitive parameter risk")
	}
}
