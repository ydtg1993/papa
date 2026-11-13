package app

import (
	"github.com/ydtg1993/oao"
	"github.com/ydtg1993/papa/v4/admin/tasksource"
	"gorm.io/gorm"
)

// taskDataTables keeps the historical task key and action routes so trace
// drawers, batch links and operation protections continue to use one table.
func taskDataTables(db *gorm.DB, acts tasksource.TaskActions) []oao.Table {
	return []oao.Table{tasksource.WorkspaceTable(db, acts)}
}
