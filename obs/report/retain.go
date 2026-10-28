package report

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// DayName 返回一份"日报"的目录名（本地日期）：2006-01-02。
//
// 报告产物按本地日历日拆分，一份 = 一天：`<dir>/<DayName>/report.md` 等四份。
// `papa report --run` 的单 run 报告仍用 RunID 当目录名（那是排查单次运行用的），不参与按天拆分。
func DayName(t time.Time) string { return t.Format("2006-01-02") }

// Prune 删除 root 下超过 retention 的旧报告目录，返回被删目录名（升序）。
//
// 判据（保守，宁可少删）：
//   - retention < 0：永久保留，直接返回（不碰文件系统）；
//   - retention == 0：按默认 7 天算（防御宿主没走 config.Load 的归一，不把今天的报告也删了）；
//   - 只看 root 的**直接子目录**，且目录里必须有 report.md —— 报告根目录里手工放的
//     其它东西（笔记、脚本、无关目录）一律不碰；
//   - 目录名是 2006-01-02（日报）就按**那一天 0 点**算年龄（保留最近 N 个自然日，含今天）；
//     其它名字（升级前留下的 run_id / window-<时间>）按目录修改时间算 —— 旧产物也能被同一套
//     保留期带走；
//   - 早于 now-retention 的删。root 不存在不算错误（还没生成过报告）。
func Prune(root string, retention time.Duration, now time.Time, loc *time.Location) ([]string, error) {
	if retention < 0 {
		return nil, nil
	}
	if retention == 0 {
		retention = 7 * 24 * time.Hour
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("读报告目录 %s: %w", root, err)
	}
	if loc == nil {
		loc = time.Local
	}
	cutoff := now.Add(-retention)
	removed := make([]string, 0, 4)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(root, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "report.md")); err != nil {
			continue // 不是报告产物目录（没有 report.md）
		}
		if !reportDirExpired(e.Name(), dir, cutoff, loc) {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			return removed, fmt.Errorf("删除过期报告 %s: %w", dir, err)
		}
		removed = append(removed, e.Name())
	}
	sort.Strings(removed)
	return removed, nil
}

// reportDirExpired 判断一个报告目录是否已过保留期。
//
// 日报目录名能解析成日期 → 用那天 0 点与 cutoff 比：保留最近 N 个自然日（含今天）。
// 旧命名（run_id / window-*）没有日期语义 → 退回目录修改时间。
func reportDirExpired(name, dir string, cutoff time.Time, loc *time.Location) bool {
	if day, err := time.ParseInLocation("2006-01-02", name, loc); err == nil {
		return day.Before(cutoff)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return false
	}
	return info.ModTime().Before(cutoff)
}
