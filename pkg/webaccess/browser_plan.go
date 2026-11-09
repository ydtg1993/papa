package webaccess

import (
	"errors"
	"time"

	"github.com/ydtg1993/papa/v4/internal/selectorbudget"
)

var ErrBrowserPlan = errors.New("webaccess: invalid browser plan")

// BrowserPlan is a host-reviewed action contract, never executable model code.
// Zero values are not defaulted: the exact reviewed limits must be persisted.
type BrowserPlan struct {
	Wait           *BrowserWait       `json:"wait,omitempty"`
	Scroll         *BrowserScroll     `json:"scroll,omitempty"`
	Pagination     *BrowserPagination `json:"pagination,omitempty"`
	AllowTruncated bool               `json:"allow_truncated,omitempty"`
}
type BrowserWait struct {
	Selector  string `json:"selector"`
	State     string `json:"state"`
	TimeoutMS int    `json:"timeout_ms"`
}
type BrowserScroll struct {
	ItemSelector string `json:"item_selector"`
	KeyAttribute string `json:"key_attribute"`
	MaxRounds    int    `json:"max_rounds"`
	IntervalMS   int    `json:"interval_ms"`
	StableRounds int    `json:"stable_rounds"`
}
type BrowserPagination struct {
	Selector string `json:"selector"`
	Mode     string `json:"mode"` // link or button
	MaxPages int    `json:"max_pages"`
	WaitMS   int    `json:"wait_ms"`
}

func (p BrowserPlan) Validate() error {
	if p.Wait != nil {
		w := p.Wait
		if ValidateRenderWait(RenderWait{Selector: w.Selector, State: w.State, Timeout: time.Duration(w.TimeoutMS) * time.Millisecond}) != nil || w.TimeoutMS > 30000 || w.TimeoutMS < 1 {
			return ErrBrowserPlan
		}
	}
	if s := p.Scroll; s != nil {
		if selectorbudget.Validate(s.ItemSelector) != nil || s.ItemSelector == "" || (s.KeyAttribute != "href" && s.KeyAttribute != "data-id" && s.KeyAttribute != "src" && s.KeyAttribute != "text") || s.MaxRounds < 1 || s.MaxRounds > 12 || s.IntervalMS < 500 || s.IntervalMS > 10000 || s.StableRounds < 1 || s.StableRounds > s.MaxRounds {
			return ErrBrowserPlan
		}
	}
	if n := p.Pagination; n != nil {
		if selectorbudget.Validate(n.Selector) != nil || n.Selector == "" || (n.Mode != "link" && n.Mode != "button") || n.MaxPages < 1 || n.MaxPages > 8 || n.WaitMS < 500 || n.WaitMS > 10000 {
			return ErrBrowserPlan
		}
	}
	return nil
}

type BrowserStep struct {
	Page  int    `json:"page"`
	Kind  string `json:"kind"`
	Count int    `json:"count"`
}
type BrowseResult struct {
	Pages      []Response    `json:"pages"`
	Steps      []BrowserStep `json:"steps"`
	StopReason string        `json:"stop_reason"`
	Truncated  bool          `json:"truncated"`
}
