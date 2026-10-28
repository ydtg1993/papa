package obs

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// eventLine 造一行最小可用的事件 JSONL（只带 eventlog 解析需要的字段）。
func eventLine(seq int, ts time.Time, runID, typ string) string {
	return fmt.Sprintf(`{"v":1,"seq":%d,"ts":%q,"run_id":%q,"type":%q}`,
		seq, ts.Format(time.RFC3339Nano), runID, typ)
}

func writeEventFile(t *testing.T, path string, lines []string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatalf("写事件文件: %v", err)
	}
}

// 事件窗口要按**本地日历日**拆开：有事件的每天一份，按日期升序；跨零点的事件只落一天。
func TestLoadDailySourcesSplitsByLocalDay(t *testing.T) {
	loc := time.Local
	day0 := time.Date(2026, 10, 24, 12, 0, 0, 0, loc)
	day1Midnight := time.Date(2026, 10, 25, 0, 0, 0, 0, loc) // 刚好零点 → 属于 10-25
	day2 := time.Date(2026, 10, 26, 12, 0, 0, 0, loc)
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writeEventFile(t, path, []string{
		eventLine(1, day0, "r-a", "run_started"),
		eventLine(2, day0.Add(time.Hour), "r-a", "task_finished"),
		eventLine(1, day1Midnight, "r-b", "run_started"),
		eventLine(1, day2, "r-c", "run_started"),
		eventLine(2, day2.Add(time.Minute), "r-c", "run_stopped"),
		eventLine(3, day2.Add(2*time.Minute), "r-c", "task_finished"),
	})

	sources, err := LoadDailySources(SourceConfig{EventsFile: path, Loc: loc})
	if err != nil {
		t.Fatalf("LoadDailySources: %v", err)
	}
	if len(sources) != 3 {
		t.Fatalf("天数 = %d, want 3（10-24 / 10-25 / 10-26）", len(sources))
	}
	wantDays := []string{"2026-10-24", "2026-10-25", "2026-10-26"}
	wantCounts := []int{2, 1, 3}
	wantRuns := [][]string{{"r-a"}, {"r-b"}, {"r-c"}}
	for i, src := range sources {
		if got := src.Run.From.Format("2006-01-02"); got != wantDays[i] {
			t.Fatalf("第 %d 天 = %s, want %s（应按日期升序）", i, got, wantDays[i])
		}
		if got := src.EventMeta.Count; got != wantCounts[i] {
			t.Errorf("%s 事件数 = %d, want %d", wantDays[i], got, wantCounts[i])
		}
		if got := strings.Join(src.Run.RunIDs, ","); got != strings.Join(wantRuns[i], ",") {
			t.Errorf("%s run_ids = %s, want %v", wantDays[i], got, wantRuns[i])
		}
		// 窗口 = [当天 0 点, 次日 0 点)；From 是日界、To 是日尾（次日零点前 1ns）。
		if got := src.Run.From.Format("15:04:05"); got != "00:00:00" {
			t.Errorf("%s 的窗口起点不是本地日界：%v", wantDays[i], src.Run.From)
		}
		if src.Run.To.Sub(src.Run.From) < 23*time.Hour {
			t.Errorf("%s 的窗口太短：%v", wantDays[i], src.Run.To.Sub(src.Run.From))
		}
	}
	// 10-25 零点整那条只算进 10-25 一天（跨天边界不重复计数）。
	if src := sources[1]; src.Events[0].RunID != "r-b" {
		t.Errorf("零点事件没有落到 10-25：%+v", src.Events[0])
	}
}

// 显式窗口只拆窗口内的天；--run 过滤后只保留该 run 出现过的天。
func TestLoadDailySourcesHonorsWindowAndRunFilter(t *testing.T) {
	loc := time.Local
	day0 := time.Date(2026, 10, 24, 12, 0, 0, 0, loc)
	day1 := time.Date(2026, 10, 25, 12, 0, 0, 0, loc)
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writeEventFile(t, path, []string{
		eventLine(1, day0, "r-a", "run_started"),
		eventLine(1, day1, "r-b", "run_started"),
		eventLine(2, day1.Add(time.Hour), "r-b", "task_finished"),
		eventLine(3, day1.Add(7*time.Hour), "r-b", "task_finished"), // 19:00，用于验证窗口收紧
	})

	// 从 10-25 零点起：只剩 10-25 一天。
	from := time.Date(2026, 10, 25, 0, 0, 0, 0, loc)
	sources, err := LoadDailySources(SourceConfig{EventsFile: path, Since: from, Loc: loc})
	if err != nil {
		t.Fatalf("LoadDailySources: %v", err)
	}
	if len(sources) != 1 || sources[0].Run.From.Format("2006-01-02") != "2026-10-25" {
		t.Fatalf("显式 --since 没裁掉窗口外的天：%+v", sources)
	}

	// 显式 --since/--until 收紧到同一天内：窗口不能被扩成"整天"，只算用户要的那一段。
	sources, err = LoadDailySources(SourceConfig{
		EventsFile: path,
		Since:      time.Date(2026, 10, 25, 18, 0, 0, 0, loc),
		Until:      time.Date(2026, 10, 25, 20, 0, 0, 0, loc),
		Loc:        loc,
	})
	if err != nil {
		t.Fatalf("LoadDailySources(window): %v", err)
	}
	if len(sources) != 1 {
		t.Fatalf("窗口收紧后应只剩 1 天，实得 %d", len(sources))
	}
	if got := sources[0].EventMeta.Count; got != 1 {
		t.Errorf("半天窗口内事件数 = %d, want 1（12:00/13:00 那两条在窗口外）", got)
	}
	if got := sources[0].Run.From.Format("15:04"); got != "18:00" {
		t.Errorf("窗口起点 = %s, want 18:00（不能被扩成当天 0 点）", got)
	}

	// 只看 r-a：只有 10-24 一天。
	sources, err = LoadDailySources(SourceConfig{EventsFile: path, RunID: "r-a", Loc: loc})
	if err != nil {
		t.Fatalf("LoadDailySources(run): %v", err)
	}
	if len(sources) != 1 || sources[0].Run.From.Format("2006-01-02") != "2026-10-24" {
		t.Fatalf("--run 过滤后应只剩 10-24：%+v", sources)
	}
	if got := sources[0].EventMeta.Count; got != 1 {
		t.Errorf("--run 过滤后事件数 = %d, want 1", got)
	}
}

// 窗口内没有事件 → 空切片（不是错误）；事件文件根本不存在才是错误。
func TestLoadDailySourcesEmptyAndMissing(t *testing.T) {
	loc := time.Local
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writeEventFile(t, path, []string{eventLine(1, time.Date(2026, 10, 24, 12, 0, 0, 0, loc), "r-a", "run_started")})

	sources, err := LoadDailySources(SourceConfig{
		EventsFile: path,
		Since:      time.Date(2027, 1, 1, 0, 0, 0, 0, loc),
		Loc:        loc,
	})
	if err != nil {
		t.Fatalf("窗口内没事件不应报错：%v", err)
	}
	if len(sources) != 0 {
		t.Fatalf("窗口内没事件应回空切片，实得 %d 天", len(sources))
	}

	if _, err := LoadDailySources(SourceConfig{EventsFile: filepath.Join(t.TempDir(), "nope.jsonl"), Loc: loc}); err == nil {
		t.Fatal("事件文件不存在应报错")
	}
}
