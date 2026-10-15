package engine

import (
	"reflect"
	"sort"
	"testing"
)

// 站点/逐请求头在交给抓取客户端前要拆成"要设的"与"要删的"（空值 = 删掉那个头）。
// 拆错的后果是静默的：本该删掉的头还带着、或本该设的被当删——都只表现为"抓到的页面不对"。
func TestSplitHeaderOverride(t *testing.T) {
	if set, remove := splitHeaderOverride(nil); set != nil || remove != nil {
		t.Fatalf("空 map 应当返回 (nil, nil)，实得 (%v, %v)", set, remove)
	}
	set, remove := splitHeaderOverride(map[string]string{
		"User-Agent": "ua",
		"Referer":    "",
		"X-Trace":    "t",
	})
	wantSet := map[string]string{"User-Agent": "ua", "X-Trace": "t"}
	if !reflect.DeepEqual(set, wantSet) {
		t.Fatalf("要设的头 = %v, want %v", set, wantSet)
	}
	sort.Strings(remove)
	if !reflect.DeepEqual(remove, []string{"Referer"}) {
		t.Fatalf("要删的头 = %v, want [Referer]", remove)
	}
}

// 混合大小写的头名要按 HTTP 语义规范化：站点级 Authorization + 逐请求 authorization:""
// 必须能删掉，否则旧凭据照发（浏览器路径按 map 精确键删）。
func TestSplitHeaderOverrideCanonicalizesKeys(t *testing.T) {
	set, remove := splitHeaderOverride(map[string]string{
		"authorization": "",
		"cookie":        "a=b",
		"x-custom":      "v",
	})
	sort.Strings(remove)
	if !reflect.DeepEqual(remove, []string{"Authorization"}) {
		t.Fatalf("小写 authorization 应规范成 Authorization 并进删除列表，实得 %v", remove)
	}
	if _, ok := set["Cookie"]; !ok {
		t.Fatalf("小写 cookie 应规范成 Cookie，实得 %v", set)
	}
	if _, ok := set["X-Custom"]; !ok {
		t.Fatalf("小写 x-custom 应规范成 X-Custom，实得 %v", set)
	}
}
