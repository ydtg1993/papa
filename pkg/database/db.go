package database

import (
	"fmt"
	"github.com/ydtg1993/papa/internal/config"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var DB *gorm.DB

// NewDB 创建数据库连接并配置连接池
func NewDB(cfg *config.DBConfig) error {
	// 根据驱动选择对应的 dialector
	var dialector gorm.Dialector
	switch cfg.Driver {
	case "mysql":
		dialector = mysql.Open(cfg.DSN)
	default:
		return fmt.Errorf("unsupported driver: %s", cfg.Driver)
	}

	// 配置 GORM 日志级别（可根据环境调整）
	gormConfig := &gorm.Config{
		Logger: logger.Default.LogMode(logger.Info),
	}

	db, err := gorm.Open(dialector, gormConfig)
	if err != nil {
		return fmt.Errorf("failed to connect database: %w", err)
	}

	// 获取底层的 sql.DB 进行连接池配置
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("failed to get underlying sql.DB: %w", err)
	}

	// 设置连接池参数
	sqlDB.SetMaxIdleConns(cfg.MaxIdleConns)
	sqlDB.SetMaxOpenConns(cfg.MaxOpenConns)
	sqlDB.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	sqlDB.SetConnMaxIdleTime(cfg.ConnMaxIdleTime)

	// 自动迁移（仅开发环境可开启，生产应使用迁移工具）
	/*if err := db.AutoMigrate(&models.Page{}); err != nil {
		return fmt.Errorf("auto migrate failed: %w", err)
	}*/
	DB = db
	return nil
}
