package database

import (
	"github.com/ydtg1993/papa/v4/config"
	"github.com/ydtg1993/papa/v4/models"
	"github.com/ydtg1993/papa/v4/pkg/account"
	"github.com/ydtg1993/papa/v4/pkg/account/onboarding"
	"gorm.io/gorm"
)

// Model 一个要建的表：Name 只用于给人看的提示 —— gorm 给不出 CrawlerTask 的表名
// （它没实现 TableName，表名靠复数化规则推成 crawler_tasks），所以名字得显式带上；
// Value 交给 gorm 的 AutoMigrate。
type Model struct {
	Name  string
	Value any
}

// AllFrameworkModels returns the framework-owned catalog, including disabled
// optional features. It is for inspection only; migration uses FrameworkModels.
func AllFrameworkModels() []Model {
	cfg := &config.Config{}
	cfg.TaskFlow.Enabled = true
	cfg.Server.OperationLog = true
	cfg.Crawler.Trace.Enabled = true
	return FrameworkModels(cfg)
}

// FrameworkModels 返回框架自带、需要建表的模型。带开关的按配置决定要不要包含 ——
// 开关关闭就不要求对应表（server.operation_log / crawler.trace.enabled / task_flow.enabled）。
//
// 抽成纯函数让显式迁移与普通启动的缺表检查吃同一份清单，也便于离线断言开关行为。
// 普通启动只检查，不会调用 AutoMigrate。
func FrameworkModels(cfg *config.Config) []Model {
	out := []Model{
		{Name: "crawler_tasks", Value: &models.CrawlerTask{}},
		{Name: "crawler_breaker_incidents", Value: &models.BreakerIncident{}},
		{Name: "crawler_breaker_history", Value: &models.BreakerHistory{}},
		{Name: "crawler_access_token", Value: &models.AccessToken{}},
		// 运行期配置（来源 IP 白名单、站点快照等）。**无条件登记**：它是后台设置页与站点页的落点，
		// 而且"表在不在"不该取决于某个开关 —— 时有时无会让排查变成猜谜。
		{Name: "crawler_config", Value: &models.Config{}},
		{Name: "crawler_runtime_policy_history", Value: &models.RuntimePolicyHistory{}},
		{Name: "crawler_browser_profiles", Value: &models.BrowserProfile{}},
		{Name: "crawler_browser_identity_bindings", Value: &models.BrowserIdentityBinding{}},
	}
	if cfg.TaskFlow.Enabled {
		out = append(out, Model{Name: "crawler_task_flows", Value: &models.TaskFlow{}})
	}
	for _, model := range account.Models() {
		out = append(out, Model{Name: model.(interface{ TableName() string }).TableName(), Value: model})
	}
	for _, model := range onboarding.Models() {
		out = append(out, Model{Name: model.(interface{ TableName() string }).TableName(), Value: model})
	}
	if cfg.Server.OperationLog {
		out = append(out, Model{Name: "crawler_operation_log", Value: &models.OperationLog{}})
	}
	if cfg.Crawler.Trace.Enabled {
		out = append(out, Model{Name: "crawler_task_trace", Value: &models.TaskTrace{}})
	}
	return out
}

// Migrate 建/补框架自带的表，外加业务传进来的 extra（业务自己的模型框架不认识 ——
// 业务得自己拿着模型跑 AutoMigrate，也就是项目自己的 `-migrate` 入口；CLI 那条
// `papa migrate` 在业务项目里会转交给它）。
//
// Explicit migration installs the standardized task identity index, backfills
// existing tasks and removes the two obsolete unique indexes only after success.
// Stop workers before migration. Conflicting canonical identities abort; no rows
// are merged or removed. This is a deliberate schema change, not startup work.
func Migrate(db *gorm.DB, cfg *config.Config, extra ...any) error {
	all := make([]any, 0, 8)
	for _, m := range FrameworkModels(cfg) {
		all = append(all, m.Value)
	}
	if err := AutoMigrate(db, append(all, extra...)...); err != nil {
		return err
	}
	return migrateTaskIdentities(db)
}
