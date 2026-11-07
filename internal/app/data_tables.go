package app

import (
	"github.com/ydtg1993/oao"
	"github.com/ydtg1993/papa/v4/admin/tasksource"
	"gorm.io/gorm"
)

// taskDataTables keeps the historical task key and action routes so trace
// drawers, batch links and operation protections continue to use one table.
func taskDataTables(db *gorm.DB, acts tasksource.TaskActions) []oao.Table {
	task := tasksource.Table(db, acts)
	task.Label = "采集任务（crawler_tasks）"
	task.Header = `<div class="muted"><code>crawler_tasks</code> · 点击任务行的「追踪」弹窗查看对应执行记录；支持重新抓取（重投）、标记失败、加急与轮询设置。</div>`
	return []oao.Table{task}
}
