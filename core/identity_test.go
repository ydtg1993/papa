package core

import (
	"strings"
	"testing"
)

func TestPageIdentity(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
		bad  bool
	}{
		{name: "裸主机按根路径", raw: "https://a.com", want: "/"},
		{name: "根路径", raw: "https://a.com/", want: "/"},
		{name: "去 fragment", raw: "https://a.com/detail/1#ep-3", want: "/detail/1"},
		{name: "保留 query 顺序", raw: "https://a.com/detail/1?b=2&a=1", want: "/detail/1?b=2&a=1"},
		{name: "host 被忽略", raw: "http://other.example/detail/1?b=2", want: "/detail/1?b=2"},
		{name: "转义路径不被解码", raw: "https://a.com/a%2Fb", want: "/a%2Fb"},
		{name: "相对地址", raw: "/detail/1", bad: true},
		{name: "非 http(s)", raw: "ftp://a.com/x", bad: true},
		{name: "带 userinfo", raw: "https://u:p@a.com/x", bad: true},
		{name: "javascript 伪链接", raw: "javascript:alert(1)", bad: true},
		{name: "空串", raw: "  ", bad: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := PageIdentity(c.raw)
			if c.bad {
				if err == nil {
					t.Fatalf("PageIdentity(%q) = %q, want error", c.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("PageIdentity(%q): %v", c.raw, err)
			}
			if got != c.want {
				t.Fatalf("PageIdentity(%q) = %q, want %q", c.raw, got, c.want)
			}
		})
	}
}

func TestTaskKey(t *testing.T) {
	key, err := TaskKey("site", "detail", "https://old-domain.com/detail/1?b=2&a=1#x")
	if err != nil {
		t.Fatalf("TaskKey: %v", err)
	}
	want := "site:detail:/detail/1?b=2&a=1"
	if key != want {
		t.Fatalf("TaskKey = %q, want %q", key, want)
	}

	// 换域名不改变键
	other, err := TaskKey("site", "detail", "https://new-domain.net/detail/1?b=2&a=1")
	if err != nil {
		t.Fatalf("TaskKey(new host): %v", err)
	}
	if other != key {
		t.Fatalf("换域名后键变了：%q -> %q", key, other)
	}

	for _, c := range []struct {
		name  string
		site  string
		stage string
		raw   string
	}{
		{name: "缺 site", stage: "detail", raw: "https://a.com/x"},
		{name: "缺 stage", site: "s", raw: "https://a.com/x"},
		{name: "非 http", site: "s", stage: "d", raw: "/x"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, err := TaskKey(c.site, c.stage, c.raw); err == nil {
				t.Fatalf("TaskKey(%q,%q,%q) 应当报错", c.site, c.stage, c.raw)
			}
		})
	}
}

// 超过 varchar(1000) 要报错而不是截断：截断会让两个不同 URL 撞同一个键。
func TestTaskKeyRejectsOverlongKey(t *testing.T) {
	raw := "https://a.com/detail/1?q=" + strings.Repeat("x", maxIdempotencyKeyLen)
	_, err := TaskKey("site", "stage", raw)
	if err == nil {
		t.Fatal("超长键应当报错")
	}
	if !strings.Contains(err.Error(), "max 1000") {
		t.Fatalf("错误里应给出上限，实得：%v", err)
	}
}
