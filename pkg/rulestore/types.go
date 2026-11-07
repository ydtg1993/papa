// Package rulestore stores immutable rule and sample metadata in MySQL, sample
// bodies in a content-addressed file directory, and publishes complete sites.
package rulestore

import (
	"errors"
	"time"

	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
)

const (
	VerifierVersion  = "crawlrule-fixtures-v3"
	MaxFixtures      = 64
	MaxExpectations  = 4096
	MaxSetBytes      = 32 << 20
	MaxMetadataBytes = 1 << 20
	MaxBindings      = 32
	MaxQueryLimit    = 100
)

var (
	ErrConflict           = errors.New("rulestore: immutable identifier already exists")
	ErrGenerationConflict = errors.New("rulestore: publication generation conflict")
	ErrOperationConflict  = errors.New("rulestore: operation ID reused with different parameters")
	ErrNotFound           = errors.New("rulestore: record not found")
	ErrUnverified         = errors.New("rulestore: rule has no matching successful verification")
	ErrNotPublished       = errors.Join(errors.New("rulestore: rule has never been published"), crawlrule.ErrUnverifiedRule)
	ErrNoRelease          = errors.New("rulestore: site has no active release")
	ErrNoRollback         = errors.New("rulestore: no earlier release to roll back to")
	ErrCorrupt            = errors.New("rulestore: persisted content failed integrity validation")
)

type Reference = crawlrule.Reference

// Actor is audit metadata, not authentication. The host must authenticate and
// authorize its caller before supplying these fields. Both fields are required.
type Actor struct {
	Operator string `json:"operator"`
	Source   string `json:"source"`
}

type Version struct {
	Reference
	Rule          crawlrule.Rule
	Digest        string
	State         string
	EverPublished bool
	Actor         Actor
	CreatedAt     time.Time
}

type Sample struct {
	ID string
	SanitizedSample
	Actor     Actor
	CreatedAt time.Time
}

// StoredFixture fixes a sample and its explicit field expectations. The store
// adds its immutable digest to the set before computing the set's digest.
type StoredFixture struct {
	Name     string                 `json:"name"`
	SampleID string                 `json:"sample_id"`
	Expect   crawlrule.Expectations `json:"expect"`
}

type SampleSet struct {
	ID        string
	Digest    string
	Fixtures  []StoredFixture
	Actor     Actor
	CreatedAt time.Time
}

type Verification struct {
	ID string
	Reference
	RuleDigest      string
	SetID           string
	SetDigest       string
	VerifierVersion string
	Passed          bool
	Report          crawlrule.FixtureReport
	Actor           Actor
	CreatedAt       time.Time
}

type Release struct {
	crawlrule.ReleaseSnapshot
	OperationID       string
	Kind              string
	PreviousReleaseID string
	VerificationIDs   map[string]string
	Actor             Actor
	CreatedAt         time.Time
}
