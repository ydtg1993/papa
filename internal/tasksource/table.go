package tasksource

import (
	"github.com/ydtg1993/oao"
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

// Table 返回内置「任务」表格的声明。
func Table(db *gorm.DB) oao.Table {
	return oao.Table{
		Key: "task", Label: "任务", Group: "数据",
		Source: New(db),
		Columns: []oao.Column{
			{Field: "id", Label: "ID", Kind: oao.KindNumber, Width: "70px"},
			{Field: "stage", Label: "阶段"},
			{Field: "url", Label: "URL", Render: oao.RenderLink, Href: "{url}"},
			{Field: "title", Label: "标题"},
			{Field: "status", Label: "状态", Kind: oao.KindNumber,
				Render: oao.RenderEnum, Enum: taskStatusEnum, Tone: taskStatusTone},
			{Field: "retry", Label: "重试", Kind: oao.KindNumber, Width: "70px"},
			{Field: "reprocess", Label: "重投", Kind: oao.KindNumber, Width: "70px"},
			{Field: "repeat", Label: "轮询", Kind: oao.KindNumber, Width: "70px"},
			{Field: "error", Label: "错误", Render: oao.RenderInput, MaxLen: 40},
			{Field: "updated_at", Label: "更新时间", Kind: oao.KindTime},
		},
		Filters: []oao.Filter{
			{Field: "url", Label: "URL", Op: oao.OpLike},
			{Field: "stage", Label: "阶段"},
			{Field: "status", Label: "状态", Kind: oao.KindNumber,
				Op: oao.OpIn, Options: taskStatusEnum},
		},
		DefaultSort: "-updated_at",
		PageSize:    20,
	}
}
