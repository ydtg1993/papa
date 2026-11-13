package taskflow

import (
	"errors"
	"testing"
	"time"
)

func TestOperationLogPreservesFlowActionIdentityAndFailure(t *testing.T) {
	err := errors.New("fixture CAS conflict")
	event := TaskFlowActionEvent{Action: "update", ID: "fixture", Err: err, Operator: "operator", IP: "127.0.0.1", At: time.Unix(123, 0)}
	got := event.OperationLog()
	if got.Table != "crawler_task_flows" || got.Action != event.Action || got.RowID != event.ID || got.Err != err || got.Operator != event.Operator || got.IP != event.IP || got.At != event.At {
		t.Fatalf("flow outcome changed: %+v", got)
	}
}
