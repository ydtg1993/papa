package events

import "strings"

// Fixed budgets bound producer work and retained payload per event.
const (
	MaxStringBytes      = 2048
	MaxEventStringBytes = 16384
	MaxMapEntries       = 64
)

const omittedString = "[omitted: event string budget]"

// Detach makes a bounded owned copy before asynchronous delivery. The caller
// must not mutate maps during this call; it may reuse them immediately afterward.
// complete is false when any payload was omitted, including prior truncation.
func Detach(e Event) (out Event, complete bool) {
	out = e
	remaining, entries := MaxEventStringBytes, MaxMapEntries
	cut := func(s string) string {
		// Preserve evidence when a caller used the legacy string-only helper.
		if s == omittedString {
			out.Truncated = true
		}
		n := len(s)
		if n > MaxStringBytes {
			n = MaxStringBytes
		}
		if n > remaining {
			n = remaining
		}
		if n < len(s) {
			out.Truncated = true
			// Redaction happens downstream. Keeping a prefix can remove the
			// delimiter that identifies URL userinfo or a DSN and expose secrets.
			// Omit the whole value without scanning unbounded producer input.
			if len(omittedString) <= n {
				remaining -= len(omittedString)
				return omittedString
			}
			return ""
		}
		remaining -= n
		return strings.Clone(s[:n])
	}
	for _, p := range []*string{&out.RunID, &out.Type, &out.Site, &out.Stage, &out.URL, &out.Kind, &out.Outcome, &out.Queue, &out.Source, &out.Error} {
		*p = cut(*p)
	}
	out.Counts = nil
	out.Attrs = nil
	out.Stages = nil
	out.Queues = nil
	if e.Counts != nil {
		out.Counts = make(map[string]int)
	}
	for k, v := range e.Counts {
		if entries == 0 || len(k) > MaxStringBytes || len(k) > remaining {
			out.Truncated = true
			break
		}
		entries--
		out.Counts[cut(k)] = v
	}
	if e.Attrs != nil {
		out.Attrs = make(map[string]string)
	}
	for k, v := range e.Attrs {
		if entries == 0 || len(k) > MaxStringBytes || len(k) > remaining {
			out.Truncated = true
			break
		}
		entries--
		key := cut(k)
		out.Attrs[key] = cut(v)
	}
	if e.Stages != nil {
		out.Stages = make(map[string]StageSnapshot)
	}
	for k, v := range e.Stages {
		if entries == 0 || len(k) > MaxStringBytes || len(k) > remaining {
			out.Truncated = true
			break
		}
		entries--
		key := cut(k)
		v.Site = cut(v.Site)
		v.Stage = cut(v.Stage)
		out.Stages[key] = v
	}
	if e.Queues != nil {
		out.Queues = make(map[string]QueueSnapshot)
	}
	for k, v := range e.Queues {
		if entries == 0 || len(k) > MaxStringBytes || len(k) > remaining {
			out.Truncated = true
			break
		}
		entries--
		key := cut(k)
		v.LastError = cut(v.LastError)
		out.Queues[key] = v
	}
	return out, !out.Truncated
}
