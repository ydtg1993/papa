package eventlog

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v3/pkg/events"
)

// testBase 是测试的固定基准时间：所有事件时间都由它偏移，测试不依赖真实时钟。
func testBase() time.Time {
	return time.Date(2026, 1, 2, 3, 0, 0, 0, time.UTC)
}

// testAt 返回基准时间之后 minutes 分钟。
func testAt(minutes int) time.Time {
	return testBase().Add(time.Duration(minutes) * time.Minute)
}

// testEvent 造一条最小事件（字段够用即可，语义不属于本包测试的关注点）。
func testEvent(runID string, seq uint64, ts time.Time) events.Event {
	return events.Event{V: events.Version, Seq: seq, TS: ts, RunID: runID, Type: events.TypeQueueRound}
}

// writeJSONL 把事件逐行 JSON 编码后写入 path。
func writeJSONL(t *testing.T, path string, evs ...events.Event) {
	t.Helper()
	var buf bytes.Buffer
	for _, ev := range evs {
		data, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("marshal event: %v", err)
		}
		buf.Write(data)
		buf.WriteByte('\n')
	}
	writeFile(t, path, buf.Bytes())
}

// writeGzipJSONL 把事件逐行 JSON 编码并 gzip 压缩后写入 path。
func writeGzipJSONL(t *testing.T, path string, evs ...events.Event) {
	t.Helper()
	var raw bytes.Buffer
	for _, ev := range evs {
		data, err := json.Marshal(ev)
		if err != nil {
			t.Fatalf("marshal event: %v", err)
		}
		raw.Write(data)
		raw.WriteByte('\n')
	}
	var compressed bytes.Buffer
	zw := gzip.NewWriter(&compressed)
	if _, err := zw.Write(raw.Bytes()); err != nil {
		t.Fatalf("gzip write %s: %v", path, err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close %s: %v", path, err)
	}
	writeFile(t, path, compressed.Bytes())
}

// writeRaw 原样写入文本内容。
func writeRaw(t *testing.T, path, content string) {
	t.Helper()
	writeFile(t, path, []byte(content))
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// setModTime 固定文件修改时间，让 Discover 的排序断言可确定。
func setModTime(t *testing.T, path string, ts time.Time) {
	t.Helper()
	if err := os.Chtimes(path, ts, ts); err != nil {
		t.Fatalf("chtimes %s: %v", path, err)
	}
}

// eventSeqs 提取事件的 seq 序列，便于整段断言排序结果。
func eventSeqs(evs []events.Event) []uint64 {
	out := make([]uint64, len(evs))
	for i := range evs {
		out[i] = evs[i].Seq
	}
	return out
}

// eventKeys 提取 "runID/seq" 序列，便于断言 TS 相同时的次级排序。
func eventKeys(evs []events.Event) []string {
	out := make([]string, len(evs))
	for i := range evs {
		out[i] = evs[i].RunID + "/" + strconv.FormatUint(evs[i].Seq, 10)
	}
	return out
}
