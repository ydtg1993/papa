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

// Stop 必须如实报出「还有多少条没跑完」—— 调用方靠它决定要不要接着关数据库/浏览器池。
// 报成 0 会让上层以为排空了，然后把库关掉，在途任务剩下的写入全废。
func TestStopReportsInFlightOnTimeout(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 2)

	p := NewWorkerPool[*testTask](2, 8, 1)
	p.Start(context.Background(), func(context.Context, *testTask) error {
		started <- struct{}{}
		<-release
		return nil
	})

	for i := 0; i < 2; i++ {
		if err := p.Submit(&testTask{url: fmt.Sprintf("t%d", i)}); err != nil {
			t.Fatalf("submit %d = %v", i, err)
		}
	}
	// 等两个 worker 真的进到 handler，别让 Stop 发生在任务被取走之前
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			t.Fatal("worker 没有取到任务")
		}
	}

	drained, inFlight := p.Stop(50 * time.Millisecond)
	if drained {
		t.Fatal("handler 还卡着，Stop 却报了 drained")
	}
	if inFlight != 2 {
		t.Fatalf("inFlight = %d, want 2", inFlight)
	}

	close(release) // 放行，别把 worker 永久挂着
}

// 排空成功报 drained=true、inFlight=0；**重复调用返回首次的结果**（stopOnce 只跑一次），
// 而不是零值 —— 否则第二次调用方会误以为没排空，进而跳过本该做的关库。
func TestStopReportsDrainedAndIsIdempotent(t *testing.T) {
	done := make(chan struct{}, 1)
	p := NewWorkerPool[*testTask](1, 4, 1)
	p.Start(context.Background(), func(context.Context, *testTask) error {
		done <- struct{}{}
		return nil
	})
	if err := p.Submit(&testTask{url: "t"}); err != nil {
		t.Fatalf("submit = %v", err)
	}
	<-done

	if drained, inFlight := p.Stop(5 * time.Second); !drained || inFlight != 0 {
		t.Fatalf("Stop = (%v, %d), want (true, 0)", drained, inFlight)
	}
	if drained, inFlight := p.Stop(5 * time.Second); !drained || inFlight != 0 {
		t.Fatalf("第二次 Stop = (%v, %d), want (true, 0)", drained, inFlight)
	}
}

// fakeGate 测试用闸门。池子只认 Gate 接口，这里不必拖进真的熔断器 ——
// 熔断器自己的闸门语义在 internal/breaker 里测。
type fakeGate struct {
	mu     sync.Mutex
	paused bool
	ch     chan struct{}
}

func newFakeGate() *fakeGate { return &fakeGate{ch: make(chan struct{})} }

func (g *fakeGate) Wait(stop <-chan struct{}) bool {
	g.mu.Lock()
	if !g.paused {
		g.mu.Unlock()
		return true
	}
	ch := g.ch
	g.mu.Unlock()
	select {
	case <-ch:
		return true
	case <-stop:
		return false
	}
}

func (g *fakeGate) Pause() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.paused {
		return
	}
	g.paused = true
	g.ch = make(chan struct{}) // 换新的，理由同 breaker
}

func (g *fakeGate) Resume() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.paused {
		return
	}
	g.paused = false
	close(g.ch)
}

// 闸门合上时 worker **一条都不消费**：队列原封不动，放行后接着跑。
//
// 这是「熔断暂停」能不能成立的关键。另两个候选都不行：
//   - 卡 Submit：队列里已经躺着的那几条会先跑完才停得下来，"暂停"就不是暂停了；
//   - 卡 claimTask：任务已经被从 channel 取走了，闸住就得塞回去 —— 塞不回去。
func TestGateBlocksWorkersFromConsuming(t *testing.T) {
	gate := newFakeGate()
	gate.Pause()

	p := NewWorkerPool[*testTask](1, 8, 1)
	p.SetGate(gate)

	ran := make(chan string, 8)
	p.Start(context.Background(), func(_ context.Context, task *testTask) error {
		ran <- task.url
		return nil
	})

	for i := 0; i < 3; i++ {
		if err := p.Submit(&testTask{url: fmt.Sprintf("t%d", i)}); err != nil {
			t.Fatalf("submit %d = %v", i, err)
		}
	}

	select {
	case url := <-ran:
		t.Fatalf("闸住时不该执行任何任务，却跑了 %s", url)
	case <-time.After(100 * time.Millisecond):
	}
	if main, urgent := p.QueueDepths(); main+urgent != 3 {
		t.Fatalf("闸住时队列应原封不动躺着 3 条，实得 %d", main+urgent)
	}

	gate.Resume()
	for i := 0; i < 3; i++ {
		select {
		case <-ran:
		case <-time.After(2 * time.Second):
			t.Fatalf("放行后只跑了 %d 条", i)
		}
	}
	p.Stop(time.Second)
}

// 暂停中停机**照旧排空** —— 闸门只管运行期「要不要取下一个任务」，停机不在它的职责里。
//
// 这里一度反着写过（暂停中不排空，理由是"别把积压打出去"），后来拆掉了：那是**错配**。
// 熔断的暂停不跨重启，重启后启动恢复会把同一批 pending 原样捞回来重跑 ——
// 净效果为零，只是把洪峰从停机挪到启动，还多绕一趟 DB。写成反向用例是把结论钉住，
// 免得以后有人又觉得"暂停时不排空"更合理。
func TestStopWhilePausedStillDrains(t *testing.T) {
	gate := newFakeGate()
	gate.Pause()

	p := NewWorkerPool[*testTask](1, 8, 1)
	p.SetGate(gate)

	ran := make(chan string, 8)
	p.Start(context.Background(), func(_ context.Context, task *testTask) error {
		ran <- task.url
		return nil
	})
	for i := 0; i < 3; i++ {
		_ = p.Submit(&testTask{url: "t"})
	}

	if drained, inFlight := p.Stop(5 * time.Second); !drained || inFlight != 0 {
		t.Fatalf("Stop = (%v, %d), want (true, 0)：停机不看闸门，队列应当排空", drained, inFlight)
	}

	close(ran)
	n := 0
	for range ran {
		n++
	}
	if n != 3 {
		t.Fatalf("排空了 %d 条, want 3", n)
	}
}
