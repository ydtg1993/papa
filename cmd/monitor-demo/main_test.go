package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/ydtg1993/oao"
)

func TestDemoTaskSiteFilter(t *testing.T) {
	table := fakeTaskTable()
	for _, site := range []string{"shop", "blog", "missing"} {
		rows, total, err := table.Source.List(context.Background(), oao.Query{Page: 1, Size: 100, Filter: map[string]string{"site": site}})
		want := int64(30)
		if site == "missing" {
			want = 0
		}
		if err != nil || total != want || int64(len(rows)) != want {
			t.Fatalf("%s: rows=%d total=%d err=%v", site, len(rows), total, err)
		}
		for _, row := range rows {
			if row["site"] != site {
				t.Fatalf("wrong site: %v", row)
			}
		}
	}
}

// demo 默认只监听回环地址：绑全网卡 + 随机令牌等于把后台开放给所有能读到日志/猜到令牌的人。
// 这里钉住"默认监听地址是 127.0.0.1"，防止谁改回 :9090 又没意识到暴露范围。
func TestDemoListensOnLoopbackByDefault(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if !strings.Contains(string(src), `addr := fmt.Sprintf("127.0.0.1:%d", *port)`) || !strings.Contains(string(src), `flag.Int("port", 9091,`) {
		t.Error("demo 必须在回环地址上使用可配置端口，默认9091避免占用现有后台")
	}
}

// 令牌随机生成，不再有写死在源码里的固定演示令牌（知道源码的人不该天然能登录任何一台 demo）。
func TestDemoTokenIsRandom(t *testing.T) {
	src, err := os.ReadFile("token_demo.go")
	if err != nil {
		t.Fatalf("read token_demo.go: %v", err)
	}
	if strings.Contains(string(src), `const demoToken = "demo-token"`) {
		t.Error("token_demo.go 不该再有固定演示令牌常量 —— 应改为随机生成（randomDemoToken）")
	}
}
