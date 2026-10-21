package core

import "testing"

func TestResolveSiteURL(t *testing.T) {
	cases := []struct {
		name string
		base string
		raw  string
		want string
		bad  bool
	}{
		{
			name: "相对路径走当前站点",
			base: "https://site.com/",
			raw:  "/detail/1?x=1#frag",
			want: "https://site.com/detail/1?x=1",
		},
		{
			name: "旧域名的绝对地址被钉回当前站点",
			base: "https://site.com/",
			raw:  "https://old-mirror.net/detail/1",
			want: "https://site.com/detail/1",
		},
		{
			name: "空 raw 得到站点根",
			base: "https://site.com/base/path",
			raw:  "",
			want: "https://site.com/",
		},
		{
			name: "base 的端口保留",
			base: "http://127.0.0.1:8080/",
			raw:  "detail/1",
			want: "http://127.0.0.1:8080/detail/1",
		},
		{
			name: "base 的路径不参与拼接",
			base: "https://site.com/foo/",
			raw:  "/detail/1",
			want: "https://site.com/detail/1",
		},
		{
			name: "base 非 http(s)",
			base: "ftp://site.com/",
			raw:  "/detail/1",
			bad:  true,
		},
		{
			name: "base 没有 host",
			base: "https:///detail",
			raw:  "/detail/1",
			bad:  true,
		},
		{
			name: "base 带 userinfo",
			base: "https://u:p@site.com/",
			raw:  "/detail/1",
			bad:  true,
		},
		{
			name: "raw 带 userinfo",
			base: "https://site.com/",
			raw:  "https://u:p@evil.com/detail/1",
			bad:  true,
		},
		{
			name: "raw 是非 http 伪链接",
			base: "https://site.com/",
			raw:  "javascript:alert(1)",
			bad:  true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ResolveSiteURL(c.base, c.raw)
			if c.bad {
				if err == nil {
					t.Fatalf("ResolveSiteURL(%q, %q) = %q, want error", c.base, c.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveSiteURL(%q, %q): %v", c.base, c.raw, err)
			}
			if got != c.want {
				t.Fatalf("ResolveSiteURL(%q, %q) = %q, want %q", c.base, c.raw, got, c.want)
			}
		})
	}
}
