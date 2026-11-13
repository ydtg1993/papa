package tasksource

import (
	_ "embed"
	"github.com/ydtg1993/oao"
	"gorm.io/gorm"
	"strings"
)

//go:embed views/workspace_header.html
var workspaceHeader string

// WorkspaceTable is the built-in task workspace presentation. Table retains
// its existing generic defaults for callers composing their own admin pages.
func WorkspaceTable(db *gorm.DB, acts TaskActions) oao.Table {
	t := Table(db, acts)
	t.Label = "采集任务（crawler_tasks）"
	t.Header = strings.TrimSuffix(workspaceHeader, "\n")
	return t
}
