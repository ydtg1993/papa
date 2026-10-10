package models

import "time"

// AccessToken 后台访问令牌：一条令牌属于一个操作人。
//
// 只存 sha256 哈希（唯一索引），不存明文 —— 库或备份泄漏也拿不到可用的令牌。
// 代价是创建后只能看一次（`papa token add` 打印那一次）。
type AccessToken struct {
	ID       uint   `gorm:"primarykey"`
	Operator string `gorm:"type:varchar(128);not null;index;comment:操作人（令牌归属）"`
	// TokenHash sha256(token) 的**十六进制（小写，定长 64 字符）**；唯一索引兼作校验路径。
	// 与 crawler_tasks.url_hash 同一套：定长列按真实长度写，别留一倍（索引键白白翻倍）。
	TokenHash string `gorm:"type:char(64);not null;uniqueIndex;comment:令牌哈希(sha256 hex)"`
	Enabled   bool   `gorm:"not null;default:true;index;comment:是否启用（停用而不删）"`
	Note      string `gorm:"type:varchar(255);comment:备注（哪台机器/哪个人）"`

	CreatedAt time.Time `gorm:"autoCreateTime"`
	UpdatedAt time.Time `gorm:"autoUpdateTime"`
}

// TableName 指定表名，与其它模型保持 crawler_ 前缀。
func (AccessToken) TableName() string { return "crawler_access_token" }
