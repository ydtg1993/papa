package database

import (
	"testing"

	"github.com/ydtg1993/papa/v2/config"
	"gorm.io/gorm/logger"
)

// SQL 日志的级别与"打不打参数值"都按环境推：dev 打 info 且带值，其它一律 warn 且隐掉参数。
//
// 回归点：原来硬编码 logger.Info —— 每条 SQL 连同参数值都进日志，而日志能从 OA 后台
// 「日志导出」下载，等于把业务数据带出去。
func TestSQLLoggerConfigFollowsEnv(t *testing.T) {
	dev := &config.Config{}
	dev.App.Env = "dev"
	cfg := sqlLoggerConfig(dev)
	if cfg.LogLevel != logger.Info {
		t.Fatalf("dev 应打 info（本地要调 SQL），实得 %v", cfg.LogLevel)
	}
	if cfg.ParameterizedQueries {
		t.Fatal("dev 应当带上参数值，方便调试")
	}

	prod := &config.Config{}
	prod.App.Env = "prod"
	cfg = sqlLoggerConfig(prod)
	if cfg.LogLevel != logger.Warn {
		t.Fatalf("非 dev 默认应降到 warn，实得 %v", cfg.LogLevel)
	}
	// 光降级别不够：慢查询那条仍然会打整行 SQL，里面带着参数值
	if !cfg.ParameterizedQueries {
		t.Fatal("非 dev 必须隐掉参数值（渲染成 ?），否则慢查询照样泄露")
	}
}

// 显式配了就听显式的。
func TestSQLLoggerConfigExplicitLevel(t *testing.T) {
	c := &config.Config{}
	c.App.Env = "prod"
	c.DB.LogLevel = config.SQLLogInfo
	if got := sqlLoggerConfig(c).LogLevel; got != logger.Info {
		t.Fatalf("显式配 info 应生效，实得 %v", got)
	}

	c.DB.LogLevel = config.SQLLogSilent
	if got := sqlLoggerConfig(c).LogLevel; got != logger.Silent {
		t.Fatalf("显式配 silent 应生效，实得 %v", got)
	}
}

// 名字合法性在 config.Load 校验；这里的兜底是"宁可少打"。
func TestParseSQLLogLevel(t *testing.T) {
	for name, want := range map[string]logger.LogLevel{
		"silent": logger.Silent, "error": logger.Error,
		"warn": logger.Warn, "info": logger.Info,
		"WARN": logger.Warn, "": logger.Warn, "乱写": logger.Warn,
	} {
		if got := parseSQLLogLevel(name); got != want {
			t.Errorf("parseSQLLogLevel(%q) = %v, want %v", name, got, want)
		}
	}
}
