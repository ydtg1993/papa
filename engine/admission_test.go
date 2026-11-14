package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"gorm.io/gorm"
)

func TestStopWaitsForAdmittedPersistence(t *testing.T) {
	for _, timeout := range []bool{false, true} {
		t.Run(map[bool]string{false: "wait", true: "timeout"}[timeout], func(t *testing.T) {
			f := newFakeTaskDB()
			f.noRows = true
			e := newTestEngine(t, f)
			if err := e.AddStage("stub", StageConfig{Site: "stub", Entry: true, WorkerCount: 1, QueueSize: 1}, nil, nil); err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			if err := e.db.Callback().Create().Before("gorm:create").Register("test:block_admission", func(tx *gorm.DB) { close(entered); <-release }); err != nil {
				t.Fatal(err)
			}
			submitted := make(chan error, 1)
			go func() {
				submitted <- e.SubmitTask(&Task{Site: "stub", Stage: "stub", URL: "https://example.invalid/admission"})
			}()
			select {
			case <-entered:
			case <-time.After(time.Second):
				t.Fatal("submission never reached persistence")
			}
			stopped := make(chan bool, 1)
			wait := time.Second
			if timeout {
				wait = 20 * time.Millisecond
			}
			go func() { drained, _ := e.Stop(wait); stopped <- drained }()
			if timeout {
				select {
				case drained := <-stopped:
					if drained {
						t.Error("pending persistence reported drained")
					}
				case <-time.After(time.Second):
					t.Error("stop exceeded deadline")
				}
			} else {
				select {
				case <-stopped:
					t.Error("stop returned before persistence released")
				case <-time.After(30 * time.Millisecond):
				}
			}
			close(release)
			select {
			case <-submitted:
			case <-time.After(time.Second):
				t.Fatal("submission did not return")
			}
			if !timeout {
				select {
				case <-stopped:
				case <-time.After(time.Second):
					t.Fatal("stop did not finish")
				}
			}
		})
	}
}

func TestAdmissionClosedRejectsEveryPublicSubmission(t *testing.T) {
	e := newTestEngine(t, newFakeTaskDB())
	e.deliveryState().Close()
	ctx := context.Background()
	checks := []struct {
		name string
		call func() error
	}{
		{"single", func() error { return e.SubmitTask(nil) }},
		{"batch", func() error { return e.SubmitTasks(nil) }},
		{"flow-create", func() error { return e.CreateTaskFlow(ctx, "", "", FlowPolicy{}) }},
		{"flow-update", func() error { return e.UpdateTaskFlow(ctx, "", 0, FlowPolicy{}) }},
		{"flow-submit", func() error { return e.SubmitFlowTask(ctx, "", 0, nil) }},
		{"retry", func() error { return e.RetryTaskVersioned(ctx, 1, 0, 0) }},
		{"urgent", func() error { return e.UrgentTask(1) }},
		{"compensate", func() error { _, err := e.BatchCompensate(ctx, nil); return err }},
		{"error-queue", func() error { _, err := e.ProcessErrorQueue(); return err }},
		{"repeat", func() error { _, err := e.RepollRepeatableTasks(); return err }},
		{"force-repeat", func() error { _, err := e.ForceRepollRepeatableTasks(); return err }},
	}
	for _, tc := range checks {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.call(); !errors.Is(err, ErrEngineStopped) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestStopWaitsForStartupRecoveryBeforeClosingDependencies(t *testing.T) {
	f := newFakeTaskDB()
	e := newTestEngine(t, f)
	entered, release := make(chan struct{}), make(chan struct{})
	if err := e.db.Callback().Update().Before("gorm:update").Register("test:block_startup", func(tx *gorm.DB) { close(entered); <-release }); err != nil {
		t.Fatal(err)
	}
	startup := make(chan struct{})
	go func() { defer close(startup); e.ApplyRegisterStage() }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("startup did not reach storage")
	}
	stopped := make(chan bool, 1)
	go func() { done, _ := e.Stop(time.Second); stopped <- done }()
	select {
	case <-stopped:
		t.Error("Stop returned before startup persistence exited")
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	select {
	case <-startup:
	case <-time.After(time.Second):
		t.Fatal("startup did not cancel")
	}
	select {
	case done := <-stopped:
		if !done {
			t.Fatal("startup cancellation did not drain")
		}
	case <-time.After(time.Second):
		t.Fatal("Stop did not finish")
	}
}
