package main

import "testing"

func TestParseAct(t *testing.T) {
	ok := []struct {
		raw       string
		verb, arg string
	}{
		{"scroll:bottom", "scroll", "bottom"},
		{"scroll:3", "scroll", "3"},
		{"scroll:.list", "scroll", ".list"},
		{"click:.load-more", "click", ".load-more"},
		{"input:#kw=火影", "input", "#kw=火影"},
		{"hover:.menu", "hover", ".menu"},
		{"wait:2s", "wait", "2s"},
		{"wait:.item", "wait", ".item"},
		// eval 的 JS 里常有冒号，只按第一个冒号切分
		{"eval:window.scrollTo(0, 0)", "eval", "window.scrollTo(0, 0)"},
		{"eval:document.querySelector('a').href", "eval", "document.querySelector('a').href"},
		// 参数带空格也要保留
		{"click:.item:nth-child(2)", "click", ".item:nth-child(2)"},
	}
	for _, c := range ok {
		got, err := parseAct(c.raw)
		if err != nil {
			t.Fatalf("parseAct(%q) 报错: %v", c.raw, err)
		}
		if got.verb != c.verb || got.arg != c.arg {
			t.Fatalf("parseAct(%q) = %q/%q, want %q/%q", c.raw, got.verb, got.arg, c.verb, c.arg)
		}
	}

	bad := []string{"", "scroll", ":bottom", "scroll:", "foo:bar", "click:"}
	for _, raw := range bad {
		if _, err := parseAct(raw); err == nil {
			t.Fatalf("parseAct(%q) 应该报错", raw)
		}
	}
}

// wait: 的时长/选择器判别规则：能解析成时长就是等待，否则按选择器。
func TestWaitIsDuration(t *testing.T) {
	durations := []string{"2s", "500ms", "1m30s", "1.5h"}
	for _, s := range durations {
		if !waitIsDuration(s) {
			t.Fatalf("waitIsDuration(%q) = false, want true", s)
		}
	}
	selectors := []string{".item", "#list", "div.load-more", "h1", "[data-id]", ".item:nth-child(2)"}
	for _, s := range selectors {
		if waitIsDuration(s) {
			t.Fatalf("waitIsDuration(%q) = true, want false（CSS 选择器不是时长）", s)
		}
	}
}

func TestParseActs(t *testing.T) {
	acts, err := parseActs([]string{"scroll:bottom", "wait:1s", "click:.more"})
	if err != nil {
		t.Fatalf("parseActs 报错: %v", err)
	}
	if len(acts) != 3 {
		t.Fatalf("动作数 = %d, want 3", len(acts))
	}
	if acts[2].verb != "click" || acts[2].arg != ".more" {
		t.Fatalf("第三个动作 = %+v", acts[2])
	}

	// 任一动作写错都应整体报错，且错误里带上原始写法方便定位
	if _, err := parseActs([]string{"scroll:bottom", "clik:.x"}); err == nil {
		t.Fatal("parseActs 应拒绝非法动作")
	}
}
