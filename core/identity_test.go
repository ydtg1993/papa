package core

import (
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
