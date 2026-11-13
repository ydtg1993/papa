package queues

import (
	"errors"
	"testing"
	"time"
)

func TestOperationLogPreservesPartialQueueFailure(t *testing.T) {
	err := errors.New("fixture partial failure")
	event := QueueProcessEvent{Queue: "error_queue", Processed: 3, Err: err, Operator: "operator", IP: "127.0.0.1", At: time.Unix(123, 0)}
	got := event.OperationLog()
	if got.Table != event.Queue || got.Action != "process" || got.Values["processed"] != 3 || got.Err != err || got.Operator != event.Operator || got.IP != event.IP || got.At != event.At {
		t.Fatalf("queue outcome changed: %+v", got)
	}
}
