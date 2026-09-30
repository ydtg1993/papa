package workerpool

import (
	"testing"
)

type testTask struct {
	url string
}

func (t *testTask) GetUrl() string { return t.url }
func (t *testTask) GetRetry() int  { return 0 }
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
