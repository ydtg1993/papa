package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"time"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/browser/identity"
	"gorm.io/gorm"
)

type GORMStore struct{ db *gorm.DB }

func NewGORMStore(db *gorm.DB) *GORMStore { return &GORMStore{db: db} }
func (s *GORMStore) database(ctx context.Context) (*gorm.DB, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("identity database required")
	}
	return s.db.WithContext(ctx), nil
}
func duplicate(err error) bool {
	var e *mysqlDriver.MySQLError
	return errors.Is(err, gorm.ErrDuplicatedKey) || (errors.As(err, &e) && e.Number == 1062)
}
func decodeProfile(r models.BrowserProfile) (identity.Profile, error) {
	var p identity.Profile
	if len(r.ProfileJSON) > 64<<10 || json.Unmarshal([]byte(r.ProfileJSON), &p) != nil || p.Validate() != nil || p.Reference() != (identity.Reference{Site: r.Site, ID: r.ProfileID, Version: r.Version}) || int(p.SchemaVersion) != r.SchemaVersion {
		return p, ErrCorrupt
	}
	return p, nil
}
func decodeBinding(r models.BrowserIdentityBinding) (Binding, error) {
	var b Binding
	if len(r.StateJSON) > 16<<10 || json.Unmarshal([]byte(r.StateJSON), &b) != nil || b.Validate() != nil || b.Subject != (Subject{Site: r.Site, Kind: r.SubjectKind, Key: r.SubjectKey}) || b.Revision != r.Revision {
		return b, ErrCorrupt
	}
	return b, nil
}
func (s *GORMStore) GetProfile(ctx context.Context, ref identity.Reference) (identity.Profile, error) {
	db, err := s.database(ctx)
	if err != nil {
		return identity.Profile{}, err
	}
	var row models.BrowserProfile
	err = db.Where("site = ? AND profile_id = ? AND version = ?", ref.Site, ref.ID, ref.Version).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return identity.Profile{}, ErrNotFound
	}
	if err != nil {
		return identity.Profile{}, err
	}
	return decodeProfile(row)
}
func (s *GORMStore) ListProfiles(ctx context.Context, site string) ([]identity.Profile, error) {
	db, err := s.database(ctx)
	if err != nil {
		return nil, err
	}
	if site != "" {
		db = db.Where("site = ?", site)
	}
	var rows []models.BrowserProfile
	if err = db.Order("site, profile_id, version").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]identity.Profile, 0, len(rows))
	for _, row := range rows {
		p, err := decodeProfile(row)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}
func (s *GORMStore) AppendProfile(ctx context.Context, p identity.Profile, expected uint64) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if p.Version != expected+1 || p.Version == 0 {
		return ErrConflict
	}
	db, err := s.database(ctx)
	if err != nil {
		return err
	}
	var latest models.BrowserProfile
	err = db.Where("site = ? AND profile_id = ?", p.Site, p.ID).Order("version DESC").First(&latest).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if latest.Version != expected {
		return ErrConflict
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	row := models.BrowserProfile{Site: p.Site, ProfileID: p.ID, Version: p.Version, SchemaVersion: int(p.SchemaVersion), ProfileJSON: string(raw)}
	err = db.Create(&row).Error
	if duplicate(err) {
		return ErrConflict
	}
	if err != nil {
		readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		got, readErr := s.GetProfile(readCtx, p.Reference())
		if readErr == nil && reflect.DeepEqual(got, p) {
			return nil
		}
		return errors.Join(ErrUncertain, err)
	}
	return nil
}
func (s *GORMStore) GetBinding(ctx context.Context, k Subject) (Binding, error) {
	if err := k.Validate(); err != nil {
		return Binding{}, err
	}
	db, err := s.database(ctx)
	if err != nil {
		return Binding{}, err
	}
	var row models.BrowserIdentityBinding
	err = db.Where("site = ? AND subject_kind = ? AND subject_key = ?", k.Site, k.Kind, k.Key).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Binding{}, ErrNotFound
	}
	if err != nil {
		return Binding{}, err
	}
	return decodeBinding(row)
}
func (s *GORMStore) ListBindings(ctx context.Context, site string) ([]Binding, error) {
	db, err := s.database(ctx)
	if err != nil {
		return nil, err
	}
	if site != "" {
		db = db.Where("site = ?", site)
	}
	var rows []models.BrowserIdentityBinding
	if err = db.Order("site, subject_kind, subject_key").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]Binding, 0, len(rows))
	for _, row := range rows {
		b, err := decodeBinding(row)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, nil
}
func (s *GORMStore) SaveBindingCAS(ctx context.Context, b Binding, expected uint64) error {
	if err := b.Validate(); err != nil {
		return err
	}
	if b.Revision != expected+1 {
		return ErrConflict
	}
	db, err := s.database(ctx)
	if err != nil {
		return err
	}
	for _, r := range []*identity.Reference{b.Active, b.Pending} {
		if r != nil {
			if _, err = s.GetProfile(ctx, *r); err != nil {
				return err
			}
		}
	}
	raw, err := json.Marshal(b)
	if err != nil {
		return err
	}
	row := models.BrowserIdentityBinding{Site: b.Subject.Site, SubjectKind: b.Subject.Kind, SubjectKey: b.Subject.Key, Revision: b.Revision, StateJSON: string(raw)}
	if expected == 0 {
		err = db.Create(&row).Error
		if duplicate(err) {
			return ErrConflict
		}
	} else {
		r := db.Model(&models.BrowserIdentityBinding{}).Where("site = ? AND subject_kind = ? AND subject_key = ? AND revision = ?", row.Site, row.SubjectKind, row.SubjectKey, expected).Updates(map[string]any{"revision": row.Revision, "state_json": row.StateJSON, "updated_at": gorm.Expr("NOW()")})
		err = r.Error
		if err == nil && r.RowsAffected == 0 {
			return ErrConflict
		}
	}
	// A success acknowledgement can be lost after commit. Only an exact readback
	// proves publication; otherwise callers must leave the subject blocked.
	readCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer cancel()
	got, readErr := s.GetBinding(readCtx, b.Subject)
	if readErr == nil && reflect.DeepEqual(got, b) {
		return nil
	}
	if err != nil {
		return errors.Join(ErrUncertain, err)
	}
	if readErr != nil {
		return errors.Join(ErrUncertain, readErr)
	}
	return ErrConflict
}
