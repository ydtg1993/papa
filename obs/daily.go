package obs

import (
	"fmt"
	"sort"
	"time"

	"github.com/ydtg1993/papa/v3/obs/eventlog"
)

// LoadDailySources 把一个事件窗口按**本地日历日**拆成多份 Sources（有事件的每天一份，按日期升序）。
//
// 报告产物按天拆分（`<dir>/<2006-01-02>/`）时用它：先把事件读一遍拿到窗口内有事件的日期，
// 再对每天用 LoadSources 读那一天的输入（事件按 [当日 0 点, 次日 0 点) 过滤，日志证据也按那天索引，
// 与单份报告同一套 5 分钟富余）。返回空切片表示事件文件里没有落在窗口内的事件。
//
// 为什么逐日重读而不是"读一遍再切"：每天的报告要带**当天自己的**事件元信息（条数 / 缺口 / 坏行）
// 与日志证据，重读才能让这些口径对得上；重读成本 = 天数 × 事件文件大小，报告只在退出/显式
// 生成时跑，值得用这一点开销换"日报之间不串数据"。
func LoadDailySources(sc SourceConfig) ([]*Sources, error) {
	if sc.EventsFile == "" {
		return nil, fmt.Errorf("事件文件路径为空")
	}
	paths, err := eventlog.Discover(sc.EventsFile)
	if err != nil {
		return nil, fmt.Errorf("扫描事件文件: %w", err)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("没有找到事件文件 %s —— 确认 obs.events.enabled 已打开并跑过一轮", sc.EventsFile)
	}
	res, err := eventlog.Read(paths, sc.Since, sc.Until)
	if err != nil {
		return nil, fmt.Errorf("读取事件: %w", err)
	}
	evs := res.Events
	if sc.RunID != "" {
		evs = FilterRunID(evs, sc.RunID)
		if len(evs) == 0 {
			return nil, fmt.Errorf("事件里没有 run_id=%s 的记录", sc.RunID)
		}
	}

	loc := sc.Loc
	if loc == nil {
		loc = time.Local
	}
	// 收集有事件的本地日期：按本地时区落成当天 0 点，去重后升序。
	days := make([]time.Time, 0, 8)
	seen := make(map[string]bool, 8)
	for _, ev := range evs {
		if ev.TS.IsZero() {
			continue
		}
		d := ev.TS.In(loc)
		day := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, loc)
		key := day.Format("2006-01-02")
		if seen[key] {
			continue
		}
		seen[key] = true
		days = append(days, day)
	}
	sort.Slice(days, func(i, j int) bool { return days[i].Before(days[j]) })

	out := make([]*Sources, 0, len(days))
	for _, day := range days {
		next := day.AddDate(0, 0, 1)
		dsc := sc
		// 用户显式给的 --since/--until 要**收紧到当天窗口的并集**里：只影响"哪些天"会被拆分，
		// 但每天的报告仍只覆盖用户要的那一段（比如 --since 12:00 时当天的窗口从 12:00 起，
		// 而不是把整天的数据都算进来）。
		dsc.Since = day
		if !sc.Since.IsZero() && sc.Since.After(dsc.Since) {
			dsc.Since = sc.Since
		}
		// eventlog 的窗口是闭区间 [from, to]：用次日零点前 1ns 兜住当天最后一刻，
		// 避免零点整的事件同时算进相邻两天。
		dsc.Until = next.Add(-time.Nanosecond)
		if !sc.Until.IsZero() && sc.Until.Before(dsc.Until) {
			dsc.Until = sc.Until
		}
		src, err := LoadSources(dsc)
		if err != nil {
			return nil, fmt.Errorf("读取 %s 的输入: %w", day.Format("2006-01-02"), err)
		}
		out = append(out, src)
	}
	return out, nil
}
