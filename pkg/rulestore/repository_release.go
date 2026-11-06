package rulestore

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/ydtg1993/papa/v3/models"
	"github.com/ydtg1993/papa/v3/pkg/crawlrule"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type bindingsEncoding struct {
	SchemaVersion   int                  `json:"schema_version"`
	Bindings        map[string]Reference `json:"bindings"`
	VerificationIDs map[string]string    `json:"verification_ids"`
}

type operationRequest struct {
	Kind               string               `json:"kind"`
	Site               string               `json:"site"`
	ExpectedGeneration uint64               `json:"expected_generation"`
	Bindings           map[string]Reference `json:"bindings,omitempty"`
	Actor              Actor                `json:"actor"`
	ReviewedProofs     map[string]string    `json:"reviewed_proofs,omitempty"`
}

// PublishReviewed pins the latest verification identities the operator reviewed.
// A re-verification while confirmation is open must invalidate that review.
func (r *Repository) PublishReviewed(ctx context.Context, opID, site string, generation uint64, bindings map[string]Reference, proofs map[string]string, actor Actor) (Release, error) {
	if err := validateBindings(bindings); err != nil {
		return Release{}, err
	}
	if len(proofs) != len(bindings) {
		return Release{}, ErrReviewConflict
	}
	for stage := range bindings {
		if validateText("verification ID", proofs[stage], 64) != nil {
			return Release{}, ErrReviewConflict
		}
	}
	return r.changeRelease(ctx, opID, operationRequest{Kind: "publish", Site: site, ExpectedGeneration: generation, Bindings: bindings, Actor: actor, ReviewedProofs: proofs})
}

func validateBindings(bindings map[string]Reference) error {
	if len(bindings) == 0 || len(bindings) > MaxBindings {
		return fmt.Errorf("rulestore: publication requires 1..%d stages", MaxBindings)
	}
	for stage, ref := range bindings {
		if err := validateText("stage", stage, 128); err != nil {
			return err
		}
		if err := validateReference(ref); err != nil {
			return err
		}
	}
	return nil
}

func releaseFromRow(row models.RuleRelease) (Release, error) {
	var data bindingsEncoding
	if digest([]byte(row.BindingsJSON)) != row.BindingsDigest {
		return Release{}, ErrCorrupt
	}
	if err := decode(row.BindingsJSON, &data, MaxMetadataBytes); err != nil {
		return Release{}, err
	}
	if data.SchemaVersion != 1 || validateBindings(data.Bindings) != nil || row.Generation == 0 || (row.Kind != "publish" && row.Kind != "rollback") {
		return Release{}, ErrCorrupt
	}
	if len(data.VerificationIDs) != len(data.Bindings) {
		return Release{}, ErrCorrupt
	}
	for stage := range data.Bindings {
		if validateText("verification ID", data.VerificationIDs[stage], 64) != nil {
			return Release{}, ErrCorrupt
		}
	}
	return Release{ReleaseSnapshot: crawlrule.ReleaseSnapshot{ID: row.ID, Site: row.Site, Generation: row.Generation, Bindings: data.Bindings}, OperationID: row.OperationID, Kind: row.Kind, PreviousReleaseID: row.PreviousReleaseID, VerificationIDs: data.VerificationIDs, Actor: Actor{row.Operator, row.Source}, CreatedAt: row.CreatedAt}, nil
}

func operation(db *gorm.DB, id string) (models.RuleRelease, error) {
	var row models.RuleRelease
	err := db.Where("operation_id = ?", id).Take(&row).Error
	return row, err
}

func checkOperation(row models.RuleRelease, requestDigest string) (Release, error) {
	if row.RequestDigest != requestDigest {
		return Release{}, ErrOperationConflict
	}
	return releaseFromRow(row)
}

func (r *Repository) Publish(ctx context.Context, opID, site string, expectedGeneration uint64, bindings map[string]Reference, actor Actor) (Release, error) {
	if err := validateBindings(bindings); err != nil {
		return Release{}, err
	}
	return r.changeRelease(ctx, opID, operationRequest{Kind: "publish", Site: site, ExpectedGeneration: expectedGeneration, Bindings: bindings, Actor: actor})
}

// Rollback advances generation while consuming one predecessor in the active
// lineage. Audit rows are append-only; repeated rollback never toggles forward.
func (r *Repository) Rollback(ctx context.Context, opID, site string, expectedGeneration uint64, actor Actor) (Release, error) {
	return r.changeRelease(ctx, opID, operationRequest{Kind: "rollback", Site: site, ExpectedGeneration: expectedGeneration, Actor: actor})
}

func (r *Repository) changeRelease(ctx context.Context, opID string, request operationRequest) (Release, error) {
	if err := validateText("operation ID", opID, 128); err != nil {
		return Release{}, err
	}
	if err := validateText("site", request.Site, 128); err != nil {
		return Release{}, err
	}
	if err := validateActor(request.Actor); err != nil {
		return Release{}, err
	}
	if request.ExpectedGeneration == math.MaxUint64 {
		return Release{}, ErrGenerationConflict
	}
	data, err := encode(request, MaxMetadataBytes)
	if err != nil {
		return Release{}, err
	}
	requestDigest := digest(data)
	// Detach caller-owned maps before a transaction or lock wait.
	// Decoding into request itself would reuse and write to its existing map.
	var detached operationRequest
	if err := decode(string(data), &detached, MaxMetadataBytes); err != nil {
		return Release{}, err
	}
	request = detached
	if prior, err := operation(r.db.WithContext(ctx), opID); err == nil {
		return checkOperation(prior, requestDigest)
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return Release{}, err
	}
	var committedID string
	err = r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// The upsert serializes even the first generation, where no row existed.
		// It changes no established pointer and stays inside this transaction.
		if err := ensureSite(tx, request.Site).Error; err != nil {
			return err
		}
		var site models.RuleSite
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("site = ?", request.Site).Take(&site).Error; err != nil {
			return err
		}
		// Locking reads see a concurrent transaction's committed idempotency row
		// under MySQL REPEATABLE READ, unlike an earlier consistent snapshot.
		if prior, err := operation(tx.Clauses(clause.Locking{Strength: "UPDATE"}), opID); err == nil {
			if _, err := checkOperation(prior, requestDigest); err != nil {
				return err
			}
			committedID = prior.ID
			return nil
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if site.Generation != request.ExpectedGeneration {
			return ErrGenerationConflict
		}
		bindings := request.Bindings
		var verificationIDs map[string]string
		previous := site.CurrentReleaseID
		if request.Kind == "rollback" {
			if site.CurrentReleaseID == "" {
				return ErrNoRollback
			}
			var current models.RuleRelease
			if err := tx.Where("id = ?", site.CurrentReleaseID).Take(&current).Error; err != nil {
				return err
			}
			if current.Site != request.Site || current.Generation != site.Generation {
				return ErrCorrupt
			}
			if _, err := releaseFromRow(current); err != nil {
				return err
			}
			if current.PreviousReleaseID == "" {
				return ErrNoRollback
			}
			var target models.RuleRelease
			if err := tx.Where("id = ?", current.PreviousReleaseID).Take(&target).Error; err != nil {
				return err
			}
			if target.Site != request.Site || target.Generation >= current.Generation {
				return ErrCorrupt
			}
			release, err := releaseFromRow(target)
			if err != nil {
				return err
			}
			bindings = release.Bindings
			verificationIDs = release.VerificationIDs
			previous = target.PreviousReleaseID
		}
		verificationIDs, err = verifyBindings(tx, bindings, verificationIDs, request.ReviewedProofs)
		if err != nil {
			return err
		}
		encoded, err := encode(bindingsEncoding{SchemaVersion: 1, Bindings: bindings, VerificationIDs: verificationIDs}, MaxMetadataBytes)
		if err != nil {
			return err
		}
		row := models.RuleRelease{ID: newID(), OperationID: opID, RequestDigest: requestDigest, Site: request.Site, Generation: site.Generation + 1, BindingsJSON: string(encoded), BindingsDigest: digest(encoded), PreviousReleaseID: previous, Kind: request.Kind, Operator: request.Actor.Operator, Source: request.Actor.Source}
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		result := advanceSite(tx, request.Site, site.Generation, row.ID)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return ErrGenerationConflict
		}
		committedID = row.ID
		return nil
	})
	if err != nil {
		// A lost commit acknowledgement is not blindly retried as a new release.
		// Only a matching committed operation proves the operation succeeded.
		if prior, lookupErr := operation(r.db.WithContext(ctx), opID); lookupErr == nil {
			return checkOperation(prior, requestDigest)
		}
		return Release{}, dbError(err)
	}
	var row models.RuleRelease
	if err := r.db.WithContext(ctx).Where("id = ?", committedID).Take(&row).Error; err != nil {
		return Release{}, dbError(err)
	}
	return releaseFromRow(row)
}

func ensureSite(tx *gorm.DB, site string) *gorm.DB {
	return tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&models.RuleSite{Site: site})
}

func advanceSite(tx *gorm.DB, site string, generation uint64, releaseID string) *gorm.DB {
	return tx.Model(&models.RuleSite{}).Where("site = ? AND generation = ?", site, generation).
		Updates(map[string]any{"generation": gorm.Expr("`generation` + 1"), "current_release_id": releaseID})
}

func verifyBindings(tx *gorm.DB, bindings map[string]Reference, historicalProofs map[string]string, reviewedProofs map[string]string) (map[string]string, error) {
	// Acquire version locks in a deterministic order across different sites.
	refs := make([]Reference, 0, len(bindings))
	seen := map[Reference]bool{}
	proofByRef := map[Reference]string{}
	rollback := historicalProofs != nil
	if rollback {
		for stage, ref := range bindings {
			id := historicalProofs[stage]
			if id == "" || (proofByRef[ref] != "" && proofByRef[ref] != id) {
				return nil, ErrCorrupt
			}
			proofByRef[ref] = id
		}
	}
	for _, ref := range bindings {
		if !seen[ref] {
			refs = append(refs, ref)
			seen[ref] = true
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].ID == refs[j].ID {
			return refs[i].Version < refs[j].Version
		}
		return refs[i].ID < refs[j].ID
	})
	for _, ref := range refs {
		var version models.RuleVersion
		if err := versionScope(tx.Clauses(clause.Locking{Strength: "UPDATE"}), ref).Take(&version).Error; err != nil {
			return nil, err
		}
		if _, err := compiledVersion(version); err != nil {
			return nil, err
		}
		if rollback {
			if !version.EverPublished {
				return nil, ErrNotPublished
			}
		} else {
			if reviewedProofs != nil {
				for stage, bound := range bindings {
					if bound == ref && reviewedProofs[stage] != version.VerificationID {
						return nil, ErrReviewConflict
					}
				}
			}
			proofByRef[ref] = version.VerificationID
		}
		if proofByRef[ref] == "" {
			return nil, ErrUnverified
		}
		var proof models.RuleVerification
		// Version locks are current reads. Their proof and set must use current
		// reads too: a preceding stage may already have established a MySQL
		// REPEATABLE READ snapshot before this version's latest Verify committed.
		if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).Where("id = ?", proofByRef[ref]).Take(&proof).Error; err != nil {
			return nil, err
		}
		if !proof.Passed || proof.RuleID != ref.ID || proof.Version != ref.Version || proof.RuleDigest != version.Digest || proof.VerifierVersion != VerifierVersion {
			return nil, ErrUnverified
		}
		verification, err := publicVerification(proof)
		if err != nil {
			return nil, err
		}
		var set models.RuleSampleSet
		if err := tx.Clauses(clause.Locking{Strength: "SHARE"}).Where("set_id = ?", proof.SetID).Take(&set).Error; err != nil {
			return nil, err
		}
		fixtures, err := decodeSet(set)
		if err != nil {
			return nil, err
		}
		if set.Digest != proof.SetDigest {
			return nil, ErrUnverified
		}
		if err := validateVerificationSet(verification.Report, fixtures); err != nil {
			return nil, err
		}
		if !rollback {
			if err := versionScope(tx.Model(&models.RuleVersion{}), ref).Updates(map[string]any{"state": "published", "ever_published": true}).Error; err != nil {
				return nil, err
			}
		}
	}
	proofs := make(map[string]string, len(bindings))
	for stage, ref := range bindings {
		proofs[stage] = proofByRef[ref]
	}
	return proofs, nil
}

// Current uses a single join so a concurrent publication cannot split the
// generation and release pointer across separate reads.
func (r *Repository) Current(ctx context.Context, site string) (Release, error) {
	if err := validateText("site", site, 128); err != nil {
		return Release{}, err
	}
	var row models.RuleRelease
	err := r.db.WithContext(ctx).Table("crawler_rule_releases AS release_row").Select("release_row.*").
		Joins("JOIN crawler_rule_sites AS site_row ON site_row.current_release_id = release_row.id AND site_row.generation = release_row.generation AND site_row.site = release_row.site").
		Where("site_row.site = ?", site).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Release{}, ErrNoRelease
	}
	if err != nil {
		return Release{}, err
	}
	return releaseFromRow(row)
}

func (r *Repository) Snapshot(ctx context.Context, site string) (crawlrule.ReleaseSnapshot, error) {
	release, err := r.Current(ctx, site)
	if err != nil {
		return crawlrule.ReleaseSnapshot{}, err
	}
	return release.ReleaseSnapshot, nil
}

func (r *Repository) LookupOperation(ctx context.Context, opID string) (Release, error) {
	if err := validateText("operation ID", opID, 128); err != nil {
		return Release{}, err
	}
	row, err := operation(r.db.WithContext(ctx), opID)
	if err != nil {
		return Release{}, dbError(err)
	}
	return releaseFromRow(row)
}

func (r *Repository) History(ctx context.Context, site string, limit int) ([]Release, error) {
	if err := validateText("site", site, 128); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > MaxQueryLimit {
		return nil, fmt.Errorf("rulestore: limit must be 1..%d", MaxQueryLimit)
	}
	var rows []models.RuleRelease
	if err := r.db.WithContext(ctx).Where("site = ?", site).Order("generation DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	result := make([]Release, 0, len(rows))
	for _, row := range rows {
		release, err := releaseFromRow(row)
		if err != nil {
			return nil, err
		}
		result = append(result, release)
	}
	return result, nil
}
