package rulequality

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type qualityCheckedContext struct {
	context.Context
	checked chan struct{}
	once    sync.Once
}

func (c *qualityCheckedContext) Err() error {
	err := c.Context.Err()
	c.once.Do(func() { close(c.checked) })
	return err
}

func TestQualityMemoryCancellationWhileWaitingDoesNotAccessStore(t *testing.T) {
	for _, name := range []string{"record", "query", "save_report", "report", "create_request", "request", "operation", "start", "finish", "prune"} {
		t.Run(name, func(t *testing.T) {
			s := NewMemoryStore()
			ctx := context.Background()
			o, err := s.Record(ctx, storeObservation("before"))
			if err != nil {
				t.Fatal(err)
			}
			r, err := s.SaveReport(ctx, storeReport("report", o.Scope))
			if err != nil {
				t.Fatal(err)
			}
			pending, err := s.CreateRequest(ctx, storeRequest("pending", r.ID))
			if err != nil {
				t.Fatal(err)
			}
			running, err := s.CreateRequest(ctx, storeRequest("running", r.ID))
			if err != nil {
				t.Fatal(err)
			}
			running, err = s.StartRequest(ctx, running.ID, running.Revision, strings.Repeat("a", 64))
			if err != nil {
				t.Fatal(err)
			}
			beforeObservations := detached(s.observations)
			beforeReports := detached(s.reports)
			beforeRequests := detached(s.requests)
			canceled, cancel := context.WithCancel(ctx)
			defer cancel()
			checked := &qualityCheckedContext{Context: canceled, checked: make(chan struct{})}
			result := make(chan error, 1)
			s.mu.Lock()
			go func() {
				var err error
				switch name {
				case "record":
					_, err = s.Record(checked, storeObservation("after"))
				case "query":
					_, err = s.Query(checked, r.Query)
				case "save_report":
					_, err = s.SaveReport(checked, storeReport("after", o.Scope))
				case "report":
					_, err = s.Report(checked, r.ID)
				case "create_request":
					_, err = s.CreateRequest(checked, storeRequest("after", r.ID))
				case "request":
					_, err = s.Request(checked, pending.ID)
				case "operation":
					_, err = s.RequestByOperation(checked, pending.OperationID)
				case "start":
					_, err = s.StartRequest(checked, pending.ID, pending.Revision, strings.Repeat("b", 64))
				case "finish":
					_, err = s.FinishRequest(checked, running.ID, running.Revision, Failed, `{}`)
				case "prune":
					_, err = s.PruneObservations(checked, o.CreatedAt.Add(time.Hour), 1)
				}
				result <- err
			}()
			select {
			case <-checked.checked:
			case <-time.After(5 * time.Second):
				s.mu.Unlock()
				t.Fatal("store did not check context")
			}
			cancel()
			s.mu.Unlock()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Errorf("canceled waiter accessed store: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("store did not return after unlocking")
			}
			if !reflect.DeepEqual(s.observations, beforeObservations) || !reflect.DeepEqual(s.reports, beforeReports) || !reflect.DeepEqual(s.requests, beforeRequests) {
				t.Error("canceled waiter modified persisted state")
			}
		})
	}
}
