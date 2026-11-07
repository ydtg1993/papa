package rulestore

import (
	"context"
	"fmt"
	"net/url"
	"reflect"

	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/crawlrule"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type sampleEncoding struct {
	SchemaVersion int             `json:"schema_version"`
	Sample        SanitizedSample `json:"sample"`
}

type fixtureEncoding struct {
	StoredFixture
	BodyDigest     string `json:"body_digest"`
	MetadataDigest string `json:"metadata_digest"`
}

type setEncoding struct {
	SchemaVersion int               `json:"schema_version"`
	Fixtures      []fixtureEncoding `json:"fixtures"`
}

func (r *Repository) AddSample(ctx context.Context, input SampleInput, policy htmlfetch.SamplePolicy, actor Actor) (Sample, error) {
	if err := validateActor(actor); err != nil {
		return Sample{}, err
	}
	sample, err := SanitizeSample(input, policy)
	if err != nil {
		return Sample{}, err
	}
	hash, err := r.blobs.Put(ctx, sample.Body)
	if err != nil {
		return Sample{}, err
	}
	if hash != sample.BodySHA256 {
		return Sample{}, ErrCorrupt
	}
	data, err := encode(sampleEncoding{1, sample}, MaxMetadataBytes)
	if err != nil {
		return Sample{}, err
	}
	row := models.RuleSample{SampleID: newID(), Digest: hash, MetadataJSON: string(data), Operator: actor.Operator, Source: actor.Source}
	// A failed metadata insert intentionally leaves an unreferenced blob. Removing
	// it here could delete data shared by another concurrently inserted sample.
	if err := r.db.WithContext(ctx).Create(&row).Error; err != nil {
		return Sample{}, dbError(err)
	}
	return r.Sample(ctx, row.SampleID)
}

func decodeSample(row models.RuleSample) (SanitizedSample, error) {
	var stored sampleEncoding
	if err := decode(row.MetadataJSON, &stored, MaxMetadataBytes); err != nil {
		return SanitizedSample{}, err
	}
	s := stored.Sample
	if stored.SchemaVersion != 1 || s.BodySHA256 != row.Digest || s.BodyBytes < 0 || s.BodyBytes > MaxSampleBodyBytes {
		return SanitizedSample{}, ErrCorrupt
	}
	u, err := url.Parse(s.URL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.Fragment != "" {
		return SanitizedSample{}, ErrCorrupt
	}
	if (s.Rendered && s.StatusCode != 0) || (!s.Rendered && (s.StatusCode < 100 || s.StatusCode > 599)) {
		return SanitizedSample{}, ErrCorrupt
	}
	return s, nil
}

func (r *Repository) sampleRow(ctx context.Context, id string) (models.RuleSample, SanitizedSample, error) {
	if err := validateText("sample ID", id, 64); err != nil {
		return models.RuleSample{}, SanitizedSample{}, err
	}
	var row models.RuleSample
	if err := r.db.WithContext(ctx).Where("sample_id = ?", id).Take(&row).Error; err != nil {
		return row, SanitizedSample{}, dbError(err)
	}
	s, err := decodeSample(row)
	return row, s, err
}

// Sample returns metadata only, so listing or inspecting samples does not read
// every body into memory. Verification loads each bounded body separately.
func (r *Repository) Sample(ctx context.Context, id string) (Sample, error) {
	row, s, err := r.sampleRow(ctx, id)
	if err != nil {
		return Sample{}, err
	}
	return Sample{ID: id, SanitizedSample: s, Actor: Actor{row.Operator, row.Source}, CreatedAt: row.CreatedAt}, nil
}

func (r *Repository) CreateSampleSet(ctx context.Context, id string, fixtures []StoredFixture, actor Actor) error {
	if err := validateActor(actor); err != nil {
		return err
	}
	if err := validateText("sample set ID", id, 128); err != nil {
		return err
	}
	if len(fixtures) == 0 || len(fixtures) > MaxFixtures {
		return fmt.Errorf("rulestore: sample set needs 1..%d fixtures", MaxFixtures)
	}
	// Serialize first to detach expectations, preserve JSON numeric precision,
	// bound their memory footprint, and reject unserializable caller values.
	raw, err := encode(fixtures, MaxMetadataBytes)
	if err != nil {
		return err
	}
	var detached []StoredFixture
	if err := decode(string(raw), &detached, MaxMetadataBytes); err != nil {
		return err
	}
	fixtures = detached
	stored := setEncoding{SchemaVersion: 1}
	names := map[string]bool{}
	total := 0
	expectations := 0
	for _, f := range fixtures {
		if err := validateText("fixture name", f.Name, 128); err != nil {
			return err
		}
		if names[f.Name] || crawlrule.ValidateExpectations(f.Expect) != nil {
			return fmt.Errorf("rulestore: fixture names must be unique with bounded explicit expectations")
		}
		names[f.Name] = true
		if f.Expect.Kind != "" {
			stored.SchemaVersion = 2
		}
		expectations += len(f.Expect.Fields)
		if expectations > MaxExpectations {
			return fmt.Errorf("rulestore: sample set exceeds %d field expectations", MaxExpectations)
		}
		row, s, err := r.sampleRow(ctx, f.SampleID)
		if err != nil {
			return err
		}
		total += s.BodyBytes
		if total > MaxSetBytes {
			return fmt.Errorf("rulestore: sample set exceeds %d body bytes", MaxSetBytes)
		}
		body, err := r.blobs.Get(ctx, row.Digest, MaxSampleBodyBytes)
		if err != nil {
			return err
		}
		if len(body) != s.BodyBytes {
			return ErrCorrupt
		}
		stored.Fixtures = append(stored.Fixtures, fixtureEncoding{StoredFixture: f, BodyDigest: row.Digest, MetadataDigest: digest([]byte(row.MetadataJSON))})
	}
	if !hasPositiveFixtures(stored) {
		return fmt.Errorf("rulestore: at least one positive fixture required")
	}
	data, err := encode(stored, MaxMetadataBytes)
	if err != nil {
		return err
	}
	row := models.RuleSampleSet{SetID: id, Digest: digest(data), FixturesJSON: string(data), Operator: actor.Operator, Source: actor.Source}
	return dbError(r.db.WithContext(ctx).Create(&row).Error)
}

func decodeSet(row models.RuleSampleSet) (setEncoding, error) {
	var set setEncoding
	if digest([]byte(row.FixturesJSON)) != row.Digest {
		return set, ErrCorrupt
	}
	if err := decode(row.FixturesJSON, &set, MaxMetadataBytes); err != nil {
		return set, err
	}
	if (set.SchemaVersion != 1 && set.SchemaVersion != 2) || len(set.Fixtures) == 0 || len(set.Fixtures) > MaxFixtures {
		return set, ErrCorrupt
	}
	names := map[string]bool{}
	for _, f := range set.Fixtures {
		if validateText("fixture name", f.Name, 128) != nil || names[f.Name] || crawlrule.ValidateExpectations(f.Expect) != nil {
			return set, ErrCorrupt
		}
		if set.SchemaVersion == 1 && (f.Expect.Kind != "" || f.Expect.ErrorKind != "" || f.Expect.StatusCode != 0) {
			return set, ErrCorrupt
		}
		names[f.Name] = true
	}
	if !hasPositiveFixtures(set) {
		return set, ErrCorrupt
	}
	return set, nil
}

func hasPositiveFixtures(set setEncoding) bool {
	for _, f := range set.Fixtures {
		if f.Expect.Kind == "" || f.Expect.Kind == "positive" {
			return true
		}
	}
	return false
}

func supportedVerifierVersion(version string) bool {
	return version == VerifierVersion || version == "crawlrule-fixtures-v2"
}

func (r *Repository) SampleSet(ctx context.Context, id string) (SampleSet, error) {
	if err := validateText("sample set ID", id, 128); err != nil {
		return SampleSet{}, err
	}
	var row models.RuleSampleSet
	if err := r.db.WithContext(ctx).Where("set_id = ?", id).Take(&row).Error; err != nil {
		return SampleSet{}, dbError(err)
	}
	set, err := decodeSet(row)
	if err != nil {
		return SampleSet{}, err
	}
	out := SampleSet{ID: id, Digest: row.Digest, Actor: Actor{row.Operator, row.Source}, CreatedAt: row.CreatedAt}
	for _, fixture := range set.Fixtures {
		out.Fixtures = append(out.Fixtures, fixture.StoredFixture)
	}
	return out, nil
}

// Verify persists both successful and failed offline reports. A successful
// in-memory evaluation is never returned as success if its transaction fails.
func (r *Repository) Verify(ctx context.Context, ref Reference, setID string, actor Actor) (Verification, error) {
	if err := validateActor(actor); err != nil {
		return Verification{}, err
	}
	if err := validateText("sample set ID", setID, 128); err != nil {
		return Verification{}, err
	}
	v, err := r.Get(ctx, ref)
	if err != nil {
		return Verification{}, err
	}
	c, err := crawlrule.Compile(v.Rule)
	if err != nil {
		return Verification{}, err
	}
	var setRow models.RuleSampleSet
	if err := r.db.WithContext(ctx).Where("set_id = ?", setID).Take(&setRow).Error; err != nil {
		return Verification{}, dbError(err)
	}
	set, err := decodeSet(setRow)
	if err != nil {
		return Verification{}, err
	}
	report := crawlrule.FixtureReport{Passed: true}
	total := 0
	for _, f := range set.Fixtures {
		if err := ctx.Err(); err != nil {
			return Verification{}, err
		}
		row, s, loadErr := r.sampleRow(ctx, f.SampleID)
		// A database read failure must not masquerade as a fixture mismatch.
		if loadErr != nil {
			return Verification{}, loadErr
		}
		if row.Digest != f.BodyDigest || digest([]byte(row.MetadataJSON)) != f.MetadataDigest {
			return Verification{}, ErrCorrupt
		}
		total += s.BodyBytes
		if total > MaxSetBytes {
			return Verification{}, ErrCorrupt
		}
		body, loadErr := r.blobs.Get(ctx, row.Digest, MaxSampleBodyBytes)
		var result crawlrule.FixtureResult
		if loadErr != nil || len(body) != s.BodyBytes {
			result = crawlrule.FixtureResult{Name: f.Name, Kind: f.Expect.Kind, ErrorKind: "execution", Failures: []string{"sample body unavailable or failed integrity validation"}}
		} else {
			u, _ := url.Parse(s.URL)
			part, _ := c.Verify([]crawlrule.Fixture{{Name: f.Name, Response: &htmlfetch.Response{URL: u, StatusCode: s.StatusCode, Body: body}, Rendered: s.Rendered, Expect: f.Expect}})
			result = part.Results[0]
		}
		report.Results = append(report.Results, result)
		report.Passed = report.Passed && result.Passed
	}
	report = crawlrule.SummarizeFixtures(report.Results)
	data, err := encode(report, MaxMetadataBytes)
	if err != nil {
		return Verification{}, err
	}
	row := models.RuleVerification{ID: newID(), RuleID: ref.ID, Version: ref.Version, RuleDigest: v.Digest, SetID: setID, SetDigest: setRow.Digest, VerifierVersion: VerifierVersion, Passed: report.Passed, ReportJSON: string(data), Operator: actor.Operator, Source: actor.Source}
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var current models.RuleVersion
		if err := versionScope(tx.Clauses(clause.Locking{Strength: "UPDATE"}), ref).Take(&current).Error; err != nil {
			return err
		}
		if current.Digest != v.Digest {
			return ErrCorrupt
		}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		state, proof := "pending", ""
		if report.Passed {
			state, proof = "verified", row.ID
		}
		if current.EverPublished {
			state = "published"
		}
		return versionScope(tx.Model(&models.RuleVersion{}), ref).Updates(map[string]any{"state": state, "verification_id": proof}).Error
	})
	if err != nil {
		return Verification{}, dbError(err)
	}
	result, err := r.Verification(ctx, row.ID)
	if err != nil {
		return Verification{}, err
	}
	if !report.Passed {
		return result, crawlrule.ErrFixtureFailed
	}
	return result, nil
}

func publicVerification(row models.RuleVerification) (Verification, error) {
	var report crawlrule.FixtureReport
	if err := decode(row.ReportJSON, &report, MaxMetadataBytes); err != nil {
		return Verification{}, err
	}
	if report.Passed != row.Passed || len(report.Results) == 0 || len(report.Results) > MaxFixtures {
		return Verification{}, ErrCorrupt
	}
	allPassed := true
	names := map[string]bool{}
	for _, result := range report.Results {
		if validateText("fixture name", result.Name, 128) != nil || names[result.Name] || result.Passed != (len(result.Failures) == 0) || result.Rows < 0 || result.InvalidRows < 0 {
			return Verification{}, ErrCorrupt
		}
		if row.VerifierVersion == "crawlrule-fixtures-v2" && (result.Kind != "" || result.ErrorKind != "" || result.StatusCode != 0) {
			return Verification{}, ErrCorrupt
		}
		if row.VerifierVersion == VerifierVersion && !validFixtureResult(result) {
			return Verification{}, ErrCorrupt
		}
		names[result.Name] = true
		allPassed = allPassed && result.Passed
	}
	if row.VerifierVersion == VerifierVersion {
		summary := crawlrule.SummarizeFixtures(report.Results)
		if summary.Passed != report.Passed || summary.Positive != report.Positive || summary.Negative != report.Negative || !reflect.DeepEqual(summary.Failures, report.Failures) {
			return Verification{}, ErrCorrupt
		}
	} else if allPassed != report.Passed || len(report.Failures) != 0 || report.Positive != 0 || report.Negative != 0 {
		return Verification{}, ErrCorrupt
	}
	return Verification{ID: row.ID, Reference: Reference{ID: row.RuleID, Version: row.Version}, RuleDigest: row.RuleDigest, SetID: row.SetID, SetDigest: row.SetDigest, VerifierVersion: row.VerifierVersion, Passed: row.Passed, Report: report, Actor: Actor{row.Operator, row.Source}, CreatedAt: row.CreatedAt}, nil
}

func validFixtureResult(r crawlrule.FixtureResult) bool {
	if r.Kind != "" && r.Kind != "positive" && r.Kind != "empty" && r.Kind != "reject" {
		return false
	}
	if r.ErrorKind != "" && r.ErrorKind != "execution" && r.ErrorKind != "http_status" && r.ErrorKind != "no_records" && r.ErrorKind != "validation" {
		return false
	}
	if r.ErrorKind == "http_status" {
		if r.StatusCode < 100 || r.StatusCode > 599 || (r.StatusCode >= 200 && r.StatusCode < 300) || r.Rows != 0 || r.InvalidRows != 0 {
			return false
		}
	} else if r.StatusCode != 0 {
		return false
	}
	if !r.Passed {
		return true
	}
	switch r.Kind {
	case "", "positive":
		return r.Rows > 0 && r.InvalidRows == 0 && r.ErrorKind == ""
	case "empty":
		return r.Rows == 0 && r.InvalidRows == 0 && !r.Truncated && r.ErrorKind == "no_records"
	case "reject":
		if r.Truncated {
			return false
		}
		switch r.ErrorKind {
		case "http_status":
			return r.StatusCode >= 400
		case "no_records":
			return r.InvalidRows == 0
		case "validation":
			return r.InvalidRows > 0 && r.InvalidRows <= r.Rows
		}
	}
	return false
}

// validateVerificationSet binds a structurally valid report to the exact
// ordered fixture set it claims to verify. A self-consistent passing report
// must not authorize publication when fixture results are missing or changed.
func validateVerificationSet(report crawlrule.FixtureReport, set setEncoding) error {
	if len(report.Results) != len(set.Fixtures) || !hasPositiveFixtures(set) {
		return ErrCorrupt
	}
	for i, result := range report.Results {
		fixture := set.Fixtures[i]
		if result.Name != fixture.Name || result.Kind != fixture.Expect.Kind {
			return ErrCorrupt
		}
		if result.Passed {
			switch fixture.Expect.Kind {
			case "", "positive":
				if result.Rows != fixture.Expect.RowCount || result.Truncated != fixture.Expect.Truncated || result.InvalidRows != 0 || result.ErrorKind != "" || result.StatusCode != 0 {
					return ErrCorrupt
				}
			case "empty":
				if result.Rows != 0 || result.InvalidRows != 0 || result.Truncated || result.ErrorKind != "no_records" || result.StatusCode != 0 {
					return ErrCorrupt
				}
			case "reject":
				if result.ErrorKind != fixture.Expect.ErrorKind || result.StatusCode != fixture.Expect.StatusCode || result.Truncated {
					return ErrCorrupt
				}
			default:
				return ErrCorrupt
			}
		}
	}
	return nil
}

func (r *Repository) Verification(ctx context.Context, id string) (Verification, error) {
	if err := validateText("verification ID", id, 64); err != nil {
		return Verification{}, err
	}
	var row models.RuleVerification
	if err := r.db.WithContext(ctx).Where("id = ?", id).Take(&row).Error; err != nil {
		return Verification{}, dbError(err)
	}
	return publicVerification(row)
}

func (r *Repository) Verifications(ctx context.Context, ref Reference, limit int) ([]Verification, error) {
	if err := validateReference(ref); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > MaxQueryLimit {
		return nil, fmt.Errorf("rulestore: limit must be 1..%d", MaxQueryLimit)
	}
	var rows []models.RuleVerification
	if err := versionScope(r.db.WithContext(ctx), ref).Order("created_at DESC, id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]Verification, 0, len(rows))
	for _, row := range rows {
		v, err := publicVerification(row)
		if err != nil {
			return nil, err
		}
		result = append(result, v)
	}
	return result, nil
}

// VerifyHistory is the bounded, newest-first audit history for one rule version.
func (r *Repository) VerifyHistory(ctx context.Context, ref Reference, limit int) ([]Verification, error) {
	return r.Verifications(ctx, ref, limit)
}
