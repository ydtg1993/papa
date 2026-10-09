package database

import (
	"reflect"
	"testing"

	"github.com/ydtg1993/papa/v3/config"
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

// 框架自有表的清单：两张总是建，另两张跟着开关走。
// 这份清单同时喂给 App.Migrate()（脚手架的 `make migrate`）和 CLI 的 `papa migrate` —— 它错了两边一起错。
func TestFrameworkModelsFollowsSwitches(t *testing.T) {
	names := func(cfg *config.Config) []string {
		var out []string
		for _, m := range FrameworkModels(cfg) {
			out = append(out, m.Name)
		}
		return out
	}

	c := &config.Config{}
	if got := names(c); !reflect.DeepEqual(got, []string{"crawler_tasks", "crawler_access_token"}) {
		t.Fatalf("开关都关着时的清单 = %v", got)
	}

	c.Server.OperationLog = true
	if got := names(c); !reflect.DeepEqual(got, []string{
		"crawler_tasks", "crawler_access_token", "crawler_operation_log",
	}) {
		t.Fatalf("开审计后的清单 = %v", got)
	}

	c.Crawler.Trace.Enabled = true
	if got := names(c); !reflect.DeepEqual(got, []string{
		"crawler_tasks", "crawler_access_token", "crawler_operation_log", "crawler_task_trace",
	}) {
		t.Fatalf("两个开关都开后的清单 = %v", got)
	}

	// 清单里的名字是给人看的提示（`papa migrate` 会打印它），必须和模型的实际表名一致；
	// CrawlerTask 例外 —— 它没实现 TableName，表名靠复数化规则推成 crawler_tasks。
	for _, m := range FrameworkModels(c) {
		if m.Name == "crawler_tasks" {
			continue
		}
		tn, ok := m.Value.(interface{ TableName() string })
		if !ok {
			t.Fatalf("%s：模型没实现 TableName，清单里的名字就成了唯一来源，容易写飘", m.Name)
		}
		if tn.TableName() != m.Name {
			t.Fatalf("表名对不上：清单写 %s，模型的实际表名是 %s", m.Name, tn.TableName())
		}
	}
}
