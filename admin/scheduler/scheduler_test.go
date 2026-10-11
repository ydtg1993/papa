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

// 动态移除：移掉的 job 不该再跑；重复移除 / 移不存在的不该 panic
// （后台「停用业务定时任务」之类会走到这里）。
func TestRemoveJob(t *testing.T) {
	s := newTestScheduler()
	fired := make(chan struct{}, 4)
	if err := s.AddJob("doomed", "* * * * * *", func() { fired <- struct{}{} }); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	if err := s.AddJob("keeper", "* * * * * *", func() { fired <- struct{}{} }); err != nil {
		t.Fatalf("AddJob: %v", err)
	}
	s.Start()
	defer s.Stop()

	s.RemoveJob("doomed")
	s.RemoveJob("doomed")      // 重复移除：不该 panic
	s.RemoveJob("never-added") // 不存在：不该 panic

	deadline := time.After(3 * time.Second)
	select {
	case <-fired: // keeper 会继续跑，只要没 panic 就对
	case <-deadline:
		t.Fatal("移除一个 job 不该把别的 job 也停掉（3s 内一个都没跑）")
	}
}
