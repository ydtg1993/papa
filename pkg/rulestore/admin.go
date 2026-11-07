package rulestore

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strconv"
	"time"

	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/crawlrule"
	"gorm.io/gorm"
)

var ErrAdminInput = errors.New("rulestore: invalid administration input")
var ErrReviewConflict = errors.New("rulestore: reviewed verification changed")

const AdminPageSize = 20

func sortedStages(bindings map[string]Reference) []string {
	keys := make([]string, 0, len(bindings))
	for key := range bindings {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// Admin exposes metadata-only operations. It owns no blob directory, never
// migrates, and cannot import samples or evaluate their bodies.
type Admin struct{ repo *Repository }

func NewAdmin(db *gorm.DB) (*Admin, error) {
	if err := validateDB(db); err != nil {
		return nil, err
	}
	return &Admin{repo: &Repository{db: db}}, nil
}

type VersionSummary struct {
	Reference
	Digest        string    `json:"digest"`
	State         string    `json:"state"`
	EverPublished bool      `json:"ever_published"`
	CreatedAt     time.Time `json:"created_at"`
}
type VersionList struct {
	Items []VersionSummary `json:"items"`
	Next  *Reference       `json:"next"`
}
type VersionDetail struct {
	Version      Version        `json:"version"`
	Verification *Verification  `json:"verification"`
	History      []Verification `json:"history,omitempty"`
}
type ReleaseView struct {
	ID                string               `json:"id"`
	Site              string               `json:"site"`
	Generation        string               `json:"generation"`
	Kind              string               `json:"kind"`
	PreviousReleaseID string               `json:"previous_release_id"`
	OperationID       string               `json:"operation_id"`
	Bindings          map[string]Reference `json:"bindings"`
	VerificationIDs   map[string]string    `json:"verification_ids"`
	Actor             Actor                `json:"actor"`
	CreatedAt         time.Time            `json:"created_at"`
}

func ViewRelease(r Release) ReleaseView {
	return ReleaseView{r.ID, r.Site, strconv.FormatUint(r.Generation, 10), r.Kind, r.PreviousReleaseID, r.OperationID, r.Bindings, r.VerificationIDs, r.Actor, r.CreatedAt}
}

type ReleasePage struct {
	Current *ReleaseView  `json:"current"`
	Items   []ReleaseView `json:"items"`
	Next    string        `json:"next"`
}
type RulePlan struct {
	Kind       string               `json:"kind"`
	Site       string               `json:"site"`
	Generation string               `json:"generation"`
	Bindings   map[string]Reference `json:"bindings,omitempty"`
}
type RuleChangeRequest struct {
	RulePlan
	OperationID     string            `json:"operation_id"`
	VerificationIDs map[string]string `json:"verification_ids,omitempty"`
}
type StageReview struct {
	Stage string `json:"stage"`
	VersionDetail
	Comparison *RuleComparison `json:"comparison,omitempty"`
}
type RulePreview struct {
	Plan            RulePlan          `json:"plan"`
	Current         *ReleaseView      `json:"current"`
	Target          *ReleaseView      `json:"target,omitempty"`
	Stages          []StageReview     `json:"stages"`
	VerificationIDs map[string]string `json:"verification_ids"`
}

func (p RulePlan) Validate() (uint64, error) {
	n, err := strconv.ParseUint(p.Generation, 10, 64)
	if err != nil || strconv.FormatUint(n, 10) != p.Generation || validateText("site", p.Site, 128) != nil {
		return 0, ErrAdminInput
	}
	if p.Kind == "publish" {
		if validateBindings(p.Bindings) != nil {
			return 0, ErrAdminInput
		}
	} else if p.Kind != "rollback" || len(p.Bindings) != 0 {
		return 0, ErrAdminInput
	}
	return n, nil
}
func (p RuleChangeRequest) Validate() error {
	if _, err := p.RulePlan.Validate(); err != nil {
		return err
	}
	if validateText("operation ID", p.OperationID, 128) != nil {
		return ErrAdminInput
	}
	if p.Kind == "rollback" {
		if len(p.VerificationIDs) != 0 {
			return ErrAdminInput
		}
		return nil
	}
	if len(p.VerificationIDs) != len(p.Bindings) {
		return ErrAdminInput
	}
	for stage := range p.Bindings {
		if validateText("verification ID", p.VerificationIDs[stage], 64) != nil {
			return ErrAdminInput
		}
	}
	return nil
}

func versionListQuery(db *gorm.DB, id string, after Reference) *gorm.DB {
	q := db.Model(&models.RuleVersion{}).Select("rule_id, version, digest, state, ever_published, created_at")
	if id != "" {
		q = q.Where("rule_id = ?", id)
	}
	if after.ID != "" {
		q = q.Where("rule_id > ? OR (rule_id = ? AND version > ?)", after.ID, after.ID, after.Version)
	}
	return q.Order("rule_id ASC, version ASC").Limit(AdminPageSize + 1)
}
func (a *Admin) Versions(ctx context.Context, id string, after Reference) (VersionList, error) {
	out := VersionList{Items: []VersionSummary{}}
	if (id != "" && validateText("rule ID", id, 128) != nil) || ((after.ID != "" || after.Version != "") && validateReference(after) != nil) {
		return out, ErrAdminInput
	}
	var rows []models.RuleVersion
	if err := versionListQuery(a.repo.db.WithContext(ctx), id, after).Find(&rows).Error; err != nil {
		return out, err
	}
	if len(rows) > AdminPageSize {
		rows = rows[:AdminPageSize]
		last := rows[len(rows)-1]
		out.Next = &Reference{ID: last.RuleID, Version: last.Version}
	}
	for _, r := range rows {
		out.Items = append(out.Items, VersionSummary{Reference{ID: r.RuleID, Version: r.Version}, r.Digest, r.State, r.EverPublished, r.CreatedAt})
	}
	return out, nil
}

func (a *Admin) read(ctx context.Context, fn func(*Repository) error) error {
	return a.repo.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error { return fn(&Repository{db: tx}) }, &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true})
}
func currentForAdmin(r *Repository, ctx context.Context, site string) (*Release, error) {
	var row models.RuleSite
	err := r.db.WithContext(ctx).Where("site = ?", site).Take(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if row.Generation == 0 && row.CurrentReleaseID == "" {
		return nil, nil
	}
	release, err := r.Current(ctx, site)
	if errors.Is(err, ErrNoRelease) {
		return nil, ErrCorrupt
	}
	if err != nil {
		return nil, err
	}
	return &release, nil
}
func (a *Admin) Releases(ctx context.Context, site string, before uint64) (ReleasePage, error) {
	out := ReleasePage{Items: []ReleaseView{}, Next: "0"}
	if validateText("site", site, 128) != nil {
		return out, ErrAdminInput
	}
	err := a.read(ctx, func(r *Repository) error {
		current, err := currentForAdmin(r, ctx, site)
		if err != nil {
			return err
		}
		if current != nil {
			v := ViewRelease(*current)
			out.Current = &v
		}
		q := r.db.Where("site = ?", site)
		if before > 0 {
			q = q.Where("generation < ?", before)
		}
		var rows []models.RuleRelease
		if err := q.Order("generation DESC").Limit(AdminPageSize + 1).Find(&rows).Error; err != nil {
			return err
		}
		if len(rows) > AdminPageSize {
			rows = rows[:AdminPageSize]
			out.Next = strconv.FormatUint(rows[len(rows)-1].Generation, 10)
		}
		for _, row := range rows {
			release, err := releaseFromRow(row)
			if err != nil {
				return err
			}
			out.Items = append(out.Items, ViewRelease(release))
		}
		return nil
	})
	return out, err
}
func inspectVersion(r *Repository, ctx context.Context, ref Reference) (VersionDetail, error) {
	var out VersionDetail
	if validateReference(ref) != nil {
		return out, ErrAdminInput
	}
	v, err := r.Get(ctx, ref)
	if err != nil {
		return out, err
	}
	out.Version = v
	var row models.RuleVersion
	if err := versionScope(r.db.WithContext(ctx), ref).Take(&row).Error; err != nil {
		return out, err
	}
	if row.VerificationID != "" {
		proof, err := r.Verification(ctx, row.VerificationID)
		if err != nil {
			return out, err
		}
		if proof.Reference != ref || proof.RuleDigest != v.Digest {
			return out, ErrCorrupt
		}
		out.Verification = &proof
	}
	return out, nil
}
func (a *Admin) Inspect(ctx context.Context, ref Reference) (VersionDetail, error) {
	var out VersionDetail
	err := a.read(ctx, func(r *Repository) error {
		var err error
		out, err = inspectVersion(r, ctx, ref)
		if err == nil {
			out.History, err = r.Verifications(ctx, ref, 5)
		}
		return err
	})
	return out, err
}
func (a *Admin) Verification(ctx context.Context, id string) (Verification, error) {
	if validateText("verification ID", id, 64) != nil {
		return Verification{}, ErrAdminInput
	}
	return a.repo.Verification(ctx, id)
}

func (a *Admin) Preview(ctx context.Context, p RulePlan) (RulePreview, error) {
	out := RulePreview{Plan: p, Stages: []StageReview{}, VerificationIDs: map[string]string{}}
	generation, err := p.Validate()
	if err != nil {
		return out, err
	}
	err = a.read(ctx, func(r *Repository) error {
		current, err := currentForAdmin(r, ctx, p.Site)
		if err != nil {
			return err
		}
		actual := uint64(0)
		if current != nil {
			actual = current.Generation
			v := ViewRelease(*current)
			out.Current = &v
		}
		if actual != generation {
			return ErrGenerationConflict
		}
		bindings := p.Bindings
		if p.Kind == "rollback" {
			if current == nil || current.PreviousReleaseID == "" {
				return ErrNoRollback
			}
			var row models.RuleRelease
			if err := r.db.Where("id = ?", current.PreviousReleaseID).Take(&row).Error; err != nil {
				return err
			}
			target, err := releaseFromRow(row)
			if err != nil {
				return err
			}
			if target.Site != p.Site || target.Generation >= actual {
				return ErrCorrupt
			}
			v := ViewRelease(target)
			out.Target = &v
			bindings = target.Bindings
			out.VerificationIDs = target.VerificationIDs
		}
		for _, stage := range sortedStages(bindings) {
			ref := bindings[stage]
			detail, err := inspectVersion(r, ctx, ref)
			if err != nil {
				return err
			}
			if p.Kind == "rollback" {
				if !detail.Version.EverPublished {
					return ErrNotPublished
				}
				proof, err := r.Verification(ctx, out.VerificationIDs[stage])
				if err != nil {
					return err
				}
				detail.Verification = &proof
			}
			proof := detail.Verification
			if proof == nil || !proof.Passed || proof.Reference != ref || proof.RuleDigest != detail.Version.Digest || !supportedVerifierVersion(proof.VerifierVersion) {
				return ErrUnverified
			}
			var setRow models.RuleSampleSet
			if err := r.db.Where("set_id = ?", proof.SetID).Take(&setRow).Error; err != nil {
				return err
			}
			set, err := decodeSet(setRow)
			if err != nil {
				return err
			}
			if setRow.Digest != proof.SetDigest {
				return ErrCorrupt
			}
			if err := validateVerificationSet(proof.Report, set); err != nil {
				return err
			}
			item := StageReview{Stage: stage, VersionDetail: detail}
			out.VerificationIDs[stage] = proof.ID
			if current != nil {
				if old, ok := current.Bindings[stage]; ok {
					oldV, err := r.Get(ctx, old)
					if err != nil {
						return err
					}
					left, err := crawlrule.Compile(oldV.Rule)
					if err != nil {
						return err
					}
					right, err := crawlrule.Compile(detail.Version.Rule)
					if err != nil {
						return err
					}
					diff, err := CompareRules(left, right)
					if err != nil {
						return err
					}
					item.Comparison = &diff
				}
			}
			out.Stages = append(out.Stages, item)
		}
		return nil
	})
	return out, err
}
func (a *Admin) Change(ctx context.Context, p RuleChangeRequest, actor Actor) (ReleaseView, error) {
	if err := p.Validate(); err != nil {
		return ReleaseView{}, err
	}
	generation, _ := p.RulePlan.Validate()
	var release Release
	var err error
	if p.Kind == "publish" {
		release, err = a.repo.PublishReviewed(ctx, p.OperationID, p.Site, generation, p.Bindings, p.VerificationIDs, actor)
	} else {
		release, err = a.repo.Rollback(ctx, p.OperationID, p.Site, generation, actor)
	}
	return ViewRelease(release), err
}
func (a *Admin) Operation(ctx context.Context, id string) (ReleaseView, error) {
	if validateText("operation ID", id, 128) != nil {
		return ReleaseView{}, ErrAdminInput
	}
	r, err := a.repo.LookupOperation(ctx, id)
	return ViewRelease(r), err
}
