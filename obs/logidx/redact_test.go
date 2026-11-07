package logidx

import (
	"strings"
	"testing"
	"time"
)

func TestLogIndexMasksDecodedAndRawHistoricalSamples(t *testing.T) {
	raw := `time="2026-11-07 12:00:00" level=error msg="GET https://u:SECRET@example.test/?token=SECRET" password="SECRET"`
	entry, ok := parseLine(raw, time.UTC, "engine.log", "engine", 1)
	if !ok {
		t.Fatal("sample did not parse")
	}
	if strings.Contains(entry.Raw, "SECRET") || strings.Contains(entry.Msg, "SECRET") || entry.Fields["password"] != "***" {
		t.Fatalf("unmasked sample: %+v", entry)
	}
	if !strings.Contains(raw, "SECRET") {
		t.Fatal("source changed")
	}
}

func TestLogIndexMasksEscapedCredentialsInRawEvidence(t *testing.T) {
	raw := `time="2026-11-07 12:00:00" level=error msg="response {\"token\":\"SECRET\"}"`
	entry, ok := parseLine(raw, time.UTC, "engine.log", "engine", 1)
	if !ok {
		t.Fatal("sample did not parse")
	}
	if strings.Contains(entry.Raw, "SECRET") || strings.Contains(entry.Msg, "SECRET") {
		t.Fatal("escaped credential persisted in raw evidence")
	}
}
