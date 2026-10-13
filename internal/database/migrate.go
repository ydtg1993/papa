package database

import (
	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/gorm"
)

// Model 一个要建的表：Name 只用于给人看的提示 —— gorm 给不出 CrawlerTask 的表名
// （它没实现 TableName，表名靠复数化规则推成 crawler_tasks），所以名字得显式带上；
// Value 交给 gorm 的 AutoMigrate。
type Model struct {
	Name  string
	Value any
}

// FrameworkModels 返回框架自带、需要建表的模型。两张带开关的按配置决定要不要包含 ——
// 开关关着就不建那张表（`server.operation_log` / `crawler.trace.enabled`）。
//
// 抽成纯函数有两个用处：让"哪个开关带出哪张表"能离线断言；以及让启动时的 dev 自动迁移
// 和 `papa migrate` 吃**同一份清单**（原来那份只写在 internal/app 里，命令行拿不到）。
func FrameworkModels(cfg *config.Config) []Model {
	out := []Model{
		{Name: "crawler_tasks", Value: &models.CrawlerTask{}},
		{Name: "crawler_access_token", Value: &models.AccessToken{}},
		{Name: "crawler_sites", Value: &models.CrawlerSite{}},
		// 运行期配置（来源 IP 白名单等）。**无条件登记**：它是后台设置页的落点，
		// 而且"表在不在"不该取决于某个开关 —— 时有时无会让排查变成猜谜。
		{Name: "crawler_config", Value: &models.Config{}},
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
// AutoMigrate **只增不减**：加表、加列、加索引，不删列也不改类型，重复跑是幂等的 ——
// 所以生产上跑它是安全的。真正的破坏性变更（改名、改类型）gorm 不会替你猜，那得手工迁移。
//
// **这里不再有"给旧库补课"的显式迁移**：框架不为更早版本的表结构负责 ——
// 删旧索引、url 改 text、回填 url_hash 与 idempotency_hash、删死列（title/content、按站点的队列统计列）、
// 删「默认 scope」那行，这些一步都删掉了（它们只对旧库有意义，而旧库不在支持范围内）。
// 从老库升上来的库要么手工对齐表结构，要么重建。
func Migrate(db *gorm.DB, cfg *config.Config, extra ...any) error {
	all := make([]any, 0, 8)
	for _, m := range FrameworkModels(cfg) {
		all = append(all, m.Value)
	}
	return AutoMigrate(db, append(all, extra...)...)
}
