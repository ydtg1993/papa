package tasksource

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/ydtg1993/oao"
	"github.com/ydtg1993/papa/v3/admin/gormsource"
	"github.com/ydtg1993/papa/v3/engine"
	"github.com/ydtg1993/papa/v3/models"
	"gorm.io/gorm"
)

// 任务状态文案，与 models.TaskStatus 对应（0 待处理 / 1 处理中 / 2 成功 / 3 失败）。
var taskStatusEnum = map[string]string{
	"0": "待处理",
	"1": "处理中",
	"2": "成功",
	"3": "失败",
}

var taskStatusTone = map[string]string{
	"0": "info",
	"1": "warn",
	"2": "ok",
	"3": "err",
}

// TaskActions 后台对单行任务的手动操作，由 engine.Engine 实现。
// 定义成接口是为了让本包不依赖 crawler，且这些处理函数能在没有数据库时单测。
type TaskActions interface {
	// RetryTask 的 wasReprocess 是行快照里带回来的 reprocess 旧值，用作版本条件防重复提交。
	RetryTask(id uint, wasReprocess int) error
	MarkTaskFailed(id uint, reason string) error
	DeleteTask(id uint) error
	// UrgentTask 把一行还没被取走的任务投到快车道；重复加急/已被取走返回 engine.ErrTaskUrgent。
	UrgentTask(id uint) error
	// SetTaskRepeatable 开/停这一行的周期轮询（只改标记，下一轮 scan 才生效）；
	// 已经开着又开返回 engine.ErrTaskRepeatOn，已经停着又停返回 engine.ErrTaskRepeatOff。
	SetTaskRepeatable(id uint, on bool) error
	// SetTaskRepeatInterval 改这一行的轮询周期（秒；0 = 跟全局）。
	// wasSeconds 是行快照里的旧值，当版本条件防重复提交；周期不合法返回 engine.ErrRepeatIntervalBad。
	SetTaskRepeatInterval(id uint, wasSeconds, seconds int) error
}

// Table 返回内置「任务」表格的声明。acts 为 nil 时不注册任何写操作，表格退化为只读。
func Table(db *gorm.DB, acts TaskActions) oao.Table {
	t := oao.Table{
		Key: "task", Label: "任务", Group: "数据",
		Source: gormsource.New(gormsource.Config{
			DB:     db,
			Model:  &models.CrawlerTask{},
			Search: []string{"url", "title", "error"},
		}),
		Columns: []oao.Column{
			{Field: "id", Label: "ID", Kind: oao.KindNumber, Width: "70px", NoEdit: true},
			// 列名必须是**数据库里的真实列名**：行数据的键来自 `SELECT *` 的结果集，对不上就永远渲染成空。
			// gorm 给 `CrawlerTask.PID` 生成的列名是 `p_id`（不是 `pid`）—— 这条约定由
			// engine.TestFakeTaskColumnsCoverModel 与下面的声明用例一起钉住。
			{Field: "p_id", Label: "父任务", Kind: oao.KindNumber, Width: "80px", NoEdit: true},
			{Field: "stage", Label: "阶段"},
			{Field: "site", Label: "站点", Width: "90px"}, // 多站时按站点分：熔断、日志、任务都按它
			{Field: "url", Label: "URL", Render: oao.RenderLink, Href: "{url}", NewTab: true},
			{Field: "title", Label: "标题"},
			{Field: "status", Label: "状态", Kind: oao.KindNumber,
				Render: oao.RenderEnum, Enum: taskStatusEnum, Tone: taskStatusTone},
			{Field: "retry", Label: "重试", Kind: oao.KindNumber, Width: "70px"},
			{Field: "reprocess", Label: "重投", Kind: oao.KindNumber, Width: "70px"},
			{Field: "repeatable", Label: "可轮询", Kind: oao.KindBool, Width: "90px"},
			{Field: "repeat", Label: "轮询次数", Kind: oao.KindNumber, Width: "90px"},
			// 周期与两个时刻：判据是 next_repeat_at（到点才重投），last_repeat_at 只是记录
			{Field: "repeat_interval", Label: "轮询周期(秒)", Kind: oao.KindNumber, Width: "110px"},
			{Field: "last_repeat_at", Label: "上次轮询", Kind: oao.KindTime, Width: "150px"},
			{Field: "next_repeat_at", Label: "下次轮询", Kind: oao.KindTime, Width: "150px"},
			{Field: "urgent", Label: "加急", Kind: oao.KindBool, Width: "80px"},
			{Field: "error", Label: "错误", Render: oao.RenderInput, MaxLen: 40},
			{Field: "created_at", Label: "创建时间", Kind: oao.KindTime, NoEdit: true},
			{Field: "updated_at", Label: "更新时间", Kind: oao.KindTime, NoEdit: true},
		},
		Filters: []oao.Filter{
			{Field: "url", Label: "URL", Op: oao.OpLike},
			{Field: "stage", Label: "阶段", Op: oao.OpEq},
			{Field: "site", Label: "站点", Op: oao.OpEq},
			{Field: "status", Label: "状态", Kind: oao.KindNumber,
				Op: oao.OpIn, Options: taskStatusEnum},
			{Field: "retry", Label: "重试次数 >", Kind: oao.KindNumber, Op: oao.OpGt},
			{Field: "created_at", Label: "创建时间", Kind: oao.KindTime, Op: oao.OpBetween},
		},
		DefaultSort: "-updated_at",
		PageSize:    20,
	}
	if acts != nil {
		t.Actions = taskActions(acts)
	}
	return t
}

// taskActions 声明操作列。写操作只是把请求转给 TaskActions，业务错误经 oao.Fail 带上状态码返回，
// 成败都由 Config.OnAction 记审计；「追踪」是个只读入口，服务端只校验 id，展示交给前端脚本。
//
// 顺序有讲究：oao 超过 3 个动作时只平铺前两个，其余收进「更多 ▾」。
// 所以最常用的「重投」和只读排查入口「追踪」放前面，加急/开轮询/停轮询/标失败/删除收进更多里。
func taskActions(acts TaskActions) []oao.Action {
	return []oao.Action{
		{
			Key: "retry", Label: "重投", Tone: oao.ToneInfo,
			Confirm: "重置该任务并重新投递到它的阶段队列？",
			Handler: func(ctx context.Context, req oao.ActionRequest) error {
				id, err := parseID(req)
				if err != nil {
					return err
				}
				// 版本条件：拿行快照里的 reprocess 旧值，重复点击只有第一次能成功
				raw := req.RowString("reprocess")
				if raw == "" {
					return oao.Fail(http.StatusBadRequest, "缺少版本号 reprocess，无法防重复提交")
				}
				was, convErr := strconv.Atoi(raw)
				if convErr != nil {
					return oao.Fail(http.StatusBadRequest, "版本号 reprocess 不是整数：%q", raw)
				}
				return toOaoError(acts.RetryTask(id, was))
			},
		},
		{
			Key: "trace", Label: "追踪", Tone: oao.ToneInfo,
			// 这个动作本身不做事，只给前端一个可点的入口：oao 的动作成功时只能回 {"status":"ok"}，
			// 带不回数据（见 oao 组件 handler 的成功分支）。真正的展示由 /static/trace.js 接管 ——
			// 它嗅探这次请求、取到任务 id，再拉 /api/task/trace 画时间线。
			// 服务端保留这个动作是为了让「谁点了追踪」进得了操作日志。
			Handler: func(ctx context.Context, req oao.ActionRequest) error {
				_, err := parseID(req)
				return err
			},
		},
		{
			Key: "urgent", Label: "加急", Tone: oao.ToneInfo,
			Confirm: "把该任务投到所属阶段的快车道，插到排队任务前面执行？",
			Handler: func(ctx context.Context, req oao.ActionRequest) error {
				id, err := parseID(req)
				if err != nil {
					return err
				}
				return toOaoError(acts.UrgentTask(id))
			},
		},
		{
			Key: "repeat_on", Label: "开轮询", Tone: oao.ToneInfo,
			Confirm: "让该任务参与周期轮询？只改标记，下一轮扫描时才生效，不会立刻重跑一次。",
			Handler: func(ctx context.Context, req oao.ActionRequest) error {
				id, err := parseID(req)
				if err != nil {
					return err
				}
				// 方向写死在动作里，不看 req.Row —— 那是客户端回传的展示快照，
				// 拿它决定"开还是停"等于把方向交给前端状态。
				return toOaoError(acts.SetTaskRepeatable(id, true))
			},
		},
		{
			Key: "repeat_off", Label: "停轮询", Tone: oao.ToneInfo,
			Confirm: "停掉该任务的周期轮询？只改标记，下一轮扫描时才生效；已经跑起来的这一轮不受影响。",
			Handler: func(ctx context.Context, req oao.ActionRequest) error {
				id, err := parseID(req)
				if err != nil {
					return err
				}
				return toOaoError(acts.SetTaskRepeatable(id, false))
			},
		},
		{
			Key: "repeat_interval", Label: "设轮询周期", Tone: oao.ToneInfo,
			Confirm: "改这条任务的轮询周期？只改周期，不会立刻重跑一次（想立刻跑请用「重投」）。",
			Form: []oao.Field{
				{Name: "seconds", Label: "轮询周期（秒）", Kind: oao.KindNumber, Required: true,
					Help: "600 = 10 分钟；0 = 跟全局 repeat_queue.interval；最小 10 秒"},
			},
			Handler: func(ctx context.Context, req oao.ActionRequest) error {
				id, err := parseID(req)
				if err != nil {
					return err
				}
				// 版本条件：拿行快照里的旧周期，重复点击只有第一次能成功（同「重投」用 reprocess）
				raw := req.RowString("repeat_interval")
				if raw == "" {
					return oao.Fail(http.StatusBadRequest, "缺少版本号 repeat_interval，无法防重复提交")
				}
				was, convErr := strconv.Atoi(raw)
				if convErr != nil {
					return oao.Fail(http.StatusBadRequest, "版本号 repeat_interval 不是整数：%q", raw)
				}
				seconds, ok := req.Int("seconds")
				if !ok {
					return oao.Fail(http.StatusBadRequest, "轮询周期（秒）必填，且必须是整数")
				}
				return toOaoError(acts.SetTaskRepeatInterval(id, was, seconds))
			},
		},
		{
			Key: "fail", Label: "标失败", Tone: oao.ToneWarn,
			Confirm: "确认把该任务标记为失败？",
			Form: []oao.Field{
				{Name: "reason", Label: "失败原因", Widget: oao.WidgetTextarea, Required: true,
					Help: "会写进任务的错误信息，同时记入操作日志"},
			},
			Handler: func(ctx context.Context, req oao.ActionRequest) error {
				id, err := parseID(req)
				if err != nil {
					return err
				}
				// 前端 Required 只是拦一道，服务端不能信客户端
				reason := strings.TrimSpace(req.String("reason"))
				if reason == "" {
					return oao.Fail(http.StatusBadRequest, "失败原因不能为空")
				}
				return toOaoError(acts.MarkTaskFailed(id, reason))
			},
		},
		oao.RemoveAction(func(ctx context.Context, req oao.ActionRequest) error {
			id, err := parseID(req)
			if err != nil {
				return err
			}
			return toOaoError(acts.DeleteTask(id))
		}),
	}
}

// parseID 把组件透传的主键字符串转成任务 ID。
func parseID(req oao.ActionRequest) (uint, error) {
	id, err := strconv.ParseUint(req.ID, 10, 64)
	if err != nil || id == 0 {
		return 0, oao.Fail(http.StatusBadRequest, "无效的任务 ID：%q", req.ID)
	}
	return uint(id), nil
}

// toOaoError 把 crawler 的哨兵错误映射成带状态码的提示语；无法识别的错误原样返回（组件按 500 处理）。
func toOaoError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, engine.ErrTaskNotFound):
		return oao.Fail(http.StatusNotFound, "任务不存在，可能已被删除")
	case errors.Is(err, engine.ErrTaskChanged):
		return oao.Fail(http.StatusConflict, "该行已被他人修改，请刷新后重试")
	case errors.Is(err, engine.ErrTaskTerminal):
		return oao.Fail(http.StatusConflict, "该任务已结束（成功或失败），无需再标记失败")
	case errors.Is(err, engine.ErrTaskProcessing):
		return oao.Fail(http.StatusConflict, "该任务正在处理中，请先标记失败或等它结束")
	case errors.Is(err, engine.ErrStageNotRegistered):
		return oao.Fail(http.StatusConflict, "该任务的阶段未注册，无法重新投递")
	case errors.Is(err, engine.ErrTaskUrgent):
		return oao.Fail(http.StatusConflict, "该任务已经加急过了，或已被取走，刷新后再看")
	case errors.Is(err, engine.ErrTaskFinished):
		return oao.Fail(http.StatusConflict, "该任务已结束，加急没有意义（要重跑请用「重投」）")
	case errors.Is(err, engine.ErrTaskRepeatOn):
		return oao.Fail(http.StatusConflict, "该任务已经在轮询了，刷新页面后再看")
	case errors.Is(err, engine.ErrTaskRepeatOff):
		return oao.Fail(http.StatusConflict, "该任务本来就没在轮询，刷新页面后再看")
	case errors.Is(err, engine.ErrRepeatIntervalBad):
		return oao.Fail(http.StatusBadRequest, "%v", err)
	default:
		return err
	}
}
