package workerpool

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

type testTask struct {
	url string
}

func (t *testTask) Unique() string { return t.url }

func TestSubmitWatermark(t *testing.T) {
	p := NewWorkerPool[*testTask](1, 4, 0.75) // 高水位 75% = 3

	// 低于高水位：正常入队
	for i := 0; i < 3; i++ {
		if err := p.Submit(&testTask{url: "t"}); err != nil {
			t.Fatalf("submit %d below watermark = %v", i, err)
		}
	}
	// 达到高水位：返回 ErrQueueFull
	if err := p.Submit(&testTask{url: "t"}); err != ErrQueueFull {
		t.Fatalf("submit at watermark = %v, want ErrQueueFull", err)
	}
}

func TestSubmitWatermarkCustom(t *testing.T) {
	p := NewWorkerPool[*testTask](1, 4, 0.5) // 高水位 50% = 2

	if err := p.Submit(&testTask{url: "t"}); err != nil {
		t.Fatalf("submit 1 = %v", err)
	}
	if err := p.Submit(&testTask{url: "t"}); err != nil {
		t.Fatalf("submit 2 = %v", err)
	}
	// 第 3 个（len=2 >= 4*0.5=2）触发高水位
	if err := p.Submit(&testTask{url: "t"}); err != ErrQueueFull {
		t.Fatalf("submit at 50%% watermark = %v, want ErrQueueFull", err)
	}
}

func TestSubmitStopped(t *testing.T) {
	p := NewWorkerPool[*testTask](1, 4, 0.75)
	p.Stop(0)
	if err := p.Submit(&testTask{url: "t"}); err == nil || err == ErrQueueFull {
		t.Fatalf("submit after stop = %v, want non-ErrQueueFull error", err)
	}
}

// Submit 与 Stop 并发：不得 panic（向已关闭的 taskQueue 发送）。
// 回归点：Submit 曾先查 stopped 再发送，中间无同步，Stop 的 close 可插进来。
func TestSubmitStopConcurrent(t *testing.T) {
	handler := func(ctx context.Context, task *testTask) error { return nil }

	for iter := 0; iter < 200; iter++ {
		// watermark=1 → 高水位等于容量，不会因水位被拒，尽量让 Submit 走到发送
		p := NewWorkerPool[*testTask](2, 64, 1)
		p.Start(context.Background(), handler)

		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				<-start
				for j := 0; j < 200; j++ {
					_ = p.Submit(&testTask{url: "t"})
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			p.Stop(200 * time.Millisecond)
		}()

		close(start)
		wg.Wait()
	}
}

// handler panic 不能带走 worker：同一 worker 必须继续处理后续任务，panic 记为失败并上报栈。
func TestHandlerPanicKeepsWorker(t *testing.T) {
	p := NewWorkerPool[*testTask](1, 4, 1)
	ran := make(chan string, 4)
	p.Start(context.Background(), func(ctx context.Context, task *testTask) error {
		if task.url == "boom" {
			panic("handler blew up")
		}
		ran <- task.url
		return nil
	})

	if err := p.Submit(&testTask{url: "boom"}); err != nil {
		t.Fatal(err)
	}
	if err := p.Submit(&testTask{url: "ok"}); err != nil {
		t.Fatal(err)
	}

	// 只有一个 worker：panic 的那条先入队，若 worker 被 panic 带走，这条永远不会执行
	select {
	case got := <-ran:
		if got != "ok" {
			t.Fatalf("ran %q, want ok", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not survive handler panic: second task never ran")
	}

	// panic 计为失败
	waitFor(t, func() bool {
		_, _, failed, _, _ := p.Stats()
		return failed == 1
	}, "failed counter")

	// 上报的错误里要带 panic 值和栈
	select {
	case err := <-p.Errors():
		if !strings.Contains(err.Error(), "handler blew up") {
			t.Errorf("error %q should contain the panic value", err.Error())
		}
		if !strings.Contains(err.Error(), "goroutine") {
			t.Errorf("error %q should contain a stack trace", err.Error())
		}
	case <-time.After(3 * time.Second):
		t.Fatal("panic was not reported to the error queue")
	}

	p.Stop(time.Second)
}

func waitFor(t *testing.T, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}
