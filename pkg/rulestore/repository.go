package rulestore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	mysqldriver "github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/crawlrule"
	"gorm.io/gorm"
)

// Repository is safe for concurrent use. MySQL is authoritative: no active
// pointer or mutable verification state is cached. New never migrates tables.
// It requires a nontransactional connection so publication owns its commit.
type Repository struct {
	db    *gorm.DB
	blobs *FileBlobs
}

var _ crawlrule.Catalog = (*Repository)(nil)

func New(db *gorm.DB, blobs *FileBlobs) (*Repository, error) {
	if blobs == nil {
		return nil, errors.New("rulestore: MySQL database and sample blob directory required")
	}
	if err := validateDB(db); err != nil {
		return nil, err
	}
	return &Repository{db: db, blobs: blobs}, nil
}

func validateDB(db *gorm.DB) error {
	if db == nil || db.Dialector == nil || db.Dialector.Name() != "mysql" {
		return errors.New("rulestore: MySQL database required")
	}
	if db.DryRun {
		return errors.New("rulestore: dry-run database cannot persist rules")
	}
	if db.Statement != nil {
		if _, transactional := db.Statement.ConnPool.(gorm.TxCommitter); transactional {
			return errors.New("rulestore: caller-owned transaction cannot provide durable publication commits")
		}
	}
	return nil
}

func validateText(name, s string, max int) error {
	if s == "" || len(s) > max || strings.TrimSpace(s) != s || !utf8.ValidString(s) || strings.ContainsAny(s, "\x00\r\n") {
		return fmt.Errorf("rulestore: %s must be trimmed, valid text of 1..%d bytes", name, max)
	}
	return nil
}

func validateActor(a Actor) error {
	if err := validateText("operator", a.Operator, 128); err != nil {
		return err
	}
	return validateText("source", a.Source, 256)
}

func validateReference(ref Reference) error {
	if err := validateText("rule ID", ref.ID, 128); err != nil {
		return err
	}
	return validateText("version", ref.Version, 128)
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func encode(v any, max int) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("rulestore: encode: %w", err)
	}
	if len(data) > max {
		return nil, fmt.Errorf("rulestore: metadata exceeds %d bytes", max)
	}
	return data, nil
}

func decode(data string, target any, max int) error {
	if len(data) > max {
		return ErrCorrupt
	}
	d := json.NewDecoder(strings.NewReader(data))
	d.UseNumber()
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return fmt.Errorf("%w: invalid JSON", ErrCorrupt)
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("%w: trailing JSON", ErrCorrupt)
	}
	return nil
}

func dbError(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	var mysqlErr *mysqldriver.MySQLError
	if errors.Is(err, gorm.ErrDuplicatedKey) || (errors.As(err, &mysqlErr) && mysqlErr.Number == 1062) {
		return ErrConflict
	}
	return err
}

func versionScope(db *gorm.DB, ref Reference) *gorm.DB {
	return db.Where("rule_id = ? AND version = ?", ref.ID, ref.Version)
}

func compiledVersion(row models.RuleVersion) (*crawlrule.Compiled, error) {
	if digest([]byte(row.ContentJSON)) != row.Digest {
		return nil, ErrCorrupt
	}
	c, err := crawlrule.Parse([]byte(row.ContentJSON))
	if err != nil {
		return nil, fmt.Errorf("%w: invalid rule format", ErrCorrupt)
	}
	rule := c.Rule()
	if rule.ID != row.RuleID || rule.Version != row.Version {
		return nil, ErrCorrupt
	}
	normalized, err := encode(rule, crawlrule.MaxRuleBytes)
	if err != nil || !bytes.Equal(normalized, []byte(row.ContentJSON)) {
		return nil, ErrCorrupt
	}
	return c, nil
}

func publicVersion(row models.RuleVersion, c *crawlrule.Compiled) Version {
	return Version{Reference: Reference{ID: row.RuleID, Version: row.Version}, Rule: c.Rule(), Digest: row.Digest, State: row.State, EverPublished: row.EverPublished, Actor: Actor{row.Operator, row.Source}, CreatedAt: row.CreatedAt}
}

func (r *Repository) CreateDraft(ctx context.Context, rule crawlrule.Rule, actor Actor) (Version, error) {
	if err := validateActor(actor); err != nil {
		return Version{}, err
	}
	if err := validateReference(Reference{ID: rule.ID, Version: rule.Version}); err != nil {
		return Version{}, err
	}
	c, err := crawlrule.Compile(rule)
	if err != nil {
		return Version{}, err
	}
	data, err := encode(c.Rule(), crawlrule.MaxRuleBytes)
	if err != nil {
		return Version{}, err
	}
	row := models.RuleVersion{RuleID: rule.ID, Version: rule.Version, Digest: digest(data), ContentJSON: string(data), State: "draft", Operator: actor.Operator, Source: actor.Source}
	if err := r.db.WithContext(ctx).Create(&row).Error; err != nil {
		return Version{}, dbError(err)
	}
	return r.Get(ctx, Reference{ID: rule.ID, Version: rule.Version})
}

func (r *Repository) Get(ctx context.Context, ref Reference) (Version, error) {
	if err := validateReference(ref); err != nil {
		return Version{}, err
	}
	var row models.RuleVersion
	if err := versionScope(r.db.WithContext(ctx), ref).Take(&row).Error; err != nil {
		return Version{}, dbError(err)
	}
	c, err := compiledVersion(row)
	if err != nil {
		return Version{}, err
	}
	return publicVersion(row, c), nil
}

// Resolve deliberately ignores the current site pointer and the latest
// verification attempt. A pinned version remains runnable once published.
func (r *Repository) Resolve(ctx context.Context, id, version string) (*crawlrule.Compiled, error) {
	ref := Reference{ID: id, Version: version}
	if err := validateReference(ref); err != nil {
		return nil, err
	}
	var row models.RuleVersion
	if err := versionScope(r.db.WithContext(ctx), ref).Take(&row).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, errors.Join(ErrNotFound, crawlrule.ErrVersionNotFound)
		}
		return nil, err
	}
	if !row.EverPublished {
		return nil, ErrNotPublished
	}
	c, err := compiledVersion(row)
	if errors.Is(err, ErrCorrupt) {
		return nil, errors.Join(err, crawlrule.ErrInvalidVersion)
	}
	return c, err
}

func newID() string { return uuid.NewString() }
