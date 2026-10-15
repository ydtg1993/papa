package main

import (
	"os"
	"strings"
	"testing"
)

// demo 默认只监听回环地址：绑全网卡 + 随机令牌等于把后台开放给所有能读到日志/猜到令牌的人。
// 这里钉住"默认监听地址是 127.0.0.1"，防止谁改回 :9090 又没意识到暴露范围。
func TestDemoListensOnLoopbackByDefault(t *testing.T) {
	src, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("read main.go: %v", err)
	}
	if !strings.Contains(string(src), `addr := "127.0.0.1:9090"`) {
		t.Error("main.go 默认监听地址应是 127.0.0.1:9090（回环），不能是 :9090（全网卡）")
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
