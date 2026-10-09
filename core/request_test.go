package core

import (
	"context"
	"testing"
)

// 逐请求头是**叠加**的：站点级先铺底，单次请求只覆盖给到的键（其余留着）。
// 值空串表示"这次不要这个头"。
func TestWithHeadersLayersAndDeletes(t *testing.T) {
	ctx := context.Background()
	if h := HeadersFrom(ctx); h != nil {
		t.Fatalf("没挂过应当返回 nil，实得 %v", h)
	}

	// 站点级一层
	ctx = WithHeaders(ctx, map[string]string{"User-Agent": "site-ua", "Referer": "https://a/"})
	got := HeadersFrom(ctx)
	if got["User-Agent"] != "site-ua" || got["Referer"] != "https://a/" {
		t.Fatalf("站点级头没挂上：%v", got)
	}

	// 单次请求再覆盖一个键、删掉一个键 —— Referer 必须还在（不是被整份替换）
	ctx = WithHeaders(ctx, map[string]string{"User-Agent": "one-off", "Referer": ""})
	got = HeadersFrom(ctx)
	if got["User-Agent"] != "one-off" {
		t.Fatalf("逐请求应覆盖同键：%v", got)
	}
	// 空值是**标记**（"这次不要这个头"），不是当场删：不然后面就没法区分"删掉配置里那个头"
	// 与"没意见"。消费侧（静态抓取 / 浏览器）按标记处理 —— 见 core.ApplyHeaders。
	if v, ok := got["Referer"]; !ok || v != "" {
		t.Fatalf("空值应当作为删除标记留着：%v", got)
	}

	// 返回的是副本：改它不影响 ctx 里那份
	got["User-Agent"] = "tampered"
	if again := HeadersFrom(ctx); again["User-Agent"] != "one-off" {
		t.Fatalf("HeadersFrom 应返回副本，实得 %v", again)
	}

	// 空 map / nil 不改动 ctx
	if HeadersFrom(WithHeaders(ctx, nil))["User-Agent"] != "one-off" {
		t.Fatal("空 nil 不该改动已有的头")
	}
}

// ApplyHeaders 是给"手里已经有一份 map"的路径用的（静态抓取把配置头合并进来时）。
func TestApplyHeaders(t *testing.T) {
	base := map[string]string{"User-Agent": "cfg", "Accept": "*/*", "Referer": "old"}
	ApplyHeaders(base, map[string]string{"User-Agent": "req", "Referer": ""})
	if base["User-Agent"] != "req" {
		t.Fatalf("同键应覆盖：%v", base)
	}
	if _, ok := base["Referer"]; ok {
		t.Fatalf("空值应删除：%v", base)
	}
	if base["Accept"] != "*/*" {
		t.Fatalf("没提到的键要留着：%v", base)
	}
}
