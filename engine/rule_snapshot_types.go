package engine

import (
	"time"

	"github.com/ydtg1993/papa/v3/pkg/extract"
)

// Legacy site/stage sanitization replaces dots, so this namespace cannot
// overlap an existing site's archive directory.
const ruleSnapshotDir = ".rule-snapshots"
const ruleSnapshotTraceStep = "规则页面证据"

// ruleSnapshotManifest is an observation, not another copy of the response.
// BodySHA256 hashes the sanitized stored bytes; OriginalSHA256 identifies the
// response evaluated by the rule. No request headers, body or credentials live here.
type ruleSnapshotManifest struct {
	Version           int             `json:"version"`
	EvidenceID        string          `json:"evidence_id"`
	Rule              RuleRef         `json:"rule"`
	ReleaseID         string          `json:"release_id,omitempty"`
	ReleaseGeneration uint64          `json:"release_generation,omitempty"`
	TaskID            int             `json:"task_id"`
	Site              string          `json:"site"`
	Stage             string          `json:"stage"`
	Attempt           int             `json:"attempt"`
	Retry             int             `json:"retry"`
	Repeat            int             `json:"repeat"`
	SourceURL         string          `json:"source_url,omitempty"`
	Format            string          `json:"format"`
	Rendered          bool            `json:"rendered"`
	StatusCode        int             `json:"status_code"`
	OriginalSHA256    string          `json:"original_sha256,omitempty"`
	BodySHA256        string          `json:"body_sha256,omitempty"`
	BodyFile          string          `json:"body_file,omitempty"`
	BodyBytes         int             `json:"body_bytes"`
	BodyOmitted       string          `json:"body_omitted,omitempty"`
	Failed            bool            `json:"failed"`
	Validation        *extract.Report `json:"validation,omitempty"`
	CreatedAt         time.Time       `json:"created_at"`
}
