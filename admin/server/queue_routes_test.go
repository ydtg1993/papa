package server

import (
	"fmt"
	"github.com/ydtg1993/papa/v4/engine"
	"net/http"
	"testing"
)

func TestRepeatQueueProcess(t *testing.T) {
	var called int
	newMon := func() *Monitor {
		called = 0
		return NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{VerifyToken: allowUnrestrictedTestRequests,
			ProcessRepeatQueue: func() (int, error) { called++; return 3, nil },
		})
	}

	rr := serve(newMon(), http.MethodPost, "/api/repeatqueue/process", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if called != 1 {
		t.Fatalf("回调应被调一次，实得 %d", called)
	}
	if body := decodeJSON(t, rr); body["repolled"] != float64(3) {
		t.Fatalf("repolled = %v", body["repolled"])
	}
}

func TestErrorQueueProcess(t *testing.T) {
	var called int
	newMon := func() *Monitor {
		called = 0
		return NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{VerifyToken: allowUnrestrictedTestRequests,
			ProcessErrorQueue: func() (int, error) { called++; return 5, nil },
		})
	}

	rr := serve(newMon(), http.MethodPost, "/api/errorqueue/process", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if called != 1 {
		t.Fatalf("回调应被调一次，实得 %d", called)
	}
	if body := decodeJSON(t, rr); body["processed"] != float64(5) {
		t.Fatalf("processed = %v", body["processed"])
	}
}

func TestQueueProcessWritesAuditEvent(t *testing.T) {
	var events []QueueProcessEvent
	newMon := func() *Monitor {
		events = nil
		return NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{VerifyToken: allowUnrestrictedTestRequests,
			ProcessRepeatQueue: func() (int, error) { return 7, nil },
			ProcessErrorQueue:  func() (int, error) { return 5, nil },
			OnQueueProcess:     func(ev QueueProcessEvent) { events = append(events, ev) },
		})
	}

	t.Run("轮询队列成功", func(t *testing.T) {
		serve(newMon(), http.MethodPost, "/api/repeatqueue/process", nil)
		if len(events) != 1 {
			t.Fatalf("审计事件 = %+v，want 恰好一条", events)
		}
		ev := events[0]
		if ev.Queue != engine.QueueRepeat || ev.Processed != 7 || ev.Err != nil {
			t.Fatalf("审计事件 = %+v", ev)
		}
		if ev.IP == "" {
			t.Fatal("来源 IP 应当带上（与白名单同一取法）")
		}
		if ev.At.IsZero() {
			t.Fatal("时间应当带上")
		}
	})

	t.Run("错误队列成功", func(t *testing.T) {
		serve(newMon(), http.MethodPost, "/api/errorqueue/process", nil)
		if len(events) != 1 || events[0].Queue != engine.QueueError || events[0].Processed != 5 {
			t.Fatalf("审计事件 = %+v", events)
		}
	})

	t.Run("处理失败：也要记一条", func(t *testing.T) {
		events = nil
		m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{VerifyToken: allowUnrestrictedTestRequests,
			ProcessRepeatQueue: func() (int, error) { return 0, fmt.Errorf("db down") },
			OnQueueProcess:     func(ev QueueProcessEvent) { events = append(events, ev) },
		})
		if rr := serve(m, http.MethodPost, "/api/repeatqueue/process", nil); rr.Code != http.StatusInternalServerError {
			t.Fatalf("status = %d, want 500", rr.Code)
		}
		if len(events) != 1 || events[0].Err == nil {
			t.Fatalf("失败的触发也要记一条（err 非空）：%+v", events)
		}
	})

	t.Run("接口没配置（404）：没跑过就不该记", func(t *testing.T) {
		events = nil
		m := NewMonitor(emptyGetter, testLogger{t}, MonitorConfig{VerifyToken: allowUnrestrictedTestRequests,
			OnQueueProcess: func(ev QueueProcessEvent) { events = append(events, ev) },
		})
		if rr := serve(m, http.MethodPost, "/api/repeatqueue/process", nil); rr.Code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", rr.Code)
		}
		if len(events) != 0 {
			t.Fatalf("压根没这条接口时不该记审计：%+v", events)
		}
	})
}
