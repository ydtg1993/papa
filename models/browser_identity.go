package models

import "time"

// BrowserProfile is an append-only declaration revision. Constructing identity
// stores never migrates this table; it belongs to the explicit framework migration.
type BrowserProfile struct {
	Site          string    `gorm:"type:varchar(256) COLLATE utf8mb4_bin;primaryKey;comment:站点键"`
	ProfileID     string    `gorm:"type:varchar(256) COLLATE utf8mb4_bin;primaryKey;comment:身份档案ID"`
	Version       uint64    `gorm:"primaryKey;autoIncrement:false;comment:不可变档案版本"`
	SchemaVersion int       `gorm:"not null;comment:档案结构版本"`
	ProfileJSON   string    `gorm:"type:longtext;not null;comment:身份声明JSON，不含凭据"`
	CreatedAt     time.Time `gorm:"autoCreateTime;comment:创建时间"`
}

func (BrowserProfile) TableName() string { return "crawler_browser_profiles" }

// BrowserIdentityBinding deliberately has its own CAS revision, independent of
// account invalidation and pool selection versions.
type BrowserIdentityBinding struct {
	Site        string    `gorm:"type:varchar(256) COLLATE utf8mb4_bin;primaryKey;comment:站点键"`
	SubjectKind string    `gorm:"type:varchar(16) COLLATE utf8mb4_bin;primaryKey;comment:account或session"`
	SubjectKey  string    `gorm:"type:varchar(256) COLLATE utf8mb4_bin;primaryKey;comment:账号ID或业务提供的稳定会话键"`
	Revision    uint64    `gorm:"not null;comment:绑定CAS版本，独立于账号管理版本"`
	StateJSON   string    `gorm:"type:longtext;not null;comment:活动与待生效引用及状态，不含凭据"`
	UpdatedAt   time.Time `gorm:"autoUpdateTime;comment:更新时间"`
}

func (BrowserIdentityBinding) TableName() string { return "crawler_browser_identity_bindings" }
