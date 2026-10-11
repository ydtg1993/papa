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
