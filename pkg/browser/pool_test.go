package browser

import (
	"context"
	"testing"
)

func TestPoolGetDirectNotConfigured(t *testing.T) {
	p := &Pool{cfg: PoolConfig{DirectSize: 0}}
	_, err := p.GetDirect(context.Background())
	if err == nil || err.Error() != "direct browsers not configured" {
		t.Fatalf("GetDirect() error = %v, want 'direct browsers not configured'", err)
	}
}

func TestPoolGetDirectRecreatesDirectBrowser(t *testing.T) {
	directCh := make(chan *Browser, 1)
	directCh <- &Browser{useProxy: false} // 死实例（Browser 为 nil）

	var createdProxy []bool
	p := &Pool{
		directBrowsers: directCh,
		cfg:            PoolConfig{DirectSize: 1},
		newBrowserFn: func(useProxy bool) (*Browser, error) {
			createdProxy = append(createdProxy, useProxy)
			return &Browser{useProxy: useProxy}, nil
		},
	}

	b, err := p.GetDirect(context.Background())
	if err != nil {
		t.Fatalf("GetDirect() error = %v", err)
	}
	if b.useProxy {
		t.Fatal("GetDirect() returned a proxy browser")
	}
	if len(createdProxy) != 1 || createdProxy[0] {
		t.Fatalf("recreated browser useProxy = %v, want [false]", createdProxy)
	}
}

func TestPoolPutRoutesToCorrectChannel(t *testing.T) {
	proxyCh := make(chan *Browser, 1)
	directCh := make(chan *Browser, 1)
	p := &Pool{
		browsers:       proxyCh,
		directBrowsers: directCh,
		cfg:            PoolConfig{}, // MaxIdleTime 为 0，跳过空闲检查
		newBrowserFn: func(useProxy bool) (*Browser, error) {
			return &Browser{useProxy: useProxy}, nil
		},
	}

	// 直连浏览器归还到 directCh
	if err := p.Put(&Browser{useProxy: false}); err != nil {
		t.Fatalf("Put(direct) error = %v", err)
	}
	select {
	case b := <-directCh:
		if b.useProxy {
			t.Fatal("direct channel got a proxy browser")
		}
	default:
		t.Fatal("direct browser not routed to direct channel")
	}

	// 代理浏览器归还到 browsers
	if err := p.Put(&Browser{useProxy: true}); err != nil {
		t.Fatalf("Put(proxy) error = %v", err)
	}
	select {
	case b := <-proxyCh:
		if !b.useProxy {
			t.Fatal("proxy channel got a direct browser")
		}
	default:
		t.Fatal("proxy browser not routed to proxy channel")
	}
}
