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

	before, err := bw.Browser.Pages()
	if err != nil {
		t.Fatalf("列页面失败：%v", err)
	}

	start := time.Now()
	_, err = bw.NewPageWithOptions(ctx, "http://127.0.0.1:1/", PageOptions{Timeout: 5 * time.Second})
	if err == nil {
		t.Fatal("连不上的地址应当返回错误")
	}
	if elapsed := time.Since(start); elapsed > 20*time.Second {
		t.Fatalf("导航失败应当很快返回，实得 %v", elapsed)
	}

	// 失败时**刚建出来的那个页面**必须被关掉：页面挂在池化浏览器上，漏一个就少一个标签页，
	// 反复失败会一直吃下去。回归点：这里曾经判的是命名返回值 `page != nil` —— 而失败路径写的是
	// `return nil, ...`，那个变量早被置成 nil，于是"失败要收掉页面"的 defer 永远不生效。
	// 关页面是发给 Chrome 的异步指令，给它一点时间落下去再判。
	deadline := time.Now().Add(3 * time.Second)
	for {
		after, perr := bw.Browser.Pages()
		if perr == nil && len(after) <= len(before) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("导航失败后页面没被关掉：建之前 %d 个页面，现在还有 %d 个", len(before), len(after))
		}
		time.Sleep(50 * time.Millisecond)
	}
}
