package m3u8

import (
	"encoding/json"
	"github.com/ydtg1993/papa/v4/pkg/middleware"
)

// WithResources returns a clone with immutable local limits and shared admission.
// Pass the same budget to independent downloaders to bound their combined work.
func (d *Downloader) WithResources(l middleware.DownloadLimits, b *middleware.DownloadBudget) *Downloader {
	copy := *d
	copy.limits = l.Normalized()
	if b != nil {
		copy.budget = b
	}
	return &copy
}
func (d *Downloader) validateResume(s *ResumeState) error {
	if s.TotalSegments < 0 {
		return middleware.ResourceLimit("negative resume count", 0)
	}
	if len(s.Completed) > d.limits.Segments || len(s.SegmentFiles) > d.limits.Segments || s.TotalSegments > d.limits.Segments {
		return middleware.ResourceLimit("resume segments", int64(d.limits.Segments))
	}
	return nil
}

type resumeEnvelope struct {
	Version int             `json:"version"`
	State   json.RawMessage `json:"state"`
	Done    []byte          `json:"done,omitempty"`
}

func (d *Downloader) encodeResume(s ResumeState) ([]byte, error) {
	if len(s.SegmentFiles) != s.TotalSegments {
		state, err := json.Marshal(s)
		if err != nil {
			return nil, err
		}
		return json.Marshal(resumeEnvelope{Version: 1, State: state})
	}
	var n int64
	if s.TotalSegments > 0 {
		n = int64(s.TotalSegments)
	}
	if n > int64(d.limits.Segments) {
		return nil, middleware.ResourceLimit("resume entries", int64(d.limits.Segments))
	}
	done := make([]byte, (n+7)/8)
	for _, v := range s.Completed {
		if n == 0 {
			break
		}
		i := int64(v)
		if i >= 0 && i < n {
			done[i/8] |= 1 << uint(i%8)
		}
	}
	s.Completed = nil
	s.SegmentFiles = nil
	state, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	return json.Marshal(resumeEnvelope{Version: 2, State: state, Done: done})
}
func (d *Downloader) decodeResume(data []byte, s *ResumeState) error {
	var e resumeEnvelope
	if err := json.Unmarshal(data, &e); err != nil {
		return err
	}
	if e.Version == 0 {
		return json.Unmarshal(data, s)
	}
	if e.Version != 1 && e.Version != 2 {
		return middleware.ResourceLimit("unsupported resume version", int64(e.Version))
	}
	if err := json.Unmarshal(e.State, s); err != nil {
		return err
	}
	if err := d.validateResume(s); err != nil {
		return err
	}
	if e.Version == 2 {
		var n int64
		if s.TotalSegments > 0 {
			n = int64(s.TotalSegments)
		}
		if int64(len(e.Done)) != (n+7)/8 {
			return middleware.ResourceLimit("invalid resume bitmap", n)
		}
		s.Completed = nil
		for i := int64(0); i < n; i++ {
			if e.Done[i/8]&(1<<uint(i%8)) != 0 {
				s.Completed = append(s.Completed, int(i))
			}
		}
		s.SegmentFiles = make([]string, s.TotalSegments)
	}
	return nil
}
