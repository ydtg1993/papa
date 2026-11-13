package events

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestTruncateError(t *testing.T) {
	if got := TruncateError("x"); got != "x" {
		t.Fatalf("short error changed: %q", got)
	}
	long := strings.Repeat("a", maxErrorLen+100)
	got := TruncateError(long)
	if got != omittedString {
		t.Fatalf("oversized error was not omitted: %q", got)
	}
}

func TestDetachPreservesExactStringBudgetAndPriorOmission(t *testing.T) {
	padding := strings.Repeat("界", MaxStringBytes/3) + "ab"
	input := Event{
		RunID: padding, Type: padding, Site: padding, Stage: padding,
		URL: padding, Kind: padding, Outcome: padding, Queue: padding,
	}
	got, complete := Detach(input)
	if !complete || got.Truncated {
		t.Fatal("exactly full byte budget was marked truncated")
	}
	if got.RunID != padding || got.Type != padding || got.Site != padding || got.Stage != padding || got.URL != padding || got.Kind != padding || got.Outcome != padding || got.Queue != padding {
		t.Fatal("valid multibyte string changed at exact budget")
	}
	for _, input := range []Event{{Truncated: true}, {Error: TruncateError(padding + "x")}} {
		got, complete := Detach(input)
		if complete || !got.Truncated {
			t.Fatal("prior omission evidence was lost")
		}
		got, complete = Detach(got)
		if complete || !got.Truncated {
			t.Fatal("second asynchronous boundary lost omission evidence")
		}
	}
}

func TestEventJSONOmitsZeroFields(t *testing.T) {
	ev := Event{Type: TypeRunStarted, RunID: "r-1", TS: time.Unix(0, 0).UTC()}
	b, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	if !strings.Contains(s, `"type":"run_started"`) {
		t.Fatalf("missing type: %s", s)
	}
	if strings.Contains(s, `"task_id"`) || strings.Contains(s, `"site"`) {
		t.Fatalf("zero fields should be omitted: %s", s)
	}
}
