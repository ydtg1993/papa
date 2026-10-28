package logidx

import (
	"compress/gzip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// ---------- 测试辅助 ----------

func writeLog(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", p, err)
	}
	return p
}

func writeGzipLog(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	f, err := os.Create(p)
	if err != nil {
		t.Fatalf("create %s: %v", p, err)
	}
	zw := gzip.NewWriter(f)
	if _, err := zw.Write([]byte(content)); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", p, err)
	}
	return p
}

func buildIndex(t *testing.T, cfg Config) *Index {
	t.Helper()
	idx, err := Build(cfg)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if idx == nil {
		t.Fatal("Build returned nil index")
	}
	return idx
}

func findSignature(t *testing.T, idx *Index, pattern string) Signature {
	t.Helper()
	for _, sig := range idx.Signatures {
		if sig.Pattern == pattern {
			return sig
		}
	}
	var got []string
	for _, sig := range idx.Signatures {
		got = append(got, sig.Pattern)
	}
	t.Fatalf("signature %q not found; got %q", pattern, got)
	return Signature{}
}

func utc(y int, mo time.Month, d, h, mi, s int) time.Time {
	return time.Date(y, mo, d, h, mi, s, 0, time.UTC)
}

// ---------- logfmt ----------

func TestParseLineLogfmt(t *testing.T) {
	line := `time="2026-10-28 10:00:00" level=error msg="fetch alpha: 请求失败\r\n重试 2 次 \"quoted\"，结束" stage=catalog task_id=42 url="https://example.com/a b" note="中文 空格"`
	e, ok := parseLine(line, time.UTC, "engine.log", "engine", 7)
	if !ok {
		t.Fatal("parseLine returned ok=false for a valid line")
	}
	if want := utc(2026, 10, 28, 10, 0, 0); !e.Time.Equal(want) {
		t.Fatalf("Time = %v, want %v", e.Time, want)
	}
	if e.File != "engine.log" || e.Logger != "engine" || e.Line != 7 {
		t.Fatalf("identity = %s/%s:%d, want engine.log/engine:7", e.File, e.Logger, e.Line)
	}
	if e.Level != "error" {
		t.Fatalf("Level = %q, want error", e.Level)
	}
	wantMsg := "fetch alpha: 请求失败\r\n重试 2 次 \"quoted\"，结束"
	if e.Msg != wantMsg {
		t.Fatalf("Msg = %q, want %q", e.Msg, wantMsg)
	}
	if e.Raw != line {
		t.Fatalf("Raw mismatch:\n got %q\nwant %q", e.Raw, line)
	}
	wantFields := map[string]string{
		"stage":   "catalog",
		"task_id": "42",
		"url":     "https://example.com/a b",
		"note":    "中文 空格",
	}
	if !reflect.DeepEqual(e.Fields, wantFields) {
		t.Fatalf("Fields = %#v, want %#v", e.Fields, wantFields)
	}
	for _, k := range []string{"time", "level", "msg"} {
		if _, ok := e.Fields[k]; ok {
			t.Fatalf("Fields must not contain %q", k)
		}
	}
}

func TestParseLineBad(t *testing.T) {
	cases := []string{
		"garbage without logfmt",
		`level=info msg="missing time"`,
		`time="not-a-time" level=info msg="bad time"`,
		`time="2026-10-28 10:00:00" msg="missing level"`,
		`time="2026-10-28 10:00:00" level=info msg="unterminated`,
		`time="2026-10-28 10:00:00" level=info =badkey`,
	}
	for _, line := range cases {
		if _, ok := parseLine(line, time.UTC, "engine.log", "engine", 1); ok {
			t.Errorf("expected ok=false for %q", line)
		}
	}

	// 空 msg 是合法的 logrus 记录。
	line := `time="2026-10-28 10:00:00" level=info msg=""`
	e, ok := parseLine(line, time.UTC, "engine.log", "engine", 1)
	if !ok {
		t.Fatalf("parseLine(%q) = false, want true", line)
	}
	if e.Msg != "" {
		t.Fatalf("Msg = %q, want empty", e.Msg)
	}
}

func TestParseLineUsesLocation(t *testing.T) {
	loc := time.FixedZone("CST", 8*3600)
	line := `time="2026-10-28 10:00:00" level=info msg="x"`
	e, ok := parseLine(line, loc, "sys.log", "sys", 3)
	if !ok {
		t.Fatal("parseLine returned ok=false")
	}
	if want := time.Date(2026, 10, 28, 10, 0, 0, 0, loc); !e.Time.Equal(want) {
		t.Fatalf("Time = %v, want %v", e.Time, want)
	}
}

func TestRawTruncation(t *testing.T) {
	longMsg := strings.Repeat("日", 3000)
	line := `time="2026-10-28 10:00:00" level=info msg="` + longMsg + `"`
	e, ok := parseLine(line, time.UTC, "engine.log", "engine", 1)
	if !ok {
		t.Fatal("parseLine returned ok=false")
	}
	if len(e.Raw) > rawLimit {
		t.Fatalf("Raw length = %d, want <= %d", len(e.Raw), rawLimit)
	}
	if !utf8.ValidString(e.Raw) {
		t.Fatal("Raw is not valid UTF-8")
	}
	if !strings.HasPrefix(line, e.Raw) {
		t.Fatal("Raw is not a prefix of the line")
	}
	if e.Msg != longMsg {
		t.Fatalf("Msg length = %d, want %d", len(e.Msg), len(longMsg))
	}
}

func TestLoggerName(t *testing.T) {
	cases := map[string]string{
		"engine.log":    "engine",
		"engine.log.gz": "engine",
		"sys.log":       "sys",
		"custom.txt":    "custom.txt",
	}
	for in, want := range cases {
		if got := loggerName(in); got != want {
			t.Errorf("loggerName(%q) = %q, want %q", in, got, want)
		}
	}
}

// ---------- Build：窗口 / 坏行 / 多文件 ----------

func TestWindowFilter(t *testing.T) {
	dir := t.TempDir()
	content := line("2026-10-28 10:00:00", "info", "alpha") +
		line("2026-10-28 10:10:00", "info", "beta") +
		line("2026-10-28 10:20:00", "info", "gamma")
	p := writeLog(t, dir, "engine.log", content)

	idx := buildIndex(t, Config{Paths: []string{p}, From: utc(2026, 10, 28, 10, 5, 0), To: utc(2026, 10, 28, 10, 15, 0)})
	if len(idx.Signatures) != 1 || idx.Signatures[0].Pattern != "beta" {
		t.Fatalf("Signatures = %#v, want only beta", idx.Signatures)
	}

	// 边界是闭区间：From=To=10:10 仍命中 beta。
	idx = buildIndex(t, Config{Paths: []string{p}, From: utc(2026, 10, 28, 10, 10, 0), To: utc(2026, 10, 28, 10, 10, 0)})
	if len(idx.Signatures) != 1 || idx.Signatures[0].Pattern != "beta" {
		t.Fatalf("inclusive window: Signatures = %#v, want only beta", idx.Signatures)
	}

	// 窗口外的记录不算 Unparsed。
	if idx.Unparsed != 0 {
		t.Fatalf("Unparsed = %d, want 0", idx.Unparsed)
	}
}

func TestBadLinesCountedAsUnparsed(t *testing.T) {
	dir := t.TempDir()
	content := line("2026-10-28 10:00:00", "info", "good") +
		"this is not logfmt\n" +
		`level=info msg="no time"` + "\n" +
		"\n" +
		`time="bad" level=info msg="bad time"` + "\n"
	p := writeLog(t, dir, "engine.log", content)

	idx := buildIndex(t, Config{Paths: []string{p}})
	if idx.Unparsed != 3 {
		t.Fatalf("Unparsed = %d, want 3", idx.Unparsed)
	}
	if idx.Files != 1 {
		t.Fatalf("Files = %d, want 1", idx.Files)
	}
	if len(idx.Signatures) != 1 || idx.Signatures[0].Count != 1 {
		t.Fatalf("Signatures = %#v, want one signature with Count 1", idx.Signatures)
	}
}

func TestLineNumbersSkipBlankLines(t *testing.T) {
	dir := t.TempDir()
	content := line("2026-10-28 10:00:00", "info", "first") + "\n" +
		line("2026-10-28 10:00:01", "info", "second") + "\n"
	p := writeLog(t, dir, "engine.log", content)

	idx := buildIndex(t, Config{Paths: []string{p}})
	first := findSignature(t, idx, "first")
	second := findSignature(t, idx, "second")
	if first.Samples[0].Line != 1 {
		t.Fatalf("first line = %d, want 1", first.Samples[0].Line)
	}
	if second.Samples[0].Line != 3 {
		t.Fatalf("second line = %d, want 3", second.Samples[0].Line)
	}
}

func TestMultipleFiles(t *testing.T) {
	dir := t.TempDir()
	p1 := writeLog(t, dir, "engine.log", line("2026-10-28 10:00:00", "info", "one"))
	p2 := writeLog(t, dir, "sys.log", line("2026-10-28 10:00:01", "error", "two"))

	idx := buildIndex(t, Config{Paths: []string{p1, p2}})
	if idx.Files != 2 {
		t.Fatalf("Files = %d, want 2", idx.Files)
	}
	if got := findSignature(t, idx, "one").Samples[0].Logger; got != "engine" {
		t.Fatalf("logger for engine.log = %q, want engine", got)
	}
	if got := findSignature(t, idx, "two").Samples[0].Logger; got != "sys" {
		t.Fatalf("logger for sys.log = %q, want sys", got)
	}
}

func TestMissingFileReturnsError(t *testing.T) {
	_, err := Build(Config{Paths: []string{filepath.Join(t.TempDir(), "nope.log")}})
	if err == nil {
		t.Fatal("Build(missing file) error = nil, want non-nil")
	}
}

func TestEmptyConfig(t *testing.T) {
	idx, err := Build(Config{})
	if err != nil {
		t.Fatalf("Build(Config{}) error = %v", err)
	}
	if idx == nil {
		t.Fatal("Build(Config{}) returned nil index")
	}
	if idx.Files != 0 || idx.Unparsed != 0 || len(idx.Signatures) != 0 || idx.Truncated {
		t.Fatalf("empty index = %#v, want zero values", idx)
	}
}

// ---------- gzip ----------

func TestGzipFile(t *testing.T) {
	dir := t.TempDir()
	content := line("2026-10-28 10:00:00", "info", "archived") +
		line("2026-10-28 10:00:01", "warning", "archived")
	p := writeGzipLog(t, dir, "engine.log.gz", content)

	idx := buildIndex(t, Config{Paths: []string{p}})
	if idx.Files != 1 || idx.Unparsed != 0 {
		t.Fatalf("Files/Unparsed = %d/%d, want 1/0", idx.Files, idx.Unparsed)
	}
	sig := findSignature(t, idx, "archived")
	if sig.Count != 2 {
		t.Fatalf("Count = %d, want 2", sig.Count)
	}
	s := sig.Samples[0]
	if s.File != "engine.log.gz" {
		t.Fatalf("File = %q, want engine.log.gz", s.File)
	}
	if s.Logger != "engine" {
		t.Fatalf("Logger = %q, want engine", s.Logger)
	}
}

// ---------- 签名 ----------

func TestSignatureMasking(t *testing.T) {
	cases := []struct{ msg, want string }{
		{"任务 3 访问 https://example.com/c 失败，耗时 30ms", "任务 # 访问 <url> 失败，耗时 #ms"},
		{"访问 https://example.com/a失败", "访问 <url>失败"},
		{"GET https://example.com/a?x=1&y=2: 502", "GET <url>: #"},
		{"digest deadbeefdeadbeefdeadbeef", "digest <id>"},
		{"frag cafe1234", "frag <id>"},
		{"addr 0x00007FF905FED592", "addr <id>"},
		{"uuid 550e8400-e29b-41d4-a716-446655440000", "uuid <id>"},
		{"id 12345678901234567890", "id #"},
		{"v3.5.2 启动", "v#.#.# 启动"},
	}
	for _, c := range cases {
		if got := signatureOf(c.msg); got != c.want {
			t.Errorf("signatureOf(%q) = %q, want %q", c.msg, got, c.want)
		}
	}
}

func TestSignatureAggregation(t *testing.T) {
	dir := t.TempDir()
	content := line("2026-10-28 10:00:30", "error", "任务 3 访问 https://example.com/c 失败，耗时 30ms") +
		line("2026-10-28 10:00:10", "error", "任务 1 访问 https://example.com/a 失败，耗时 10ms") +
		line("2026-10-28 10:00:50", "error", "任务 5 访问 https://example.com/e 失败，耗时 50ms") +
		line("2026-10-28 10:00:20", "error", "任务 2 访问 https://example.com/b 失败，耗时 20ms") +
		line("2026-10-28 10:00:40", "error", "任务 4 访问 https://example.com/d 失败，耗时 40ms")
	p := writeLog(t, dir, "engine.log", content)

	idx := buildIndex(t, Config{Paths: []string{p}, MaxSamples: 2})
	const wantPattern = "任务 # 访问 <url> 失败，耗时 #ms"
	sig := findSignature(t, idx, wantPattern)
	if sig.Count != 5 {
		t.Fatalf("Count = %d, want 5", sig.Count)
	}
	if want := utc(2026, 10, 28, 10, 0, 10); !sig.First.Equal(want) {
		t.Fatalf("First = %v, want %v", sig.First, want)
	}
	if want := utc(2026, 10, 28, 10, 0, 50); !sig.Last.Equal(want) {
		t.Fatalf("Last = %v, want %v", sig.Last, want)
	}
	if len(sig.Samples) != 2 {
		t.Fatalf("Samples = %d, want 2", len(sig.Samples))
	}
	if want := utc(2026, 10, 28, 10, 0, 10); !sig.Samples[0].Time.Equal(want) {
		t.Fatalf("Samples[0].Time = %v, want %v", sig.Samples[0].Time, want)
	}
	if want := utc(2026, 10, 28, 10, 0, 20); !sig.Samples[1].Time.Equal(want) {
		t.Fatalf("Samples[1].Time = %v, want %v", sig.Samples[1].Time, want)
	}
}

func TestSignatureTruncatedToLimit(t *testing.T) {
	dir := t.TempDir()
	content := line("2026-10-28 10:00:00", "info", strings.Repeat("长", 200))
	p := writeLog(t, dir, "engine.log", content)

	idx := buildIndex(t, Config{Paths: []string{p}})
	if len(idx.Signatures) != 1 {
		t.Fatalf("Signatures = %d, want 1", len(idx.Signatures))
	}
	pattern := idx.Signatures[0].Pattern
	if got := utf8.RuneCountInString(pattern); got != patternLimit {
		t.Fatalf("pattern rune count = %d, want %d", got, patternLimit)
	}
	if pattern != strings.Repeat("长", patternLimit) {
		t.Fatalf("pattern = %q, want prefix of 长 repeated", pattern)
	}
}

func TestSignatureOrdering(t *testing.T) {
	dir := t.TempDir()
	content := line("2026-10-28 10:00:00", "info", "beta") +
		line("2026-10-28 10:00:01", "info", "alpha") +
		line("2026-10-28 10:00:02", "info", "alpha") +
		line("2026-10-28 10:00:03", "info", "gamma")
	p := writeLog(t, dir, "engine.log", content)

	idx := buildIndex(t, Config{Paths: []string{p}})
	var got []string
	for _, sig := range idx.Signatures {
		got = append(got, sig.Pattern)
	}
	want := []string{"alpha", "beta", "gamma"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ordering = %q, want %q", got, want)
	}
}

// ---------- 上限 / 确定性 ----------

func TestMaxEntriesTruncation(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for i := 0; i < 4; i++ {
		b.WriteString(lineAt(i, "error", "task 1 failed"))
	}
	p := writeLog(t, dir, "engine.log", b.String())

	idx := buildIndex(t, Config{Paths: []string{p}, MaxEntries: 2})
	if !idx.Truncated {
		t.Fatal("Truncated = false, want true")
	}
	total := 0
	for _, sig := range idx.Signatures {
		total += sig.Count
	}
	if total != 2 {
		t.Fatalf("total entries = %d, want 2", total)
	}

	// 恰好等于上限且没有更多记录：不算截断。
	content := line("2026-10-28 10:00:00", "info", "one") + line("2026-10-28 10:00:01", "info", "two")
	p2 := writeLog(t, dir, "sys.log", content)
	idx = buildIndex(t, Config{Paths: []string{p2}, MaxEntries: 2})
	if idx.Truncated {
		t.Fatal("Truncated = true when entries == MaxEntries, want false")
	}
}

func TestMaxSamplesDefault(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for i := 0; i < 5; i++ {
		b.WriteString(lineAt(i, "info", "same message"))
	}
	p := writeLog(t, dir, "engine.log", b.String())

	idx := buildIndex(t, Config{Paths: []string{p}})
	sig := findSignature(t, idx, "same message")
	if len(sig.Samples) != defaultMaxSamples {
		t.Fatalf("Samples = %d, want default %d", len(sig.Samples), defaultMaxSamples)
	}
}

func TestBuildDeterministic(t *testing.T) {
	dir := t.TempDir()
	content := line("2026-10-28 10:00:02", "error", "任务 9 访问 https://example.com/z 失败") +
		line("2026-10-28 10:00:01", "info", "alpha") +
		line("2026-10-28 10:00:03", "warning", "任务 2 访问 https://example.com/y 失败")
	p := writeLog(t, dir, "engine.log", content)

	cfg := Config{Paths: []string{p}, MaxSamples: 2}
	first := buildIndex(t, cfg)
	second := buildIndex(t, cfg)
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("two builds differ:\n first: %#v\nsecond: %#v", first, second)
	}
}

// ---------- 小工具 ----------

// lineAt 造一行日志（时间戳 = 10:00:00 + i 秒），供批量场景复用。
func lineAt(i int, level, msg string) string {
	ts := time.Date(2026, 10, 28, 10, 0, i, 0, time.UTC).Format(timestampLayout)
	return `time="` + ts + `" level=` + level + ` msg="` + msg + `"` + "\n"
}

// line 造一行带指定时间戳的日志。
func line(ts, level, msg string) string {
	return `time="` + ts + `" level=` + level + ` msg="` + msg + `"` + "\n"
}
