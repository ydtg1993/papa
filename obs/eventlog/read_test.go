package eventlog

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestReadFiltersWindow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writeJSONL(t, path,
		testEvent("run-1", 1, testAt(0)),
		testEvent("run-1", 2, testAt(10)),
		testEvent("run-1", 3, testAt(20)),
		testEvent("run-1", 4, testAt(30)),
	)

	cases := []struct {
		name string
		from time.Time
		to   time.Time
		want []uint64
	}{
		{"both bounds inclusive", testAt(10), testAt(20), []uint64{2, 3}},
		{"lower bound only", testAt(20), time.Time{}, []uint64{3, 4}},
		{"upper bound only", time.Time{}, testAt(10), []uint64{1, 2}},
		{"no bounds", time.Time{}, time.Time{}, []uint64{1, 2, 3, 4}},
		{"empty window", testAt(11), testAt(19), nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res, err := Read([]string{path}, tc.from, tc.to)
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if res.Files != 1 {
				t.Fatalf("Files = %d, want 1", res.Files)
			}
			if got := eventSeqs(res.Events); !slices.Equal(got, tc.want) {
				t.Fatalf("event seqs = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReadSortsByTimeThenRunAndSeq(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	writeJSONL(t, path,
		testEvent("run-b", 2, testAt(10)),
		testEvent("run-a", 3, testAt(10)),
		testEvent("run-a", 1, testAt(5)),
		testEvent("run-c", 9, testAt(5)),
	)

	res, err := Read([]string{path}, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := []string{"run-a/1", "run-c/9", "run-a/3", "run-b/2"}
	if got := eventKeys(res.Events); !slices.Equal(got, want) {
		t.Fatalf("event order = %v, want %v", got, want)
	}
}

func TestReadGzipBackup(t *testing.T) {
	dir := t.TempDir()
	active := filepath.Join(dir, "events.jsonl")
	backup := filepath.Join(dir, "events-2026-01-02T01-00-00.000.jsonl.gz")
	writeJSONL(t, active, testEvent("run-1", 2, testAt(20)))
	writeGzipJSONL(t, backup, testEvent("run-1", 1, testAt(10)))
	setModTime(t, backup, testAt(60))
	setModTime(t, active, testAt(120))

	paths, err := Discover(active)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if want := []string{backup, active}; !slices.Equal(paths, want) {
		t.Fatalf("Discover = %v, want %v", paths, want)
	}

	res, err := Read(paths, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if res.Files != 2 {
		t.Fatalf("Files = %d, want 2", res.Files)
	}
	if got := eventSeqs(res.Events); !slices.Equal(got, []uint64{1, 2}) {
		t.Fatalf("event seqs = %v, want [1 2]", got)
	}
	if res.Gaps != 0 || res.Unparsed != 0 || res.Duplicates != 0 {
		t.Fatalf("Gaps/Unparsed/Duplicates = %d/%d/%d, want 0/0/0",
			res.Gaps, res.Unparsed, res.Duplicates)
	}

	// 单独读一个 .gz 备份也必须可用。
	only, err := Read([]string{backup}, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Read gzip only: %v", err)
	}
	if got := eventSeqs(only.Events); !slices.Equal(got, []uint64{1}) {
		t.Fatalf("gzip only event seqs = %v, want [1]", got)
	}
}

func TestReadCountsSeqGaps(t *testing.T) {
	t.Run("gaps within one run and unordered input", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "events.jsonl")
		writeJSONL(t, path,
			testEvent("run-a", 7, testAt(70)),
			testEvent("run-b", 1, testAt(15)),
			testEvent("run-a", 1, testAt(10)),
			testEvent("run-a", 4, testAt(40)),
			testEvent("run-b", 2, testAt(20)),
			testEvent("run-a", 2, testAt(20)),
		)
		res, err := Read([]string{path}, time.Time{}, time.Time{})
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		// run-a: 1,2,4,7 → 缺口 (4-2-1)+(7-4-1) = 1+2 = 3；run-b: 1,2 → 0。
		if res.Gaps != 3 {
			t.Fatalf("Gaps = %d, want 3", res.Gaps)
		}
		want := []string{
			"run-a/1", "run-b/1", "run-a/2", "run-b/2", "run-a/4", "run-a/7",
		}
		if got := eventKeys(res.Events); !slices.Equal(got, want) {
			t.Fatalf("event order = %v, want %v", got, want)
		}
	})

	t.Run("seq numbers do not bridge runs", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "events.jsonl")
		writeJSONL(t, path,
			testEvent("run-a", 3, testAt(0)),
			testEvent("run-b", 10, testAt(10)),
			testEvent("run-c", 1, testAt(20)),
		)
		res, err := Read([]string{path}, time.Time{}, time.Time{})
		if err != nil {
			t.Fatalf("Read: %v", err)
		}
		if res.Gaps != 0 {
			t.Fatalf("Gaps = %d, want 0 (gaps are per run_id)", res.Gaps)
		}
	})
}

func TestReadDeduplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	first := testEvent("run-1", 1, testAt(0))
	first.Type = "run_started"
	duplicate := testEvent("run-1", 1, testAt(0))
	duplicate.Type = "run_stopped"
	writeJSONL(t, path, first, duplicate, testEvent("run-1", 2, testAt(10)))

	res, err := Read([]string{path}, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(res.Events) != 2 || res.Duplicates != 1 {
		t.Fatalf("Events/Duplicates = %d/%d, want 2/1", len(res.Events), res.Duplicates)
	}
	if got := res.Events[0].Type; got != "run_started" {
		t.Fatalf("kept event type = %q, want first occurrence %q", got, "run_started")
	}

	// 同一路径重复传入：整个文件再读一遍，所有事件都应按 (run_id, seq) 丢弃。
	twice, err := Read([]string{path, path}, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Read twice: %v", err)
	}
	if len(twice.Events) != 2 || twice.Duplicates != 4 {
		t.Fatalf("Events/Duplicates = %d/%d, want 2/4", len(twice.Events), twice.Duplicates)
	}
	if twice.Files != 2 {
		t.Fatalf("Files = %d, want 2", twice.Files)
	}
}

func TestReadCountsUnparsedAndIgnoresUnknownKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	lines := []string{
		`{"v":1,"seq":1,"ts":"2026-01-02T03:10:00Z","run_id":"run-1","type":"run_started"}`,
		`{not json`,
		``,
		`[]`,
		`{"v":1,"seq":2,"ts":"2026-01-02T03:20:00Z","run_id":"run-1","type":"queue_round","future_field":{"nested":true},"attrs":{"k":"v"}}`,
		`{"v":1,"seq":3,"ts":"2026-01-02T03:30:00Z","run_id":"run-1","type":"queue_round"}`,
	}
	writeRaw(t, path, strings.Join(lines, "\n")+"\n")

	res, err := Read([]string{path}, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if res.Unparsed != 2 {
		t.Fatalf("Unparsed = %d, want 2", res.Unparsed)
	}
	if got := eventSeqs(res.Events); !slices.Equal(got, []uint64{1, 2, 3}) {
		t.Fatalf("event seqs = %v, want [1 2 3]", got)
	}
	if got := res.Events[1].Attrs["k"]; got != "v" {
		t.Fatalf("attrs not decoded: %q", got)
	}
}

func TestReadHandlesCRLF(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.jsonl")
	line1 := `{"v":1,"seq":1,"ts":"2026-01-02T03:10:00Z","run_id":"run-1","type":"run_started"}`
	line2 := `{"v":1,"seq":2,"ts":"2026-01-02T03:20:00Z","run_id":"run-1","type":"queue_round"}`
	writeRaw(t, path, line1+"\r\n"+line2+"\r\n")

	res, err := Read([]string{path}, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if res.Unparsed != 0 || !slices.Equal(eventSeqs(res.Events), []uint64{1, 2}) {
		t.Fatalf("Unparsed = %d, seqs = %v, want 0 and [1 2]",
			res.Unparsed, eventSeqs(res.Events))
	}
}

func TestReadEmptyAndMissingFiles(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "events.jsonl")
	writeRaw(t, empty, "")

	res, err := Read([]string{empty}, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Read empty file: %v", err)
	}
	if res.Files != 1 || len(res.Events) != 0 || res.Unparsed != 0 {
		t.Fatalf("empty file result = %+v, want Files=1, no events", res)
	}

	res, err = Read(nil, time.Time{}, time.Time{})
	if err != nil {
		t.Fatalf("Read nil paths: %v", err)
	}
	if res.Files != 0 || len(res.Events) != 0 {
		t.Fatalf("nil paths result = %+v, want zero result", res)
	}

	missing := filepath.Join(dir, "missing.jsonl")
	if _, err := Read([]string{missing}, time.Time{}, time.Time{}); err == nil {
		t.Fatal("Read missing file: want error, got nil")
	}
	if _, err := Read([]string{empty, missing}, time.Time{}, time.Time{}); err == nil {
		t.Fatal("Read with one missing file: want error, got nil")
	}
}

func TestReadCorruptGzipReturnsError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events-2026-01-02T01-00-00.000.jsonl.gz")
	writeRaw(t, path, "this is not gzip\n")

	if _, err := Read([]string{path}, time.Time{}, time.Time{}); err == nil {
		t.Fatal("Read corrupt gzip: want error, got nil")
	}
}
