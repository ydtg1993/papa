package models

import (
	"time"
)

// Page 页面数据模型
type Page struct {
	ID        uint      `gorm:"primarykey"`
	URL       string    `gorm:"type:varchar(500);not null;uniqueIndex"`
	Title     string    `gorm:"type:text"`
	Content   string    `gorm:"type:longtext"` // 可选，存储 HTML 内容
	CrawledAt time.Time `gorm:"index"`
	CreatedAt time.Time
	UpdatedAt time.Time
}
