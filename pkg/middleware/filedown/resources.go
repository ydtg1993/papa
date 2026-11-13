package filedown

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
	if int64(len(s.Completed)) > int64(d.limits.Chunks) || len(s.TempFiles) > d.limits.Chunks {
		return middleware.ResourceLimit("resume chunks", int64(d.limits.Chunks))
	}
	if s.TotalSize > 0 && s.ChunkSize > 0 && (s.TotalSize-1)/s.ChunkSize+1 > int64(d.limits.Chunks) {
		return middleware.ResourceLimit("resume chunks", int64(d.limits.Chunks))
	}
	return nil
}

type resumeEnvelope struct {
	Version int             `json:"version"`
	State   json.RawMessage `json:"state"`
	Done    []byte          `json:"done,omitempty"`
}

func (d *Downloader) encodeResume(s ResumeState) ([]byte, error) {
	var n int64
	if s.TotalSize > 0 && s.ChunkSize > 0 {
		n = (s.TotalSize-1)/s.ChunkSize + 1
	}
	if n > int64(d.limits.Chunks) {
		return nil, middleware.ResourceLimit("resume entries", int64(d.limits.Chunks))
	}
	done := make([]byte, (n+7)/8)
	for _, v := range s.Completed {
		if n == 0 {
			break
		}
		if !isChunkStart(v, s.TotalSize, s.ChunkSize) {
			continue
		}
		i := v / s.ChunkSize
		if i >= 0 && i < n {
			done[i/8] |= 1 << uint(i%8)
		}
	}
	s.Completed = nil
	s.TempFiles = nil
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
		if s.TotalSize > 0 && s.ChunkSize > 0 {
			n = (s.TotalSize-1)/s.ChunkSize + 1
		}
		if int64(len(e.Done)) != (n+7)/8 {
			return middleware.ResourceLimit("invalid resume bitmap", n)
		}
		s.Completed = nil
		for i := int64(0); i < n; i++ {
			if e.Done[i/8]&(1<<uint(i%8)) != 0 {
				s.Completed = append(s.Completed, i*s.ChunkSize)
			}
		}
	}
	return nil
}
