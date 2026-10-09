package core

import "testing"

// 相对地址 → 绝对地址，且 scheme / fragment 都按规矩处理。
func TestResolveURL(t *testing.T) {
	const base = "https://example.com/detail/1?from=list"

	cases := []struct {
		name string
		ref  string
		want string
	}{
		{"根相对", "/video/2", "https://example.com/video/2"},
		{"同级相对", "2", "https://example.com/detail/2"},
		{"绝对地址", "https://cdn.example.com/a.webp", "https://cdn.example.com/a.webp"},
		{"协议相对", "//cdn.example.com/a.webp", "https://cdn.example.com/a.webp"},
		{"去掉 fragment", "/video/2#ep-3", "https://example.com/video/2"},
		{"只有 fragment", "#top", "https://example.com/detail/1?from=list"},
		{"两端空格", "  /video/2  ", "https://example.com/video/2"},
		{"空 reference 就是 base 自己", "", base},
	}
	for _, c := range cases {
		got, err := ResolveURL(base, c.ref)
		if err != nil {
			t.Errorf("%s: ResolveURL(%q) = %v", c.name, c.ref, err)
			continue
		}
		if got != c.want {
			t.Errorf("%s: ResolveURL(%q) = %q, want %q", c.name, c.ref, got, c.want)
		}
	}

	// 这些"链接"不该变成任务 URL
	for _, ref := range []string{"javascript:void(0)", "mailto:a@b.com", "tel:+8613800000000", "data:text/html,x"} {
		if got, err := ResolveURL(base, ref); err == nil {
			t.Errorf("ResolveURL(%q) 应当报错，实得 %q", ref, got)
		}
	}
	// 解析不出来的地址当场报错，别留个半截 URL 给后面
	if got, err := ResolveURL(base, "http://[::1"); err == nil {
		t.Errorf("残缺地址应当报错，实得 %q", got)
	}
	if got, err := ResolveURL(":::", "/a"); err == nil {
		t.Errorf("base 解析失败应当报错，实得 %q", got)
	}
}

// 裸 host、带端口、大小写、www 前缀都算同一个 host —— 分开算会让"本站链接"判定莫名失败。
func TestSameHost(t *testing.T) {
	same := [][2]string{
		{"https://example.com/a", "www.example.com"},
		{"HTTPS://EXAMPLE.COM/x", "example.com:443"},
		{"example.com", "http://example.com/path"},
		{"example.com.", "example.com"},
		{"https://example.com:8443/a", "example.com"},
	}
	for _, c := range same {
		if !SameHost(c[0], c[1]) {
			t.Errorf("SameHost(%q, %q) = false, want true", c[0], c[1])
		}
	}

	different := [][2]string{
		{"example.com", "example.org"},
		{"example.com", "cdn.example.com"}, // 子域不是"同一个 host"，用 IsSubdomainOf 判
		{"example.com", "evil-example.com"},
		{"", "example.com"},
		{"", ""},
	}
	for _, c := range different {
		if SameHost(c[0], c[1]) {
			t.Errorf("SameHost(%q, %q) = true, want false", c[0], c[1])
		}
	}
}

// 点边界：evil-example.com 不是 example.com 的子域（HasSuffix 写法最容易在这儿翻车）。
func TestIsSubdomainOf(t *testing.T) {
	cases := []struct {
		child, parent string
		want          bool
	}{
		{"https://cdn.example.com/a.webp", "https://example.com/", true},
		{"cdn.example.com", "www.example.com", true}, // 归一化后是 cdn.example.com vs example.com
		{"a.b.example.com", "example.com", true},
		{"evil-example.com", "example.com", false},
		{"notexample.com", "example.com", false},
		{"example.com", "www.example.com", false}, // 同一个 host，不是子域
		{"example.org", "example.com", false},
		{"", "example.com", false},
		{"cdn.example.com", "", false},
	}
	for _, c := range cases {
		if got := IsSubdomainOf(c.child, c.parent); got != c.want {
			t.Errorf("IsSubdomainOf(%q, %q) = %t, want %t", c.child, c.parent, got, c.want)
		}
	}
}
