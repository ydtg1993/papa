package taskflow

import (
	"embed"
	"github.com/ydtg1993/papa/v4/admin/page"
)

// Assets contains this feature's browser controller and view resources.
//
//go:embed static
var Assets embed.FS

//go:embed static/taskflow.js
var Script string

//go:embed static/taskflow.css
var CSS string

func Page() page.Descriptor {
	return page.Descriptor{Key: "task-flow-policy", Label: "运行调度策略", Group: "调度", Section: "flow", Script: Script, CSS: CSS}
}
