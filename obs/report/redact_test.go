package report

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v3/obs"
)

func TestReportArtifactsMaskKnownSecretsWithoutMutatingSource(t *testing.T) {
	rep := sampleReport()
	raw := "https://example.test/a?token=SECRET&cursor=17"
	rep.Problems[0].Evidence[0].Excerpt = raw
	rep.Environment = map[string]string{"password": "SECRET"}
	files, err := Write(t.TempDir(), rep)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 4 {
		t.Fatalf("artifacts=%d", len(files))
	}
	for _, p := range files {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "SECRET") {
			t.Fatalf("secret in %s", p)
		}
	}
	if rep.Problems[0].Evidence[0].Excerpt != raw || rep.Environment["password"] != "SECRET" {
		t.Fatal("source report mutated")
	}
}

func TestReportDiagnosticsPreserveLargeCountsAndBacklog(t *testing.T) {
	const count int64 = 9007199254740993 // first integer not exactly representable by float64
	rep := sampleReport()
	backlog := obs.BacklogSummary{TotalPending: count, TotalProcessing: 7, NonRepeatablePending: count - 2, ThroughputPerMin: 1.25, ETA: "token=SECRET"}
	rep.Sections = []obs.SectionReport{
		{Key: "backlog", Title: "Backlog", Metrics: backlog},
		{Key: "custom", Title: "Custom", Metrics: map[string]any{"count": count, "nested": map[string]string{"password": "SECRET"}}},
	}
	before, err := json.Marshal(rep)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := JSON(rep)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Sections []struct {
			Key     string                     `json:"key"`
			Metrics map[string]json.RawMessage `json:"metrics"`
		} `json:"sections"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if string(decoded.Sections[0].Metrics["total_pending"]) != "9007199254740993" || string(decoded.Sections[1].Metrics["count"]) != "9007199254740993" {
		t.Fatalf("report rounded large counts: %s", raw)
	}
	if decoded.Sections[0].Key != "backlog" || strings.Contains(string(raw), "SECRET") {
		t.Fatalf("report schema identifier or masking changed: %s", raw)
	}
	want := backlog
	want.ETA = "token=***"
	brief := BuildAIBrief(rep)
	if brief.Snapshot.Backlog != want {
		t.Fatalf("concrete backlog lost in diagnostic copy: got %+v, want %+v", brief.Snapshot.Backlog, want)
	}
	ai, err := AIBriefJSON(rep)
	if err != nil {
		t.Fatal(err)
	}
	var briefDecoded AIBrief
	if err := json.Unmarshal(ai, &briefDecoded); err != nil || briefDecoded.Snapshot.Backlog != want {
		t.Fatalf("AI JSON backlog changed: %s (%v)", ai, err)
	}
	_ = Markdown(rep)
	_ = AIBriefMarkdown(rep)
	after, err := json.Marshal(rep)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("diagnostic rendering mutated source report: %v", err)
	}
}

func TestReportTextDoesNotDependOnUnrenderedBusinessMetrics(t *testing.T) {
	rep := sampleReport()
	rep.Sections = append(rep.Sections, obs.SectionReport{Key: "custom", Title: "custom metric section", Metrics: make(chan int)})
	if !strings.Contains(Markdown(rep), "custom metric section") || len(BuildAIBrief(rep).Problems) != len(rep.Problems) {
		t.Fatal("unrendered business metric suppressed diagnostic text")
	}
	if _, err := JSON(rep); err == nil {
		t.Fatal("full JSON contract must still reject non-JSON metrics")
	}
}
