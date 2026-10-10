package sitesource

import (
	"context"
	"errors"
	"net/http"

	"github.com/ydtg1993/oao"
	"github.com/ydtg1993/papa/v3/admin/gormsource"
	"github.com/ydtg1993/papa/v3/engine"
	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/gorm"
)

// SiteActions 后台对**单个站点**的手动操作，由 engine.Engine 实现。
// 定义成接口是为了让本包不依赖引擎，且这些处理函数能在没有数据库时单测。
//
// 站点用 **Key**（不是行 ID）标识：表的 `IDField` 就是 `key`，所以 `req.ID` 直接是站点键
// —— 与队列/熔断/任务表那一套站点维度一致，也免得把客户端的行快照当参数。
type SiteActions interface {
	// ForceRepollSiteRepeatableTasks 把该站**所有可轮询的已完成任务**都投一遍（**忽略周期**）。
	ForceRepollSiteRepeatableTasks(site string) (int, error)
	// SetSiteAutoRepeat 开/停该站的自动轮询；已经是目标值时返回 engine.ErrSiteAlreadyAuto / Manual。
	SetSiteAutoRepeat(site string, on bool) error
}

// Table 返回内置「站点」表格的声明。acts 为 nil 时不注册任何写操作，表格退化为只读。
func Table(db *gorm.DB, acts SiteActions) oao.Table {
	t := oao.Table{
		Key: "site", Label: "站点", Group: "数据",
		// 站点行是引擎启动时按声明播种 + 跑起来按列回写的，所以这里**不给编辑**：
		// 配置在声明里（configs/sites/<站名>.go），运营能动的只有下面两个动作。
		Source: gormsource.New(gormsource.Config{
			DB:     db,
			Model:  &models.CrawlerSite{},
			Search: []string{"key", "base_url"},
		}),
		// 主键用「站点键」而不是自增 ID：动作要的就是站点（与队列名 repeat_queue:<站点> 一脉相承）
		IDField: "key",
		Columns: []oao.Column{
			{Field: "key", Label: "站点键", NoEdit: true},
			{Field: "base_url", Label: "BaseURL", Render: oao.RenderLink, Href: "{base_url}", NewTab: true, NoEdit: true},
			{Field: "auto_repeat", Label: "自动轮询", Kind: oao.KindBool, Width: "100px", NoEdit: true},
			{Field: "stage_count", Label: "阶段数", Kind: oao.KindNumber, Width: "80px", NoEdit: true},
			{Field: "last_repeat_at", Label: "上次轮询", Kind: oao.KindTime, Width: "150px", NoEdit: true},
			{Field: "repeat_total", Label: "累计轮询", Kind: oao.KindNumber, Width: "90px", NoEdit: true},
			{Field: "repeat_backlog", Label: "待轮询", Kind: oao.KindNumber, Width: "80px", NoEdit: true},
			{Field: "last_repeat_error", Label: "最近错误", Render: oao.RenderInput, MaxLen: 40, NoEdit: true},
			{Field: "breaker_paused", Label: "熔断闸住", Kind: oao.KindBool, Width: "90px", NoEdit: true},
			{Field: "breaker_paused_at", Label: "闸住时间", Kind: oao.KindTime, Width: "150px", NoEdit: true},
			{Field: "updated_at", Label: "更新时间", Kind: oao.KindTime, NoEdit: true},
		},
		Filters: []oao.Filter{
			{Field: "key", Label: "站点键", Op: oao.OpLike},
			{Field: "auto_repeat", Label: "自动轮询", Op: oao.OpEq},
			{Field: "breaker_paused", Label: "熔断闸住", Op: oao.OpEq},
		},
		DefaultSort: "key",
		PageSize:    50,
	}
	if acts != nil {
		t.Actions = siteActions(acts)
	}
	return t
}

// siteActions 三个动作都**显式**（方向写死在动作里，不看客户端回传的行快照）：
// 全量轮询、暂停自动轮询、恢复自动轮询。
func siteActions(acts SiteActions) []oao.Action {
	return []oao.Action{
		{
			Key: "repeat_now", Label: "轮询任务", Tone: oao.ToneInfo,
			Confirm: "把该站可轮询的任务全投一遍？**忽略周期**（比「立即执行」激进）：每条的下次到点会按各自周期往后推。",
			Handler: func(ctx context.Context, req oao.ActionRequest) error {
				site, err := parseSite(req)
				if err != nil {
					return err
				}
				// 投出去多少条由引擎记日志；组件这边只能回成功/失败（oao 的动作带不回数据）
				_, err = acts.ForceRepollSiteRepeatableTasks(site)
				return toOaoError(err)
			},
		},
		{
			Key: "repeat_pause", Label: "暂停自动轮询", Tone: oao.ToneInfo,
			Confirm: "停掉该站的自动轮询？已经排上队的那一轮仍会跑完，之后只能手动触发（「轮询任务」照旧可用）。",
			Handler: func(ctx context.Context, req oao.ActionRequest) error {
				site, err := parseSite(req)
				if err != nil {
					return err
				}
				return toOaoError(acts.SetSiteAutoRepeat(site, false))
			},
		},
		{
			Key: "repeat_resume", Label: "恢复自动轮询", Tone: oao.ToneInfo,
			Confirm: "让该站重新参与自动轮询？下一轮扫描就会投它。",
			Handler: func(ctx context.Context, req oao.ActionRequest) error {
				site, err := parseSite(req)
				if err != nil {
					return err
				}
				return toOaoError(acts.SetSiteAutoRepeat(site, true))
			},
		},
	}
}

// parseSite 取这次动作要操作的站点键（表的 IDField 就是 key）。
func parseSite(req oao.ActionRequest) (string, error) {
	if req.ID == "" {
		return "", oao.Fail(http.StatusBadRequest, "缺少站点键（主键字段是 key）")
	}
	return req.ID, nil
}

// toOaoError 把引擎的哨兵错误映射成带状态码的提示语；无法识别的错误原样返回（组件按 500 处理）。
func toOaoError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, engine.ErrUnknownSite):
		return oao.Fail(http.StatusNotFound, "站点不存在：站点行可能已被删（重启会按声明重新播种）")
	case errors.Is(err, engine.ErrSiteAlreadyAuto):
		return oao.Fail(http.StatusConflict, "该站点已经在自动轮询了，刷新页面后再看")
	case errors.Is(err, engine.ErrSiteAlreadyManual):
		return oao.Fail(http.StatusConflict, "该站点本来就没在自动轮询，刷新页面后再看")
	case errors.Is(err, engine.ErrDefaultScopeNoAuto):
		return oao.Fail(http.StatusBadRequest, "%v", err)
	case errors.Is(err, engine.ErrTaskChanged):
		return oao.Fail(http.StatusConflict, "该行已被他人修改，请刷新后重试")
	default:
		return err
	}
}
