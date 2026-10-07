package scheduler

import (
	"io"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
)

func newTestScheduler() *Scheduler {
	logger := logrus.New()
	logger.SetOutput(io.Discard)
	return NewScheduler(nil, logger, "Asia/Shanghai")
}

func TestSchedulerAddJobFires(t *testing.T) {
	s := newTestScheduler()
	fired := make(chan struct{}, 1)
	if err := s.AddJob("test", "* * * * * *", func() { fired <- struct{}{} }); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	s.Start()
	defer s.Stop()

	select {
	case <-fired:
	case <-time.After(3 * time.Second):
		t.Fatal("custom job did not fire within 3s")
	}
}

func TestSchedulerAddJobInvalidSpec(t *testing.T) {
	s := newTestScheduler()
	if err := s.AddJob("bad", "not a cron spec", func() {}); err == nil {
		t.Fatal("expected error for invalid cron spec")
	}
}
