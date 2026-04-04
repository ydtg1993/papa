package workerpool

import (
	"context"
	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/internal/models"
	"gorm.io/gorm"
)

// Tasker 任务接口 方法
type Tasker interface {
	GetUrl() string
	GetRetry() int
	IncRetry(db *gorm.DB, logger *logrus.Logger)
	Insert(db *gorm.DB, logger *logrus.Logger) bool
	UpdateStatus(db *gorm.DB, logger *logrus.Logger, status models.TaskStatus, err error) bool
	Unique() string
}

// TaskHandler 处理单个任务
type TaskHandler[T Tasker] func(ctx context.Context, task T) error
