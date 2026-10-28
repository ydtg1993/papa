package events

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// bufCloser 是测试用 io.WriteCloser：写入收进内存，Close 无副作用。
type bufCloser struct{ bytes.Buffer }

func (b *bufCloser) Close() error { return nil }

// writeRecorder 记录每次 Write 的原始参数与 Close 次数，用于断言"一行一次 Write"。
type writeRecorder struct {
	writes [][]byte
	closes int
}

func (r *writeRecorder) Write(p []byte) (int, error) {
	r.writes = append(r.writes, append([]byte(nil), p...))
	return len(p), nil
}

func (r *writeRecorder) Close() error {
	r.closes++
	return nil
}

func TestJSONLSinkWritesOneLinePerEvent(t *testing.T) {
	var w bufCloser
	s := NewJSONLSink(&w)
	if err := s.Write(Event{Type: TypeRunStarted, RunID: "r-1"}); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(w.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("want 1 line, got %d: %q", len(lines), w.String())
	}
	var ev Event
	if err := json.Unmarshal([]byte(lines[0]), &ev); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if ev.Type != TypeRunStarted || ev.RunID != "r-1" {
		t.Fatalf("decoded type/run_id = %q/%q", ev.Type, ev.RunID)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestJSONLSinkOneWritePerLine(t *testing.T) {
	var r writeRecorder
	s := NewJSONLSink(&r)
	for i := 1; i <= 3; i++ {
		if err := s.Write(Event{Type: TypeQueueRound, Seq: uint64(i)}); err != nil {
			t.Fatal(err)
		}
	}
	if len(r.writes) != 3 {
		t.Fatalf("underlying Write calls = %d, want 3（每行必须一次 Write 写完）", len(r.writes))
	}
	for i, chunk := range r.writes {
		if len(chunk) == 0 || chunk[len(chunk)-1] != '\n' {
			t.Fatalf("write %d does not end with newline: %q", i, chunk)
		}
		if bytes.Count(chunk, []byte("\n")) != 1 {
			t.Fatalf("write %d contains %d newlines, want exactly 1", i, bytes.Count(chunk, []byte("\n")))
		}
		var ev Event
		if err := json.Unmarshal(bytes.TrimSuffix(chunk, []byte("\n")), &ev); err != nil {
			t.Fatalf("write %d not valid JSON: %v", i, err)
		}
		if ev.Seq != uint64(i+1) {
			t.Fatalf("write %d seq = %d, want %d", i, ev.Seq, i+1)
		}
	}
}

func TestJSONLSinkCloseIsIdempotent(t *testing.T) {
	var r writeRecorder
	s := NewJSONLSink(&r)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if r.closes != 1 {
		t.Fatalf("underlying Close called %d times, want 1", r.closes)
	}
}
