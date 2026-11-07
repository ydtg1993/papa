package database

import (
	"github.com/ydtg1993/papa/v3/config"
	"testing"
)

func TestTaskFlowModelsOptIn(t *testing.T) {
	c := &config.Config{}
	before := len(FrameworkModels(c))
	c.TaskFlow.Enabled = true
	after := FrameworkModels(c)
	if len(after) != before+1 {
		t.Fatal("flow schema switch not independent")
	}
	found := false
	for _, m := range after {
		if m.Name == "crawler_task_flows" {
			found = true
		}
	}
	if !found {
		t.Fatal("flow schema missing")
	}
}
