package taskflow

import "github.com/ydtg1993/papa/v4/admin/oplog"

func (ev TaskFlowActionEvent) OperationLog() oplog.Event {
	return oplog.Event{Table: "crawler_task_flows", Action: ev.Action, RowID: ev.ID, Operator: ev.Operator, IP: ev.IP, At: ev.At, Err: ev.Err}
}
