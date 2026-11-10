package onboarding

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"sync"

	mysqlDriver "github.com/go-sql-driver/mysql"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"gorm.io/gorm"
)

// ProfileRecord is credential-free. Migrations are explicit; NewGORMStore
// never creates tables. SecretRef points to immutable encrypted generations.
type ProfileRecord struct {
	Site      string `gorm:"type:varchar(256) COLLATE utf8mb4_bin;primaryKey;comment:账号站点键"`
	AccountID string `gorm:"type:varchar(256) COLLATE utf8mb4_bin;primaryKey;comment:站点内稳定账号ID"`
	Revision  uint64 `gorm:"not null;comment:资料CAS版本"`
	StateJSON string `gorm:"type:longtext;not null;comment:脱敏资料及已核验身份，不含凭据"`
	SecretRef string `gorm:"type:varchar(64);not null;comment:加密登录态不可变代次引用"`
	InputsRef string `gorm:"type:varchar(64);not null;comment:自愿保存输入的加密引用"`
}

func (ProfileRecord) TableName() string { return "crawler_account_profiles" }
func Models() []any                     { return []any{&ProfileRecord{}} }

type GORMStore struct{ db *gorm.DB }

func NewGORMStore(db *gorm.DB) *GORMStore { return &GORMStore{db: db} }
func profileRecord(p Profile) (ProfileRecord, error) {
	b, err := json.Marshal(p)
	return ProfileRecord{Site: p.Key.Site, AccountID: p.Key.ID, Revision: p.Revision, StateJSON: string(b), SecretRef: p.SecretRef, InputsRef: p.InputsRef}, err
}
func decodeRecord(r ProfileRecord) (Profile, error) {
	var p Profile
	err := json.Unmarshal([]byte(r.StateJSON), &p)
	if err != nil || p.Key != (account.Key{Site: r.Site, ID: r.AccountID}) || p.Revision != r.Revision {
		return p, errors.New("invalid persisted account profile")
	}
	p.SecretRef, p.InputsRef = r.SecretRef, r.InputsRef
	return p, nil
}
func (s *GORMStore) List(ctx context.Context) ([]Profile, error) {
	if s == nil || s.db == nil {
		return nil, ErrNotConfigured
	}
	var rows []ProfileRecord
	if err := s.db.WithContext(ctx).Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]Profile, 0, len(rows))
	for _, r := range rows {
		p, err := decodeRecord(r)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}
func (s *GORMStore) Get(ctx context.Context, k account.Key) (Profile, error) {
	if s == nil || s.db == nil {
		return Profile{}, ErrNotConfigured
	}
	var r ProfileRecord
	err := s.db.WithContext(ctx).Where("site = ? AND account_id = ?", k.Site, k.ID).Take(&r).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return Profile{}, os.ErrNotExist
	}
	if err != nil {
		return Profile{}, err
	}
	return decodeRecord(r)
}
func (s *GORMStore) SaveCAS(ctx context.Context, p Profile, expected uint64) error {
	if s == nil || s.db == nil {
		return ErrNotConfigured
	}
	if p.Revision != expected+1 {
		return ErrConflict
	}
	r, err := profileRecord(p)
	if err != nil {
		return err
	}
	if expected == 0 {
		err = s.db.WithContext(ctx).Create(&r).Error
		var m *mysqlDriver.MySQLError
		if errors.Is(err, gorm.ErrDuplicatedKey) || (errors.As(err, &m) && m.Number == 1062) {
			return ErrConflict
		}
		if err != nil {
			return s.resolveWrite(ctx, r, err)
		}
		return nil
	}
	result := s.db.WithContext(ctx).Model(&ProfileRecord{}).Where("site = ? AND account_id = ? AND revision = ?", p.Key.Site, p.Key.ID, expected).Updates(map[string]any{"revision": r.Revision, "state_json": r.StateJSON, "secret_ref": r.SecretRef, "inputs_ref": r.InputsRef})
	if result.Error != nil {
		return s.resolveWrite(ctx, r, result.Error)
	}
	if result.RowsAffected != 1 {
		return ErrConflict
	}
	return nil
}

func (s *GORMStore) resolveWrite(ctx context.Context, want ProfileRecord, cause error) error {
	var got ProfileRecord
	if err := s.db.WithContext(ctx).Where("site = ? AND account_id = ?", want.Site, want.AccountID).Take(&got).Error; err != nil {
		return cause
	}
	if got.Revision == want.Revision && got.StateJSON == want.StateJSON && got.SecretRef == want.SecretRef && got.InputsRef == want.InputsRef {
		return nil
	}
	return cause
}

type MemoryStore struct {
	mu       sync.Mutex
	profiles map[account.Key]Profile
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{profiles: map[account.Key]Profile{}} }
func cloneProfile(p Profile) Profile {
	if p.Identity != nil {
		i := *p.Identity
		i.Scopes = append([]string(nil), i.Scopes...)
		p.Identity = &i
	}
	return p
}
func (s *MemoryStore) List(ctx context.Context) ([]Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]Profile, 0, len(s.profiles))
	for _, p := range s.profiles {
		out = append(out, cloneProfile(p))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Key.Site != out[j].Key.Site {
			return out[i].Key.Site < out[j].Key.Site
		}
		return out[i].Key.ID < out[j].Key.ID
	})
	return out, nil
}
func (s *MemoryStore) Get(ctx context.Context, k account.Key) (Profile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Profile{}, err
	}
	p, ok := s.profiles[k]
	if !ok {
		return p, os.ErrNotExist
	}
	return cloneProfile(p), nil
}
func (s *MemoryStore) SaveCAS(ctx context.Context, p Profile, expected uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.profiles[p.Key].Revision != expected || p.Revision != expected+1 {
		return ErrConflict
	}
	s.profiles[p.Key] = cloneProfile(p)
	return nil
}
