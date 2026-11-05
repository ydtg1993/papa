package htmlfetch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"

	"github.com/PuerkitoBio/goquery"
	"github.com/andybalholm/cascadia"
)

// DecodeJSON decodes exactly one JSON value, retaining integer precision in
// interface{} destinations via json.Number. It does not enforce Content-Type.
func DecodeJSON(data []byte, dst any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.UseNumber()
	if err := d.Decode(dst); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	var trailing any
	if err := d.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("decode JSON: multiple values")
		}
		return fmt.Errorf("decode JSON trailing data: %w", err)
	}
	return nil
}

func (r *Response) DecodeJSON(dst any) error { return DecodeJSON(r.Body, dst) }

// JSONBlocks extracts valid JSON from each matched node's text, in document order.
// It never evaluates JavaScript. No matches, invalid selectors and invalid JSON
// are errors; callers can distinguish a missing optional block before calling.
func (d *Document) JSONBlocks(selector string) ([]json.RawMessage, error) {
	matcher, err := cascadia.Compile(selector)
	if err != nil {
		return nil, fmt.Errorf("JSON selector: %w", err)
	}
	nodes := d.selection.FindMatcher(matcher)
	if nodes.Length() == 0 {
		return nil, fmt.Errorf("JSON selector %q matched no nodes", selector)
	}
	blocks := make([]json.RawMessage, 0, nodes.Length())
	nodes.EachWithBreak(func(i int, node *goquery.Selection) bool {
		raw := []byte(node.Text())
		if !json.Valid(raw) {
			err = fmt.Errorf("JSON selector %q node %d: invalid JSON", selector, i)
			return false
		}
		blocks = append(blocks, json.RawMessage(raw))
		return true
	})
	if err != nil {
		return nil, err
	}
	return blocks, nil
}
