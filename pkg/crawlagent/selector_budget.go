package crawlagent

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/ydtg1993/papa/v3/internal/selectorbudget"
	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
	"golang.org/x/net/html"
)

const (
	maxAgentPointerSegments   = 32
	maxAgentEvidenceBytes     = 8 << 20
	maxAgentEvidenceDepth     = 64
	maxAgentEvidenceNodes     = 20000
	maxAgentHTMLCopyWorkBytes = 2 << 20
)

// checkRuleComplexity runs before crawlrule.Compile. Agent-generated rules use
// a deliberately smaller CSS language than the general deterministic library.
func checkRuleComplexity(rule crawlrule.Rule) error {
	if len(rule.Fields) > 32 {
		return ErrAction
	}
	switch rule.Format {
	case "html":
		if err := checkSelector(rule.Rows); err != nil {
			return err
		}
		for _, field := range rule.Fields {
			if err := checkSelector(field.Selector); err != nil {
				return err
			}
		}
		if rule.Next != nil {
			return checkSelector(rule.Next.Selector)
		}
	case "json":
		if err := checkPointerComplexity(rule.Rows); err != nil {
			return err
		}
		for _, field := range rule.Fields {
			if err := checkPointerComplexity(field.Pointer); err != nil {
				return err
			}
		}
		if rule.Next != nil {
			return checkPointerComplexity(rule.Next.Pointer)
		}
	default:
		return ErrAction
	}
	return nil
}

func checkPointerComplexity(pointer string) error {
	if pointer == "" {
		return nil
	}
	if len(pointer) > 1024 || !utf8.ValidString(pointer) || pointer[0] != '/' || strings.Count(pointer, "/") > maxAgentPointerSegments {
		return ErrAction
	}
	for i := 0; i < len(pointer); i++ {
		if pointer[i] == '~' {
			if i+1 >= len(pointer) || (pointer[i+1] != '0' && pointer[i+1] != '1') {
				return ErrAction
			}
			i++
		}
	}
	return nil
}

func checkSelector(selector string) error {
	if selectorbudget.Validate(selector) != nil {
		return ErrAction
	}
	return nil
}

// checkEvidenceBudget bounds work before any selector traversal or JSON decode.
// The check is synchronous: no abandoned goroutine keeps evaluating hostile
// input after a timeout. HTML is checked against its actual HTML5 tree, so
// void elements, implied end tags and table repair do not invent false depth.
func checkEvidenceBudget(body []byte) error {
	if len(body) > maxAgentEvidenceBytes {
		return ErrBudget
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[' || json.Valid(trimmed)) {
		return checkJSONTreeBudget(trimmed)
	}
	return checkHTMLTreeBudget(body)
}

func checkJSONTreeBudget(body []byte) error {
	d := json.NewDecoder(bytes.NewReader(body))
	d.UseNumber()
	depth, nodes, roots := 0, 0, 0
	for {
		token, err := d.Token()
		if err == io.EOF {
			if roots == 1 && depth == 0 {
				return nil
			}
			return ErrAction
		}
		if err != nil {
			return ErrAction
		}
		if depth == 0 {
			roots++
			if roots > 1 {
				return ErrAction
			}
		}
		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{', '[':
				depth++
				nodes++
			case '}', ']':
				depth--
			default:
				return ErrAction
			}
		} else {
			nodes++
		}
		// Property names count as nodes too, a conservative allocation bound.
		if depth > maxAgentEvidenceDepth || nodes > maxAgentEvidenceNodes {
			return ErrBudget
		}
	}
}

func checkHTMLTreeBudget(body []byte) error {
	// Reject token/attribute floods before allocating a complete tree. Do not
	// use a lexical tag stack: HTML5 implied closures differ from raw nesting.
	// Count raw delimiters too: script/comment/foreign-content tokenization
	// differs without the tree builder and must not hide a large tag stream.
	if bytes.Count(body, []byte{'<'}) > 2*maxAgentEvidenceNodes+maxAgentEvidenceDepth {
		return ErrBudget
	}
	z := html.NewTokenizer(bytes.NewReader(body))
	z.SetMaxBuf(maxAgentEvidenceBytes)
	tokens, allocations := 0, 0
	for {
		typ := z.Next()
		if typ == html.ErrorToken {
			if z.Err() != io.EOF {
				return ErrAction
			}
			break
		}
		tokens++
		if tokens > 2*maxAgentEvidenceNodes+maxAgentEvidenceDepth {
			return ErrBudget
		}
		if typ != html.EndTagToken {
			allocations++
		}
		if typ == html.StartTagToken || typ == html.SelfClosingTagToken {
			_, more := z.TagName()
			for more {
				_, _, more = z.TagAttr()
				allocations++
				if allocations > maxAgentEvidenceNodes {
					return ErrBudget
				}
			}
			// Without the HTML tree builder's foreign-content state, the
			// tokenizer can otherwise treat an SVG <title> as raw text and
			// hide a large actual subtree from this allocation preflight.
			// Counting apparent markup inside real script/text elements is
			// conservative; the repaired DOM check remains authoritative.
			z.NextIsNotRawText()
		}
		if allocations > maxAgentEvidenceNodes {
			return ErrBudget
		}
	}
	// The pinned x/net/html parser itself caps its open-element stack at 512;
	// our iterative walk applies the stricter depth=64 and total-node=20000
	// limits to the repaired tree, including implicitly inserted nodes.
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return ErrBudget
	}
	type entry struct {
		node  *html.Node
		depth int
	}
	stack := []entry{{doc, 0}}
	nodes := 0
	var copyWork int64
	for len(stack) > 0 {
		last := len(stack) - 1
		current := stack[last]
		stack = stack[:last]
		nodes += 1 + len(current.node.Attr)
		if current.depth > maxAgentEvidenceDepth || nodes > maxAgentEvidenceNodes {
			return ErrBudget
		}
		// Bound potential repeated ancestor-text extraction, including decoded
		// attribute values conservatively. A 2 MiB weighted input bound and
		// at most 32 fields limit body-copy work to about 64 MiB. This is an
		// input-copy work limit, not a process RSS or transformation-output
		// guarantee; JSON strings do not need ancestor-text copies.
		var payload int64
		if current.node.Type == html.TextNode {
			payload = int64(len(current.node.Data))
		}
		for _, attribute := range current.node.Attr {
			payload += int64(len(attribute.Val))
		}
		depth := int64(current.depth)
		if depth > 0 && payload > (maxAgentHTMLCopyWorkBytes-copyWork)/depth {
			return ErrBudget
		}
		copyWork += payload * depth
		for child := current.node.LastChild; child != nil; child = child.PrevSibling {
			if nodes+len(stack) >= maxAgentEvidenceNodes {
				return ErrBudget
			}
			stack = append(stack, entry{child, current.depth + 1})
		}
	}
	return nil
}
