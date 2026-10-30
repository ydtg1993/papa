package analyze

import (
	"context"
	"time"

	"github.com/ydtg1993/papa/v3/models"
	"github.com/ydtg1993/papa/v3/obs"
	"gorm.io/gorm"
)

// TaskBucket 是 crawler_tasks 按 site×stage×status×repeatable×age 的聚合行。
type TaskBucket struct {
	Site       string
	Stage      string
	Status     int
	Repeatable int
	AgeBucket  string // lt10m / 10m-1h / 1h-6h / gt6h
	N          int64
}

// Querier 是报告段的只读数据源；抽成接口是为了让分析段可用假数据单测。
type Querier interface {
	SiteStates(ctx context.Context) ([]obs.SiteState, error)
	TaskBuckets(ctx context.Context, now time.Time) ([]TaskBucket, error)
	OldProcessing(ctx context.Context, before time.Time) (int64, error)
}

// NewGormQuerier 用现有业务库构造只读数据源。
func NewGormQuerier(db *gorm.DB) Querier { return &gormQuerier{db: db} }

type gormQuerier struct{ db *gorm.DB }

func (q *gormQuerier) SiteStates(ctx context.Context) ([]obs.SiteState, error) {
	var row models.Config
	if err := q.db.WithContext(ctx).Where("cfg_key = ?", "sites").Take(&row).Error; err != nil {
		return nil, err
	}
	return parseSiteStates(row.CfgVal)
}

func (q *gormQuerier) TaskBuckets(ctx context.Context, now time.Time) ([]TaskBucket, error) {
	var out []TaskBucket
	err := taskBucketsQuery(q.db, now).WithContext(ctx).Find(&out).Error
	return out, err
}

func (q *gormQuerier) OldProcessing(ctx context.Context, before time.Time) (int64, error) {
	var n int64
	err := q.db.WithContext(ctx).Model(&models.CrawlerTask{}).
		Where("status = ? AND updated_at < ?", models.TaskStatusProcessing, before).
		Count(&n).Error
	return n, err
}

// taskBucketsQuery 单独抽出来：单测用 DryRun 断言 SQL 形状（不连库）。
//
// 老化基准按「这个年龄在问什么」分两套（设计文档口径）：
//   - pending：还没被 worker 取走 → `created_at`，年龄 ≈ 排队等了多久；
//   - processing：已被取走 → `updated_at`，年龄 ≈ 距最后一次更新多久（孤儿/卡死会落 gt6h）。
//
// 用 updated_at 算 pending 会被 claim/重试反复覆盖，测出来的是「最近一次状态变化」而不是排队时长。
func taskBucketsQuery(db *gorm.DB, now time.Time) *gorm.DB {
	b10m := now.Add(-10 * time.Minute)
	b1h := now.Add(-time.Hour)
	b6h := now.Add(-6 * time.Hour)
	return db.Model(&models.CrawlerTask{}).
		Select(`site, stage, status, repeatable,
			CASE
				WHEN status = ? THEN CASE
					WHEN created_at >= ? THEN 'lt10m'
					WHEN created_at >= ? THEN '10m-1h'
					WHEN created_at >= ? THEN '1h-6h'
					ELSE 'gt6h'
				END
				ELSE CASE
					WHEN updated_at >= ? THEN 'lt10m'
					WHEN updated_at >= ? THEN '10m-1h'
					WHEN updated_at >= ? THEN '1h-6h'
					ELSE 'gt6h'
				END
			END AS age_bucket,
			COUNT(*) AS n`,
			models.TaskStatusPending, b10m, b1h, b6h, b10m, b1h, b6h).
		Where("status IN ?", []models.TaskStatus{models.TaskStatusPending, models.TaskStatusProcessing}).
		Group("site, stage, status, repeatable, age_bucket")
}
