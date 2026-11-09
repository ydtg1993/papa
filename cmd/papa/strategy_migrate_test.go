package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStrategyInspectionRejectsWriteCommandsAndUnsafeManifest(t *testing.T) {
	for _, args := range [][]string{{"apply"}, {"drop"}, {"inspect", "--write"}, {"inspect", "unexpected"}} {
		if runStrategyMigrateCmd(args) == nil {
			t.Fatal("accepted unsupported mutation", args)
		}
	}
	path := filepath.Join(t.TempDir(), "manifest.json")
	for _, body := range []string{`{"version":2,"tables":[]}`, `{"version":1,"sql":"DROP TABLE x","tables":[]}`, `{"version":1,"tables":[{"name":"x;drop","disposition":"retain"}]}`, `{"version":1,"tables":[{"name":"crawler_tasks","disposition":"retain"}]}`} {
		if e := os.WriteFile(path, []byte(body), 0600); e != nil {
			t.Fatal(e)
		}
		if _, e := readStrategyManifest(path); e == nil {
			t.Fatal("invalid manifest accepted", body)
		}
	}
	if e := os.WriteFile(path, []byte(`{"version":1,"tables":[{"name":"example_sessions","disposition":"retire_after_verified_migration"}]}`), 0600); e != nil {
		t.Fatal(e)
	}
	if rows, e := readStrategyManifest(path); e != nil || len(rows) != 1 {
		t.Fatal(rows, e)
	}
	if e := os.WriteFile(path, []byte(`{"version":1,"tables":[]}`+strings.Repeat(" ", 65536)), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e := readStrategyManifest(path); e == nil {
		t.Fatal("oversized manifest accepted")
	}
}

func TestStrategyInspectionReportNeverOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "report.json")
	if e := writeStrategyInspection(path, []byte("original")); e != nil {
		t.Fatal(e)
	}
	if e := writeStrategyInspection(path, []byte("replacement")); e == nil {
		t.Fatal("report overwritten")
	}
	b, e := os.ReadFile(path)
	if e != nil || string(b) != "original" {
		t.Fatal(string(b), e)
	}
}
