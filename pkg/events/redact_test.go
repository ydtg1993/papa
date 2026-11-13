package events

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestEmitterDoesNotExposeCredentialsCutBeforeDelimiter(t *testing.T) {
	secret := "SECRET" + strings.Repeat("x", MaxStringBytes)
	userinfo := "https://user:" + secret + "@example.test/path"
	dsn := "user:" + secret + "@tcp(example.test:3306)/db"
	for name, event := range map[string]Event{
		"url":   {URL: userinfo},
		"error": {Error: dsn},
		"attrs": {Attrs: map[string]string{"detail": userinfo}},
		"stage": {Stages: map[string]StageSnapshot{"stage": {Site: userinfo}}},
		"queue": {Queues: map[string]QueueSnapshot{"queue": {LastError: dsn}}},
	} {
		t.Run(name, func(t *testing.T) {
			var out bufCloser
			emitter := NewEmitter("run", NewJSONLSink(&out), 1, nil)
			emitter.Emit(event)
			if err := emitter.Close(context.Background()); err != nil {
				t.Fatal(err)
			}
			if strings.Contains(out.String(), "SECRET") {
				t.Fatal("truncation removed credential delimiter before redaction")
			}
			var decoded Event
			if err := json.Unmarshal([]byte(out.String()), &decoded); err != nil {
				t.Fatal(err)
			}
			if !decoded.Truncated {
				t.Fatal("omitted credential must retain truncation evidence")
			}
		})
	}
}

func TestEventStringBudgetDoesNotRetainPartialCredentials(t *testing.T) {
	var out bufCloser
	padding := strings.Repeat("x", MaxStringBytes)
	// Leave 32 bytes of the total budget for Error, cutting inside userinfo
	// despite this URL being individually smaller than the field budget.
	event, complete := Detach(Event{
		RunID: padding, Type: padding, Site: padding, Stage: padding,
		URL: padding, Kind: padding, Outcome: padding,
		Queue: strings.Repeat("x", MaxStringBytes-32),
		Error: "https://user:SECRET" + strings.Repeat("x", 50) + "@example.test/",
	})
	if complete || !event.Truncated {
		t.Fatal("total string budget was not enforced")
	}
	if err := NewJSONLSink(&out).Write(event); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "SECRET") {
		t.Fatal("total budget retained partial userinfo")
	}
}

func TestTruncateErrorDoesNotExposeCredentialPrefix(t *testing.T) {
	var out bufCloser
	event := Event{Error: TruncateError("user:SECRET" + strings.Repeat("x", MaxStringBytes) + "@tcp(example.test:3306)/db")}
	if err := NewJSONLSink(&out).Write(event); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "SECRET") {
		t.Fatal("error helper retained a partial DSN credential")
	}
}

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
