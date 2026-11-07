package events

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEventSinkMasksDiagnosticCopies(t *testing.T) {
	var out bufCloser
	event := Event{URL: "https://example.test/a?token=SECRET", Error: "Authorization: Bearer SECRET", Attrs: map[string]string{"password": "SECRET"}}
	if err := NewJSONLSink(&out).Write(event); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "SECRET") {
		t.Fatal("secret persisted in event")
	}
	if event.Attrs["password"] != "SECRET" {
		t.Fatal("event input mutated")
	}
}

func TestEventRedactionPreservesMetricNamesAndTypes(t *testing.T) {
	var out bufCloser
	event := Event{Counts: map[string]int{"token": 7, "session": 8}, Stages: map[string]StageSnapshot{"session": {Site: "sample", Completed: 9007199254740993}}, Queues: map[string]QueueSnapshot{"token": {LastError: "password=SECRET"}}}
	if err := NewJSONLSink(&out).Write(event); err != nil {
		t.Fatal(err)
	}
	var decoded Event
	if err := json.Unmarshal([]byte(out.String()), &decoded); err != nil {
		t.Fatalf("redaction broke event schema: %v", err)
	}
	if decoded.Counts["token"] != 7 || decoded.Counts["session"] != 8 || decoded.Stages["session"].Completed != 9007199254740993 {
		t.Fatal("metric identity or value changed")
	}
	if strings.Contains(decoded.Queues["token"].LastError, "SECRET") {
		t.Fatal("queue error leaked")
	}
	if event.Queues["token"].LastError != "password=SECRET" {
		t.Fatal("original snapshot changed")
	}
}

func TestEventRedactionHandlesTruncatedQuotedError(t *testing.T) {
	var out bufCloser
	event := Event{Error: TruncateError(`password="` + strings.Repeat("SECRET", 500) + `"`)}
	if err := NewJSONLSink(&out).Write(event); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "SECRET") {
		t.Fatal("truncated quoted credential leaked")
	}
}
