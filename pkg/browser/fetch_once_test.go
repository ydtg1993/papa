package browser

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/launcher"
)

// FetchOnce **成功路径**也必须把页面关掉。
//
// 回归点：那个"失败要收掉页面"的 defer 只判 `err != nil`，而 FetchOnce 不把 page 交出去
// （HTML / 标题 / 截图都拷进 FetchResult 了）—— 于是每次**成功**的调用都留下一个标签页。
// CLI 里跑一次看不出来，业务把它当长跑抓取接口用就是持续泄漏；顺带 `go wait()` 那条
// EachEvent 协程要等页面关闭才退出，不关它也跟着一起漏。
//
// 要真 Chrome（没有就跳过）：用本地 httptest 服务保证"加载成功"这条路真的走到，
// 然后数调用前后的页面数 —— 多出来就是漏了。
func TestFetchOnceClosesPageOnSuccess(t *testing.T) {
	if _, ok := launcher.LookPath(); !ok {
		t.Skip("本机没有可用浏览器，跳过（这条要真 Chrome）")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><head><title>hi</title></head><body><p>x</p></body></html>"))
	}))
	defer srv.Close()

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
	defer func() { _ = pool.Put(bw) }()

	before := pageCount(t, bw)
	res, err := bw.FetchOnce(ctx, srv.URL, PageOptions{Timeout: 15 * time.Second})
	if err != nil {
		t.Fatalf("FetchOnce: %v", err)
	}
	if res == nil || res.HTML == "" {
		t.Fatalf("应当抓到非空快照，实得 %+v", res)
	}

	// 关页是异步的（Target.closeTarget），给它一点时间；超时仍多出来就是真漏了
	deadline := time.Now().Add(3 * time.Second)
	for {
		after := pageCount(t, bw)
		if after <= before {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("FetchOnce 成功后应关掉页面：调用前 %d 个页面，调用后仍有 %d 个", before, after)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// pageCount 数当前浏览器上打开的页面数。
func pageCount(t *testing.T, bw *Browser) int {
	t.Helper()
	pages, err := bw.Browser.Pages()
	if err != nil {
		t.Fatalf("Pages(): %v", err)
	}
	return len(pages)
}
