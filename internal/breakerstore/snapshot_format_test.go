package breakerstore

import (
	"encoding/json"
	"github.com/ydtg1993/papa/v4/internal/breaker"
	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/obs/incident"
	"testing"
)

func TestDecodeRejectsUnsupportedSnapshotFormats(t *testing.T) {
	for _, payload := range []any{
		incident.Lifecycle{Capture: incident.Capture{ID: "old", Site: "synthetic"}},
		Snapshot{Control: breaker.Recovery{Version: 2, ID: "future"}, Window: &breaker.Checkpoint{}},
		Snapshot{Control: breaker.Recovery{Version: 1, ID: "missing-window"}},
	} {
		data, err := json.Marshal(payload)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := decodeSnapshots([]models.BreakerIncident{{ID: "fixture", Payload: data}}); err == nil {
			t.Fatal("unsupported snapshot accepted", string(data))
		}
	}
}
