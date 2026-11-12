package runtimecontrol

import "testing"

func historyDoc(t *testing.T, raw string) Document {
	t.Helper()
	d, err := Decode([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestRuntimeHistorySelectedRestorePreservesOtherSettings(t *testing.T) {
	current := historyDoc(t, `{"schema_version":1,"overrides":{"sites":{"fixture":{"weight":7,"request":{"enabled":true,"max_inflight":5}}}}}`)
	target := historyDoc(t, `{"schema_version":1,"overrides":{"sites":{"fixture":{"request":{"max_inflight":2}}}}}`)
	got, err := PatchHistory(current, target, [][]string{{"overrides", "sites", "fixture", "request", "max_inflight"}})
	if err != nil {
		t.Fatal(err)
	}
	policy := got.Overrides.Sites["fixture"]
	if *policy.Weight != 7 || !*policy.Request.Enabled || *policy.Request.MaxInflight != 2 {
		t.Fatal("restoration changed unselected settings")
	}
}

func TestRuntimeHistoryRemovalRestoresInheritanceAndRejectsUnknownPaths(t *testing.T) {
	current := historyDoc(t, `{"schema_version":1,"overrides":{"defaults":{"request":{"max_inflight":5}}}}`)
	target := historyDoc(t, `{"schema_version":1,"overrides":{}}`)
	path := []string{"overrides", "defaults", "request", "max_inflight"}
	got, err := PatchHistory(current, target, [][]string{path})
	if err != nil || got.Overrides.Defaults.Request.MaxInflight != nil {
		t.Fatal("removal must restore inheritance", err)
	}
	for _, paths := range [][][]string{nil, {path, path}, {{"schema_version"}}, {{"overrides", "__proto__"}}, {{}}} {
		if _, err := PatchHistory(current, target, paths); err == nil {
			t.Fatalf("invalid patch accepted: %v", paths)
		}
	}
}

func TestRuntimeHistoryRoundTripPreservesDurationPrecision(t *testing.T) {
	current := historyDoc(t, `{"schema_version":1,"overrides":{"defaults":{"request":{"manual_delay":9007199254740993,"max_inflight":5}}}}`)
	target := historyDoc(t, `{"schema_version":1,"overrides":{"defaults":{"request":{"max_inflight":2}}}}`)
	got, err := PatchHistory(current, target, [][]string{{"overrides", "defaults", "request", "max_inflight"}})
	if err != nil || *got.Overrides.Defaults.Request.ManualDelay != *current.Overrides.Defaults.Request.ManualDelay {
		t.Fatal("unselected duration changed precision", err)
	}
}
