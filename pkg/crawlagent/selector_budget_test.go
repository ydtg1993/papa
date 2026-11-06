package crawlagent

import (
	"errors"
	"strconv"
	"strings"
	"testing"

	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
)

func TestSelectorBudgetAllowsSimpleAgentSelectors(t *testing.T) {
	for _, selector := range []string{"", "article", "article.item", "#catalog", ".card > h2", "article a[href]", `a[href^="https://example.test/a,b?q=x:y"]`, `input[name='a>b']`, `[data-id=abc]`, `[class~=selected]`, `a[title="a~b"]`, "h2 + p", "section > article.item > a.title", "article, section.card, #main > h1, a[rel=next]", "a b>c>d>e>f>g>h"} {
		if err := checkSelector(selector); err != nil {
			t.Errorf("safe selector %q: %v", selector, err)
		}
	}
}

func TestSelectorBudgetRejectsRecursiveAndAmbiguousGrammar(t *testing.T) {
	for _, selector := range []string{":has(:has(:has(div)))", "article:not(.private)", ":is(a,b)", "a:first-child", "a:nth-child(2)", `a\:has(div)`, "a/* hidden */b", "a b c", "a~b", "a ~ b", "section > a ~ b", "a ~ b ~ c", "a b ~ c", "a,b,c,d,e", "a>b>c>d>e>f>g>h>i", "a >", "> a", "a,,b", "a + + b", "a[b", `a[b="unterminated]`, "a(b)", "a\x00b", strings.Repeat("a", 513)} {
		if err := checkSelector(selector); !errors.Is(err, ErrAction) {
			t.Errorf("unsafe selector %q: %v", selector, err)
		}
	}
}

func TestHTMLCopyWorkBudgetCountsDecodedBytesAndDepth(t *testing.T) {
	const capBytes = 2 << 20
	for name, body := range map[string]string{
		"text_at_cap":         "<p>" + strings.Repeat("x", capBytes/4) + "</p>",
		"attribute_below_cap": `<p title="` + strings.Repeat("x", capBytes/3) + `"></p>`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := checkEvidenceBudget([]byte(body)); err != nil {
				t.Fatal(err)
			}
		})
	}
	for name, body := range map[string]string{
		"text_over_cap":          "<p>" + strings.Repeat("x", capBytes/4+1) + "</p>",
		"attribute_over_cap":     `<p title="` + strings.Repeat("x", capBytes/3+1) + `"></p>`,
		"deep_short_text":        strings.Repeat("<div>", 40) + strings.Repeat("x", 50000) + strings.Repeat("</div>", 40),
		"multiple_text_nodes":    "<p>" + strings.Repeat("x", 300000) + "</p><p>" + strings.Repeat("x", 300000) + "</p>",
		"text_plus_attribute":    `<p title="` + strings.Repeat("x", 200000) + `">` + strings.Repeat("x", 400000) + "</p>",
		"decoded_multibyte_text": "<p>" + strings.Repeat("&#30028;", capBytes/12+1) + "</p>",
	} {
		t.Run(name, func(t *testing.T) {
			if err := checkEvidenceBudget([]byte(body)); !errors.Is(err, ErrBudget) {
				t.Fatalf("expected copy-work budget, got %v", err)
			}
		})
	}
	// JSON extraction shares decoded strings rather than repeatedly building
	// ancestor text. Its 8 MiB/body, depth and node bounds still apply.
	if err := checkEvidenceBudget([]byte(`{"value":"` + strings.Repeat("x", 3<<20) + `"}`)); err != nil {
		t.Fatalf("HTML work bound leaked into JSON: %v", err)
	}
}

func TestRuleComplexityChecksEverySelectorAndJSONPointer(t *testing.T) {
	base := crawlrule.Rule{Format: "html", Rows: "article", Fields: []crawlrule.Field{{Name: "title", Selector: "h1"}}, Next: &crawlrule.Next{Selector: "a[rel=next]"}}
	if err := checkRuleComplexity(base); err != nil {
		t.Fatal(err)
	}
	for _, where := range []string{"rows", "field", "next"} {
		t.Run(where, func(t *testing.T) {
			r := base
			r.Fields = append([]crawlrule.Field(nil), base.Fields...)
			n := *base.Next
			r.Next = &n
			switch where {
			case "rows":
				r.Rows = ":has(a)"
			case "field":
				r.Fields[0].Selector = ":not(a)"
			case "next":
				r.Next.Selector = ":is(a,b)"
			}
			if err := checkRuleComplexity(r); !errors.Is(err, ErrAction) {
				t.Fatal(err)
			}
		})
	}
	for _, where := range []string{"rows", "field", "next"} {
		t.Run("pointer_"+where, func(t *testing.T) {
			r := crawlrule.Rule{Format: "json", Rows: "/items", Fields: []crawlrule.Field{{Pointer: "/title"}}, Next: &crawlrule.Next{Pointer: "/next"}}
			bad := strings.Repeat("/x", 33)
			switch where {
			case "rows":
				r.Rows = bad
			case "field":
				r.Fields[0].Pointer = bad
			case "next":
				r.Next.Pointer = bad
			}
			if err := checkRuleComplexity(r); !errors.Is(err, ErrAction) {
				t.Fatal(err)
			}
		})
	}
	if err := checkRuleComplexity(crawlrule.Rule{Format: "json", Rows: strings.Repeat("/x", 32), Fields: []crawlrule.Field{{Pointer: "/a~1b/~0"}}}); err != nil {
		t.Fatal(err)
	}
}

func TestEvidenceBudgetRejectsDeepOrLargeTrees(t *testing.T) {
	var attributes strings.Builder
	attributes.WriteString("<p")
	for i := 0; i < 20001; i++ {
		attributes.WriteString(" a" + strconv.Itoa(i) + "=x")
	}
	attributes.WriteString(">x</p>")
	for name, body := range map[string]string{
		"html_depth":                 strings.Repeat("<div>", 65) + "x" + strings.Repeat("</div>", 65),
		"html_parser_depth":          strings.Repeat("<div>", 1000),
		"json_depth":                 strings.Repeat("[", 65) + "0" + strings.Repeat("]", 65),
		"html_nodes":                 strings.Repeat("<br>", 20000),
		"json_nodes":                 "[" + strings.Repeat("0,", 20000) + "0]",
		"html_attributes":            attributes.String(),
		"html_foreign_subtree":       "<svg><title>" + strings.Repeat("<g></g>", 20001) + "</title></svg>",
		"html_rawtext_comment_flood": `<script>const x='<!--';</script><svg><title>` + strings.Repeat("<g></g>", 25000) + "</title></svg>",
		"html_token_flood":           strings.Repeat("</x>", 41000),
		"bytes":                      strings.Repeat("x", (8<<20)+1),
	} {
		t.Run(name, func(t *testing.T) {
			if err := checkEvidenceBudget([]byte(body)); !errors.Is(err, ErrBudget) {
				t.Fatalf("got %v", err)
			}
		})
	}
}

func TestEvidenceBudgetUsesActualHTMLTreeAndJSONTokens(t *testing.T) {
	for _, body := range []string{"", "<article><h1>safe</h1></article>", strings.Repeat("<br>", 1000), strings.Repeat("<p>x", 1000), "<table><tr><td>cell</table>", strings.Repeat("<div>", 60) + "x" + strings.Repeat("</div>", 60), strings.Repeat("[", 64) + "0" + strings.Repeat("]", 64), `{"items":[{"title":"safe"}],"literal":"[[[{{{"}`, `"scalar"`, "0", "true", "null", "<script>const x = '[[[[[[[';</script>"} {
		if err := checkEvidenceBudget([]byte(body)); err != nil {
			t.Fatalf("bounded body length %d rejected: %v", len(body), err)
		}
	}
	for _, body := range []string{`{"incomplete":`, `[] []`, `[1,]`} {
		if err := checkEvidenceBudget([]byte(body)); !errors.Is(err, ErrAction) {
			t.Fatalf("malformed JSON accepted: %q %v", body, err)
		}
	}
}
