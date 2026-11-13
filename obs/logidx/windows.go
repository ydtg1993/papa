package logidx

import (
	"context"
	"github.com/ydtg1993/papa/v4/obs/inputbudget"
	"sort"
	"time"
)

// Window is one closed evidence interval for a batched scan.
type Window struct{ From, To time.Time }

// BuildWindowsContext scans source logs once for all windows. It preserves
// each independent Build's file, malformed, truncation and sample ordering.
// Aggregate accepted bytes across windows are bounded, not just each index.
func BuildWindowsContext(ctx context.Context, cfg Config, windows []Window, limits inputbudget.Limits) ([]*Index, error) {
	limits = limits.Normalize()
	if err := inputbudget.Check("days", int64(len(windows)), int64(limits.MaxDays)); err != nil {
		return nil, err
	}
	if err := inputbudget.Check("files", int64(len(cfg.Paths)), int64(limits.MaxFiles)); err != nil {
		return nil, err
	}
	max := cfg.MaxEntries
	if max <= 0 {
		max = defaultMaxEntries
	}
	samples := cfg.MaxSamples
	if samples <= 0 {
		samples = defaultMaxSamples
	}
	loc := cfg.Loc
	if loc == nil {
		loc = time.UTC
	}
	out := make([]*Index, len(windows))
	counts := make([]int, len(windows))
	groups := make([]map[string]*Signature, len(windows))
	for i := range out {
		out[i] = &Index{}
		groups[i] = make(map[string]*Signature)
	}
	meter := inputbudget.New(limits)
	var retained int64
	for _, path := range cfg.Paths {
		active := false
		for _, idx := range out {
			if !idx.Truncated {
				idx.Files++
				active = true
			}
		}
		if !active {
			break
		}
		_, err := visitEntries(ctx, meter, path, loc, func(e Entry) bool {
			alive := false
			for i, w := range windows {
				if out[i].Truncated {
					continue
				}
				if !withinWindow(e.Time, w.From, w.To) {
					alive = true
					continue
				}
				if counts[i] >= max {
					out[i].Truncated = true
					continue
				}
				counts[i]++
				retained += int64(len(e.Msg) + len(e.Raw))
				for k, v := range e.Fields {
					retained += int64(len(k) + len(v))
				}
				if retained > limits.MaxRetainedBytes {
					return false
				}
				p := signatureOf(e.Msg)
				sig := groups[i][p]
				if sig == nil {
					sig = &Signature{Pattern: p}
					groups[i][p] = sig
				}
				sig.add(e, samples)
				alive = true
			}
			return alive
		}, func() {
			for _, idx := range out {
				if !idx.Truncated {
					idx.Unparsed++
				}
			}
		})
		if err != nil {
			return nil, err
		}
		if retained > limits.MaxRetainedBytes {
			return nil, &inputbudget.LimitError{Resource: "retained log bytes", Limit: limits.MaxRetainedBytes}
		}
	}
	for i, idx := range out {
		idx.Signatures = make([]Signature, 0, len(groups[i]))
		for _, sig := range groups[i] {
			idx.Signatures = append(idx.Signatures, *sig)
		}
		sort.Slice(idx.Signatures, func(a, b int) bool {
			x, y := idx.Signatures[a], idx.Signatures[b]
			if x.Count != y.Count {
				return x.Count > y.Count
			}
			return x.Pattern < y.Pattern
		})
	}
	return out, ctx.Err()
}
