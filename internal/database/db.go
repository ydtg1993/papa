package database

import (
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/ydtg1993/papa/v2/config"
	"gorm.io/driver/mysql"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// NewDB 创建数据库连接并配置连接池。
func NewDB(cfg *config.Config) (db *gorm.DB, err error) {
	// 根据驱动选择对应的 dialector
	var dialector gorm.Dialector
	switch cfg.DB.Driver {
	case "mysql":
		dialector = mysql.Open(cfg.DB.DSN)
	default:
		return nil, fmt.Errorf("unsupported driver: %s", cfg.DB.Driver)
	}

	gormConfig := &gorm.Config{Logger: logger.New(log.New(os.Stdout, "\r\n", log.LstdFlags), sqlLoggerConfig(cfg))}
	db, err = gorm.Open(dialector, gormConfig)
	if err != nil {
		return nil, fmt.Errorf("failed to connect database: %w", err)
	}

	// 获取底层的 sql.DB 进行连接池配置
	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("failed to get underlying sql.DB: %w", err)
	}

	// 设置连接池参数
	sqlDB.SetMaxIdleConns(cfg.DB.MaxIdleConns)
	sqlDB.SetMaxOpenConns(cfg.DB.MaxOpenConns)
	sqlDB.SetConnMaxLifetime(cfg.DB.ConnMaxLifetime)
	sqlDB.SetConnMaxIdleTime(cfg.DB.ConnMaxIdleTime)
	return
}

// sqlLoggerConfig 造 gorm 的 SQL 日志配置。抽出来是为了能离线断言这两条策略：
//
//   - **级别**：默认按环境推（dev = info，其它 = warn）。原来硬编码 info ——
//     每条 SQL 都进日志，而日志能从 OA 后台「日志导出」下载，等于把业务数据带出去。
//   - **参数值**：非 dev 隐掉（渲染成 `?`）。即便只打慢查询，那行 SQL 也带着参数值，
//     所以光降级别不够 —— 留着一行 `WHERE token_hash = 'abc...'` 一样是泄露。
//     调试要看具体值就用 dev。
//
// 慢查询阈值 200ms 是 gorm 的默认值，这里显式写出来是为了让"warn 到底会打什么"一目了然。
func sqlLoggerConfig(cfg *config.Config) logger.Config {
	return logger.Config{
		SlowThreshold:        200 * time.Millisecond,
		LogLevel:             parseSQLLogLevel(cfg.SQLLogLevel()),
		ParameterizedQueries: cfg.SQLHideParams(),
		Colorful:             true,
	}
}

// parseSQLLogLevel 把级别名转成 gorm 的枚举。名字合法性在 config.Load 已经校验过，
// 这里兜底成 warn（SQL 日志宁可少打）。
func parseSQLLogLevel(name string) logger.LogLevel {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case config.SQLLogSilent:
		return logger.Silent
	case config.SQLLogError:
		return logger.Error
	case config.SQLLogInfo:
		return logger.Info
	default:
		return logger.Warn
	}
}

func AutoMigrate(db *gorm.DB, models ...any) error {
	for _, model := range models {
		err := db.AutoMigrate(model)
		if err != nil {
			return err
		}
	}
	return nil
}
