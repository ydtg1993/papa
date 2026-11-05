package engine

import (
	"fmt"
	"strconv"
	"time"

	"github.com/ydtg1993/papa/v3/obs/incident"
)

type breakerEvidence struct {
	samples     []incident.Sample
	resumed     time.Time
	lostThrough time.Time
}

func (e *Engine) resetBreakerEvidenceLocked(site string) {
	if e.breakerSamples == nil {
		e.breakerSamples = map[string]*breakerEvidence{}
	}
	e.breakerSamples[site] = &breakerEvidence{resumed: time.Now()}
}

func (e *Engine) recordBreakerSampleLocked(site string, sample incident.Sample) {
	if e.breakerSamples == nil {
		e.breakerSamples = map[string]*breakerEvidence{}
	}
	b := e.breakerSamples[site]
	if b == nil {
		b = &breakerEvidence{}
		e.breakerSamples[site] = b
	}
	from := sample.At.Add(-e.siteBreaker[site].Window())
	n := 0
	for _, s := range b.samples {
		if !s.At.Before(from) {
			b.samples[n] = s
			n++
		}
	}
	clear(b.samples[n:])
	b.samples = b.samples[:n]
	if len(b.samples) == incident.MaxSamples {
		b.lostThrough = b.samples[0].At
		copy(b.samples, b.samples[1:])
		b.samples = b.samples[:len(b.samples)-1]
	}
	b.samples = append(b.samples, sample)
}

func (e *Engine) freezeBreakerIncidentLocked(st BreakerStatus) {
	b := e.breakerSamples[st.Site]
	e.breakerIncidentSeq++
	c := incident.Capture{ID: fmt.Sprintf("%d-%d", st.PausedAt.UnixNano(), e.breakerIncidentSeq), Site: st.Site, At: st.PausedAt, From: st.PausedAt.Add(-st.Window), Window: st.Window, Threshold: st.Threshold, Failures: st.Failures}
	if b != nil {
		c.ResumedAt = b.resumed
		if b.resumed.After(c.From) {
			c.From = b.resumed
		}
		c.Limited = !b.lostThrough.IsZero() && !b.lostThrough.Before(c.From)
		for _, s := range b.samples {
			if !s.At.Before(c.From) && !s.At.After(c.At) {
				c.Samples = append(c.Samples, s)
			}
		}
	}
	// Retain newest four per site, at most 32 globally. Release evicted samples.
	count := 0
	for i := len(e.breakerIncidents) - 1; i >= 0; i-- {
		if e.breakerIncidents[i].Site == st.Site {
			count++
			if count >= 4 {
				e.breakerIncidents = append(e.breakerIncidents[:i], e.breakerIncidents[i+1:]...)
			}
		}
	}
	e.breakerIncidents = append(e.breakerIncidents, c)
	if len(e.breakerIncidents) > 32 {
		e.breakerIncidents[0] = incident.Capture{}
		e.breakerIncidents = e.breakerIncidents[1:]
	}
}

// BreakerIncidents returns detached, bounded captures newest first. Analysis and
// serialization happen in the caller, outside the worker's critical section.
func (e *Engine) BreakerIncidents(site string) []incident.Capture {
	e.breakerMu.Lock()
	defer e.breakerMu.Unlock()
	out := []incident.Capture{}
	for i := len(e.breakerIncidents) - 1; i >= 0; i-- {
		c := e.breakerIncidents[i]
		if c.Site != site {
			continue
		}
		c.Samples = append([]incident.Sample(nil), c.Samples...)
		out = append(out, c)
	}
	return out
}

func (e *Engine) breakerIncidentAttrs(st BreakerStatus) map[string]string {
	attrs := map[string]string{"window_ns": strconv.FormatInt(int64(st.Window), 10)}
	e.breakerMu.Lock()
	defer e.breakerMu.Unlock()
	for i := len(e.breakerIncidents) - 1; i >= 0; i-- {
		c := e.breakerIncidents[i]
		if c.Site == st.Site && c.At.Equal(st.PausedAt) {
			attrs["incident_id"] = c.ID
			attrs["window_from"] = c.From.Format(time.RFC3339Nano)
			if !c.ResumedAt.IsZero() {
				attrs["resumed_at"] = c.ResumedAt.Format(time.RFC3339Nano)
			}
			break
		}
	}
	return attrs
}
