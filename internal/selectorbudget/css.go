// Package selectorbudget validates the shared conservative CSS subset used by
// model-generated rules and controlled browser waits. It is not a general CSS API.
package selectorbudget

import (
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/andybalholm/cascadia"
)

const (
	maxBytes     = 512
	maxBranches  = 4
	maxCompounds = 8
)

var ErrInvalid = errors.New("selectorbudget: invalid or excessive CSS")

// Validate accepts tag/class/id/attribute compounds, direct-child and
// adjacent-sibling combinators, and at most one descendant combinator per
// branch. General siblings (~), pseudo-classes/elements, comments and escapes are
// excluded. This avoids recursively nested :has/:not/:is matching. Empty is
// allowed for crawlrule's "use the row itself" field-selector convention.
func Validate(selector string) error {
	if selector == "" {
		return nil
	}
	if len(selector) > maxBytes || !utf8.ValidString(selector) || strings.ContainsRune(selector, '\\') {
		return ErrInvalid
	}
	p := selectorParser{s: selector}
	p.space()
	branches := 0
	for {
		branches++
		if branches > maxBranches {
			return ErrInvalid
		}
		compounds, descendants := 0, 0
		for {
			if !p.compound() {
				return ErrInvalid
			}
			compounds++
			if compounds > maxCompounds {
				return ErrInvalid
			}
			space := p.space()
			if p.i == len(p.s) || p.s[p.i] == ',' {
				break
			}
			switch p.s[p.i] {
			case '>', '+':
				p.i++
				p.space()
			case '~':
				return ErrInvalid
			default:
				if !space {
					return ErrInvalid
				}
				descendants++
			}
			if descendants > 1 {
				return ErrInvalid
			}
		}
		if p.i == len(p.s) {
			break
		}
		p.i++
		p.space()
	}
	// Compilation is now bounded by the nonrecursive grammar above. Retain
	// the real matcher's syntax validation instead of assuming equivalence.
	if _, err := cascadia.Compile(selector); err != nil {
		return ErrInvalid
	}
	return nil
}

type selectorParser struct {
	s string
	i int
}

func cssSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '\f' }
func identStart(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c == '-' || c >= 0x80
}
func identChar(c byte) bool { return identStart(c) || c >= '0' && c <= '9' }

func (p *selectorParser) space() bool {
	start := p.i
	for p.i < len(p.s) && cssSpace(p.s[p.i]) {
		p.i++
	}
	return p.i != start
}

func (p *selectorParser) identifier() bool {
	if p.i == len(p.s) || !identStart(p.s[p.i]) {
		return false
	}
	p.i++
	for p.i < len(p.s) && identChar(p.s[p.i]) {
		p.i++
	}
	return true
}

func (p *selectorParser) compound() bool {
	start := p.i
	if p.i < len(p.s) && p.s[p.i] == '*' {
		p.i++
	} else {
		p.identifier()
	}
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case '.', '#':
			p.i++
			if !p.identifier() {
				return false
			}
		case '[':
			if !p.attribute() {
				return false
			}
		default:
			return p.i > start
		}
	}
	return p.i > start
}

func (p *selectorParser) attribute() bool {
	p.i++
	p.space()
	if !p.identifier() {
		return false
	}
	p.space()
	if p.i == len(p.s) {
		return false
	}
	if p.s[p.i] == ']' {
		p.i++
		return true
	}
	if strings.ContainsRune("~|^$*", rune(p.s[p.i])) {
		p.i++
	}
	if p.i == len(p.s) || p.s[p.i] != '=' {
		return false
	}
	p.i++
	p.space()
	if p.i == len(p.s) {
		return false
	}
	if quote := p.s[p.i]; quote == '\'' || quote == '"' {
		p.i++
		for p.i < len(p.s) && p.s[p.i] != quote {
			if p.s[p.i] < 0x20 || p.s[p.i] == 0x7f {
				return false
			}
			p.i++
		}
		if p.i == len(p.s) {
			return false
		}
		p.i++
	} else {
		start := p.i
		for p.i < len(p.s) && identChar(p.s[p.i]) {
			p.i++
		}
		if p.i == start {
			return false
		}
	}
	space := p.space()
	if space && p.i < len(p.s) && (p.s[p.i] == 'i' || p.s[p.i] == 'I') {
		p.i++
		p.space()
	}
	if p.i == len(p.s) || p.s[p.i] != ']' {
		return false
	}
	p.i++
	return true
}
