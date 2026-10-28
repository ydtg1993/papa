package report

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// makeReportDir 在 root 下造一个目录；isReport=true 时带上 report.md（Prune 只认这种目录）。
func makeReportDir(t *testing.T, root, name string, isReport bool, mod time.Time) {
	t.Helper()
	dir := filepath.Join(root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if isReport {
		if err := os.WriteFile(filepath.Join(dir, "report.md"), []byte("# report\n"), 0o644); err != nil {
			t.Fatalf("write report: %v", err)
		}
	}
	if err := os.Chtimes(dir, mod, mod); err != nil {
		t.Fatalf("chtimes %s: %v", dir, err)
	}
}

// 日报目录按"那一天 0 点"算年龄；旧命名（run_id / window-*）按目录修改时间算；
// 没有 report.md 的目录一律不碰。
func TestPruneExpiredReportDirs(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 10, 28, 15, 0, 0, 0, time.Local)
	oldDay := DayName(now.AddDate(0, 0, -10))
	recentDay := DayName(now.AddDate(0, 0, -1))

	makeReportDir(t, root, oldDay, true, now)                                      // 名字里的日期太老 → 删
	makeReportDir(t, root, recentDay, true, now)                                   // 名字里的日期在保留期内 → 留
	makeReportDir(t, root, "window-20260101-000000", true, now.AddDate(0, 0, -30)) // 旧命名、mtime 太老 → 删
	makeReportDir(t, root, "r-fresh", true, now.AddDate(0, 0, -2))                 // 旧命名、mtime 在保留期内 → 留
	makeReportDir(t, root, "notes", false, now.AddDate(0, 0, -100))                // 没有 report.md → 不碰
	makeReportDir(t, root, DayName(now.AddDate(0, 0, -100)), false, now)           // 名字老但没产物 → 不碰
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write root file: %v", err)
	}

	removed, err := Prune(root, 7*24*time.Hour, now, time.Local)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	want := []string{oldDay, "window-20260101-000000"}
	if !reflect.DeepEqual(removed, want) {
		t.Fatalf("removed = %v, want %v", removed, want)
	}
	for _, kept := range []string{recentDay, "r-fresh", "notes"} {
		if _, err := os.Stat(filepath.Join(root, kept)); err != nil {
			t.Errorf("不该删 %s：%v", kept, err)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "README.md")); err != nil {
		t.Errorf("根目录的文件不该被删：%v", err)
	}
}

// 负数保留期 = 永久保留：一个都不删（连旧命名也不碰）。
func TestPruneNegativeKeepsEverything(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 10, 28, 15, 0, 0, 0, time.Local)
	makeReportDir(t, root, DayName(now.AddDate(0, 0, -100)), true, now)
	makeReportDir(t, root, "window-old", true, now.AddDate(0, 0, -100))

	removed, err := Prune(root, -1*time.Hour, now, time.Local)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("负数保留期不应删任何东西，实删 %v", removed)
	}
}

// retention=0（宿主没走 config.Load 归一时的零值）按默认 7 天算，绝不能把今天的报告删了。
func TestPruneZeroUsesDefaultSevenDays(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 10, 28, 15, 0, 0, 0, time.Local)
	today := DayName(now)
	makeReportDir(t, root, today, true, now)
	makeReportDir(t, root, DayName(now.AddDate(0, 0, -3)), true, now)
	makeReportDir(t, root, DayName(now.AddDate(0, 0, -10)), true, now)

	removed, err := Prune(root, 0, now, time.Local)
	if err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if !reflect.DeepEqual(removed, []string{DayName(now.AddDate(0, 0, -10))}) {
		t.Fatalf("removed = %v, want 只删 10 天前那份", removed)
	}
	if _, err := os.Stat(filepath.Join(root, today)); err != nil {
		t.Fatalf("今天的报告被删了：%v", err)
	}
}

// 报告目录不存在不算错误（还没生成过报告）。
func TestPruneMissingRoot(t *testing.T) {
	removed, err := Prune(filepath.Join(t.TempDir(), "nope"), 7*24*time.Hour, time.Now(), time.Local)
	if err != nil {
		t.Fatalf("目录不存在不应报错：%v", err)
	}
	if len(removed) != 0 {
		t.Fatalf("removed = %v, want 空", removed)
	}
}
