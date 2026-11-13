package engine

import (
	"reflect"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v4/pkg/incident"
)

func TestBreakerProbeEvidenceMatchesBoundedDurableSnapshot(t *testing.T) {
	event := &breakerEvent{}
	for i := 0; i < incident.MaxSamples+4; i++ {
		event.report.Evidence = append(event.report.Evidence, incident.Sample{TaskID: int64(i), Error: strings.Repeat("x", 1100), URL: "https://user:secret@example.test/path"})
	}
	want := event.reportSnapshot().Evidence
	got := event.probeEvidence()
	if !reflect.DeepEqual(got, want) {
		t.Fatal("probe evidence changed redaction or bounds")
	}
	got[0].Error = "changed"
	if event.report.Evidence[0].Error == "changed" {
		t.Fatal("probe evidence aliases live state")
	}
}
