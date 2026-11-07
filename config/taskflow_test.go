package config

import "testing"

func TestTaskFlowOptIn(t *testing.T) {
	if loadString(t, baseConfig).TaskFlow.Enabled {
		t.Fatal("task flow must default off")
	}
	if !loadString(t, baseConfig+"task_flow:\n  enabled: true\n").TaskFlow.Enabled {
		t.Fatal("task flow configuration ignored")
	}
}
