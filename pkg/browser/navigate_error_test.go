package browser

import (
	"context"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/launcher"
)

// 导航失败必须是**返回错误**，不能把进程带走。
//
// 回归点：`NewPageWithOptions` 的失败路径写的是 `return nil, ...`，会把命名返回值 page 置 nil，
// 而那个"失败要收掉页面"的 defer 照样会跑 —— `(*rod.Page)(nil).Close()` 直接 nil 解引用 panic。
// 触发条件很普通（DNS 失败/连接被拒/导航超时），在 worker 里会被 recover 吃成 `panic: runtime error`
// （真实原因 navigate: … 丢掉），在 worker 之外直接崩进程。
//
// 要真 Chrome 才走得到这条路径（没有就跳过）：地址指向一个必然连不上的端口，
// 让"导航失败"确定性地发生。
func TestNavigateFailureReturnsErrorNotPanic(t *testing.T) {
	if _, ok := launcher.LookPath(); !ok {
		t.Skip("本机没有可用浏览器，跳过（这条要真 Chrome 才能走到导航失败那条路）")
	}
	pool, err := NewPool(PoolConfig{Size: 1, Headless: true, Leakless: false, NoSandbox: true})
	if err != nil {
		t.Fatalf("NewPool: %v", err)
	}
	defer pool.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	bw, err := pool.Get(ctx)
	if err != nil {
		t.Skipf("拿不到浏览器实例（本机浏览器起不来），跳过：%v", err)
	}
	defer func() { _ = pool.Put(bw) }()

	start := time.Now()
	_, err = bw.NewPageWithOptions(ctx, "http://127.0.0.1:1/", PageOptions{Timeout: 5 * time.Second})
	if err == nil {
		t.Fatal("连不上的地址应当返回错误")
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("导航失败应当很快返回，实得 %v", elapsed)
	}
}
