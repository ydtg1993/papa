package workerpool

import (
	"context"
	"fmt"
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

/* ---------- 快车道 ---------- */

// 快车道容量 = max(workers*2, 8)；水位 0.75 时 8 个位子在 6 个时就不收了。
// 满了必须**退到主队列**而不是 ErrQueueFull —— 调用方把 ErrQueueFull 翻译成
// "溢出到 DB、每 2s 回灌一次"，对加急任务那等于把加急属性悄悄吃掉。
func TestSubmitUrgentFallsBackToMainQueue(t *testing.T) {
	p := NewWorkerPool[*testTask](1, 32, 0.75)

	for i := 0; i < 6; i++ {
		if err := p.SubmitUrgent(&testTask{url: "u"}); err != nil {
			t.Fatalf("快车道第 %d 条就报错了：%v", i+1, err)
		}
	}
	if err := p.SubmitUrgent(&testTask{url: "overflow"}); err != nil {
		t.Fatalf("快车道满时应退到主队列，实得 %v", err)
	}

	main, urgent := p.QueueDepths()
	if main != 1 || urgent != 6 {
		t.Fatalf("main/urgent = %d/%d, want 1/6", main, urgent)
	}
	if _, _, _, _, queueLen := p.Stats(); queueLen != 7 {
		t.Fatalf("Stats queueLen = %d, want 7（两条队列之和）", queueLen)
	}
}

// 快车道优先：worker 有空位时，快车道上的任务必须先于主队列里的被取走。
func TestUrgentLaneIsServedFirst(t *testing.T) {
	p := NewWorkerPool[*testTask](1, 32, 1)
	gate := make(chan struct{})
	entered := make(chan string, 8) // 进入 handler 就报一声，用来确认 worker 真的被占住了
	order := make(chan string, 8)

	p.Start(context.Background(), func(_ context.Context, task *testTask) error {
		entered <- task.url
		<-gate // 所有任务都等同一个闸门，方便把队列堆满再统一放行
		order <- task.url
		return nil
	})

	// 先塞一条把唯一 worker 占住，之后的都堆在队列里
	if err := p.Submit(&testTask{url: "blocker"}); err != nil {
		t.Fatal(err)
	}
	// 不能只看 inProgress —— 它在任务入队时就为 1 了，worker 可能还没取到
	if got := <-entered; got != "blocker" {
		t.Fatalf("先进入 handler 的是 %q", got)
	}

	// 主队列先堆两条，快车道再进一条
	if err := p.Submit(&testTask{url: "n1"}); err != nil {
		t.Fatal(err)
	}
	if err := p.Submit(&testTask{url: "n2"}); err != nil {
		t.Fatal(err)
	}
	if err := p.SubmitUrgent(&testTask{url: "urgent"}); err != nil {
		t.Fatal(err)
	}

	close(gate)
	p.Stop(5 * time.Second)
	close(order)

	var got []string
	for s := range order {
		got = append(got, s)
	}
	if len(got) != 4 {
		t.Fatalf("执行了 %d 条, want 4: %v", len(got), got)
	}
	if got[0] != "blocker" {
		t.Fatalf("先跑的应是占住 worker 的那条，实得顺序 %v", got)
	}
	if got[1] != "urgent" {
		t.Fatalf("快车道应先于主队列被取，实得顺序 %v", got)
	}
}

// 停机必须把**两条**队列的存量都跑完。
//
// 回归点：Engine.Stop 是先 e.cancel() 再 pool.Stop()，进 Stop 时 ctx.Done() 早就关闭了；
// worker 循环里一旦放 `case <-ctx.Done()`，停机瞬间会随机选中它直接返回，队列里的任务被丢光。
// 这里显式用已取消的 ctx 起池，把那个条件复现出来。
func TestStopDrainsBothQueuesEvenWhenCtxCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 复现 Engine.Stop 的顺序

	const queued = 24
	var mu sync.Mutex
	ran := 0

	p := NewWorkerPool[*testTask](1, 64, 1)
	gate := make(chan struct{})
	p.Start(ctx, func(context.Context, *testTask) error {
		<-gate
		mu.Lock()
		ran++
		mu.Unlock()
		return nil
	})

	if err := p.Submit(&testTask{url: "blocker"}); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { _, _, _, inProgress, _ := p.Stats(); return inProgress == 1 }, "blocker 进入 handler")

	// 两条队列各堆一批（快车道满了会自动降级到主队列，正好一起覆盖）
	for i := 0; i < queued; i++ {
		var err error
		if i%3 == 0 {
			err = p.SubmitUrgent(&testTask{url: fmt.Sprintf("u%d", i)})
		} else {
			err = p.Submit(&testTask{url: fmt.Sprintf("n%d", i)})
		}
		if err != nil {
			t.Fatalf("第 %d 条入队失败：%v", i, err)
		}
	}
	if main, urgent := p.QueueDepths(); main+urgent < queued {
		t.Fatalf("任务没堆起来（%d 条），测试前提不成立", main+urgent)
	}

	close(gate)
	p.Stop(5 * time.Second)

	mu.Lock()
	defer mu.Unlock()
	if ran != queued+1 {
		t.Fatalf("停机后只跑了 %d 条，应为 %d 条 —— 队列存量被丢", ran, queued+1)
	}
}

// SubmitUrgent 与 Stop 并发：同样不得 panic（两条队列都不 close，结构上就不该有这条路径）。
func TestSubmitUrgentStopConcurrent(t *testing.T) {
	for iter := 0; iter < 100; iter++ {
		p := NewWorkerPool[*testTask](2, 64, 1)
		p.Start(context.Background(), func(context.Context, *testTask) error { return nil })

		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				<-start
				for j := 0; j < 100; j++ {
					if i%2 == 0 {
						_ = p.Submit(&testTask{url: "t"})
					} else {
						_ = p.SubmitUrgent(&testTask{url: "t"})
					}
				}
			}(i)
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
