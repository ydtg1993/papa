package queues

import (
	"errors"
	"github.com/ydtg1993/papa/v4/admin/auth"
	"github.com/ydtg1993/papa/v4/engine"
	"net/http/httptest"
	"testing"
)

type silentLogger struct{}

func (silentLogger) Info(...any)           {}
func (silentLogger) Infof(string, ...any)  {}
func (silentLogger) Errorf(string, ...any) {}
func TestQueueFailureAuditsExecutedOperation(t *testing.T) {
	failure := errors.New("storage offline")
	var events []QueueProcessEvent
	h := New(Options{ProcessErrorQueue: func() (int, error) { return 3, failure }, OnQueueProcess: func(e QueueProcessEvent) { events = append(events, e) }}, silentLogger{})
	r := httptest.NewRequest("POST", "/api/errorqueue/process", nil)
	r.RemoteAddr = "192.0.2.4:80"
	r = r.WithContext(auth.WithOperator(r.Context(), "alice"))
	w := httptest.NewRecorder()
	h.Error(w, r)
	if w.Code != 500 || len(events) != 1 || events[0].Processed != 3 || events[0].Err != failure || events[0].Operator != "alice" || events[0].IP != "192.0.2.4" || events[0].Queue != engine.QueueError {
		t.Fatal(w.Code, events)
	}
	w = httptest.NewRecorder()
	h.Repeat(w, r)
	if w.Code != 404 || len(events) != 1 {
		t.Fatal("unconfigured queue was audited")
	}
}
