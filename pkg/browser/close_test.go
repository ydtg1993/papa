package browser

import (
	"context"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/launcher"
)

// #10 回归：进程等不到退出时，Close 必须**有上限**地返回，而不是永久阻塞。
//
// 触发条件就是这个用例造的：只 `launcher.New()` 不启动 —— PID 为 0（rod 的 Kill 遇到 0 直接 return，
// 什么也杀不掉），而 Cleanup 那句 `<-l.exit` 永远等不到关闭。
// 旧写法 `b.launcher.Cleanup()` 直接调 → 永久挂住；而这条路是在**持池锁**时被调的
// （pool.reapChannel 第二段）→ 一次阻塞就是整池死锁 + 优雅停机永不返回。
func TestCloseDoesNotBlockWhenProcessNeverExits(t *testing.T) {
	b := &Browser{launcher: launcher.New(), cleanupTimeout: 50 * time.Millisecond}

	done := make(chan struct{})
	go func() {
		defer close(done)
		b.Close()
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Close 被 launcher.Cleanup 永久阻塞了 —— 这条路在持池锁时被调用，会把整池与停机一起卡死")
	}
}

// 重复 Close 不能变成问题：池里"归还时发现不可用"与"停机统一关闭"会各自调一次。
func TestCloseIsIdempotent(t *testing.T) {
	b := &Browser{launcher: launcher.New(), cleanupTimeout: 50 * time.Millisecond}
	b.Close()
	b.Close()
}

// 正常路径（真 Chrome）：Close 仍然"关得干净"——很快返回、实例不再可用、池还能接着给新的。
//
// 这里钉的是**顺序**没改坏：先 Kill 让 Cleanup 的等待能结束，而不是把正常收尾也拖成超时。
func TestCloseOnRealBrowserReturnsQuicklyAndLeavesPoolUsable(t *testing.T) {
	if _, ok := launcher.LookPath(); !ok {
		t.Skip("本机没有可用浏览器，跳过（这条要真 Chrome）")
	}
	pool, err := NewPool(PoolConfig{Size: 1, Headless: true, Leakless: false, NoSandbox: true})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	bw, err := pool.Get(ctx)
	if err != nil {
		t.Skipf("拿不到浏览器实例（本机浏览器起不来），跳过：%v", err)
	}

	start := time.Now()
	bw.Close()
	elapsed := time.Since(start)
	if elapsed > 10*time.Second {
		t.Fatalf("正常关闭用了 %v（应当远快于 cleanup 超时）", elapsed)
	}
	if bw.IsAlive() {
		t.Fatal("关闭之后实例不该还活着")
	}

	// 归还一个已关闭的实例：池按"不可用"处理（关掉、让出容量），不该把它当空闲实例再发出去
	_ = pool.Put(bw)
	again, err := pool.Get(ctx)
	if err != nil {
		t.Fatalf("归还已关闭的实例后池应当还能给出新的：%v", err)
	}
	if !again.IsAlive() {
		t.Fatal("池不该把已关闭的实例当空闲实例发出来")
	}

	// 优雅停机这条路也必须有上限地返回（#10 的第二个后果：Pool.Close 让停机永远卡住）
	_ = pool.Put(again)
	start = time.Now()
	pool.Close()
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("pool.Close 用了 %v（池里实例的关闭不该拖住停机）", elapsed)
	}
}
