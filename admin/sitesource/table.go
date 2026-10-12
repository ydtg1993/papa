package sitesource

import (
	"github.com/ydtg1993/oao"
	"github.com/ydtg1993/papa/v3/admin/gormsource"
	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/gorm"
)

// Table 返回内置「站点」表格的声明。**只读**：站点行是引擎启动时按声明播种、熔断状态按列回写的，
// 运营没有可写动作（原来的「轮询任务 / 暂停·恢复自动轮询」随按站点队列一起移除了）。
func Table(db *gorm.DB) oao.Table {
	return oao.Table{
		Key: "site", Label: "站点", Group: "数据",
		// 站点行是引擎启动时按声明播种 + 跑起来按列回写的，所以这里**不给编辑**：
		// 配置在声明里（configs/sites/<站名>.go），运营没有可动的列。
		Source: gormsource.New(gormsource.Config{
			DB:     db,
			Model:  &models.CrawlerSite{},
			Search: []string{"key", "base_url"},
		}),
		// 主键用「站点键」而不是自增 ID：与熔断 scope 一脉相承
		IDField: "key",
		Columns: []oao.Column{
			{Field: "key", Label: "站点键", NoEdit: true},
			{Field: "base_url", Label: "BaseURL", Render: oao.RenderLink, Href: "{base_url}", NewTab: true, NoEdit: true},
			{Field: "stage_count", Label: "阶段数", Kind: oao.KindNumber, Width: "80px", NoEdit: true},
			{Field: "breaker_paused", Label: "熔断闸住", Kind: oao.KindBool, Width: "90px", NoEdit: true},
			{Field: "breaker_paused_at", Label: "闸住时间", Kind: oao.KindTime, Width: "150px", NoEdit: true},
			{Field: "updated_at", Label: "更新时间", Kind: oao.KindTime, NoEdit: true},
		},
		Filters: []oao.Filter{
			{Field: "key", Label: "站点键", Op: oao.OpLike},
			{Field: "breaker_paused", Label: "熔断闸住", Kind: oao.KindBool, Op: oao.OpEq},
		},
		DefaultSort: "key",
		PageSize:    50,
	}
}
