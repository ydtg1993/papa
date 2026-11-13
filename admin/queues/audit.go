package queues

import "github.com/ydtg1993/papa/v4/admin/oplog"

// OperationLog preserves the action's outcome, including partial failure.
func (ev QueueProcessEvent) OperationLog() oplog.Event {
	return oplog.Event{Table: ev.Queue, Action: "process", Values: map[string]any{"processed": ev.Processed}, Err: ev.Err, IP: ev.IP, Operator: ev.Operator, At: ev.At}
}
