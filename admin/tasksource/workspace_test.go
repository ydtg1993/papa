package tasksource

import (
	"strings"
	"testing"
)

func TestWorkspaceKeepsTaskActionsAndGenericTableDefaults(t *testing.T) {
	generic := Table(nil, nil)
	workspace := WorkspaceTable(nil, nil)
	if generic.Label != "任务" || generic.Header != "" {
		t.Fatal("public Table defaults changed")
	}
	if workspace.Key != generic.Key || len(workspace.Columns) != len(generic.Columns) || len(workspace.Actions) != len(generic.Actions) {
		t.Fatal("workspace must retain the same task table contract")
	}
	if workspace.Label != "采集任务（crawler_tasks）" || !strings.Contains(workspace.Header, "点击任务行的「追踪」") || strings.HasSuffix(workspace.Header, "\n") {
		t.Fatal("historical workspace presentation changed")
	}
}
