package rulestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"

	"github.com/ydtg1993/papa/v3/pkg/htmlfetch"
)

// ReadSnapshot reads one complete, already retained RuleFetcher observation.
// The result must still pass an explicit SamplePolicy before sample insertion.
// It never fetches the source URL or changes the short-lived archive's retention.
func ReadSnapshot(ctx context.Context, archiveDir, evidenceID string) (SampleInput, error) {
	if err := ctx.Err(); err != nil {
		return SampleInput{}, err
	}
	invalid := errors.New("rulestore: snapshot missing, incomplete or corrupt")
	if !snapshotHex(evidenceID, 32) {
		return SampleInput{}, invalid
	}
	r, err := os.OpenRoot(archiveDir)
	if err != nil {
		return SampleInput{}, invalid
	}
	defer r.Close()
	read := func(path string, limit int) ([]byte, error) {
		info, err := r.Lstat(path)
		if err != nil || !info.Mode().IsRegular() {
			return nil, invalid
		}
		f, err := r.Open(path)
		if err != nil {
			return nil, invalid
		}
		defer f.Close()
		opened, err := f.Stat()
		if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
			return nil, invalid
		}
		data, err := io.ReadAll(io.LimitReader(f, int64(limit)+1))
		if err != nil || len(data) > limit {
			return nil, invalid
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return data, nil
	}
	data, err := read(".rule-snapshots/observations/"+evidenceID+".json", 256<<10)
	if err != nil {
		return SampleInput{}, err
	}
	// The envelope's extra metadata is deliberately ignored. Version and body
	// location/digest are strict; a future schema cannot be mistaken for v1.
	var m struct {
		Version     int    `json:"version"`
		EvidenceID  string `json:"evidence_id"`
		SourceURL   string `json:"source_url"`
		StatusCode  int    `json:"status_code"`
		Rendered    bool   `json:"rendered"`
		BodyFile    string `json:"body_file"`
		BodySHA256  string `json:"body_sha256"`
		BodyBytes   int    `json:"body_bytes"`
		BodyOmitted string `json:"body_omitted"`
	}
	d := json.NewDecoder(bytes.NewReader(data))
	if d.Decode(&m) != nil || d.Decode(new(any)) != io.EOF || m.Version != 1 || m.EvidenceID != evidenceID || m.BodyOmitted != "" || !snapshotHex(m.BodySHA256, 64) || m.BodyFile != ".rule-snapshots/bodies/"+m.BodySHA256+".body" || m.BodyBytes < 0 || m.BodyBytes > MaxSampleBodyBytes {
		return SampleInput{}, invalid
	}
	body, err := read(m.BodyFile, MaxSampleBodyBytes)
	if err != nil {
		return SampleInput{}, err
	}
	hash := sha256.Sum256(body)
	if len(body) != m.BodyBytes || hex.EncodeToString(hash[:]) != m.BodySHA256 {
		return SampleInput{}, invalid
	}
	if !validSampleStatus(m.StatusCode, m.Rendered) || len(m.SourceURL) > MaxSampleURLBytes {
		return SampleInput{}, invalid
	}
	if _, err := sanitizeSampleURL(m.SourceURL, nil); err != nil {
		return SampleInput{}, invalid
	}
	return SampleInput{URL: m.SourceURL, StatusCode: m.StatusCode, Rendered: m.Rendered, Body: body, EvidenceID: evidenceID}, nil
}

// ImportSnapshot promotes a selected archive observation into the independent
// sample library. Archive GC may later remove its original files safely.
func (r *Repository) ImportSnapshot(ctx context.Context, archiveDir, evidenceID string, policy htmlfetch.SamplePolicy, actor Actor) (Sample, error) {
	input, err := ReadSnapshot(ctx, archiveDir, evidenceID)
	if err != nil {
		return Sample{}, err
	}
	return r.AddSample(ctx, input, policy, actor)
}

func snapshotHex(value string, n int) bool {
	if len(value) != n {
		return false
	}
	for _, c := range value {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
