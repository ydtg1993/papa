package server

import (
	"github.com/ydtg1993/papa/v4/admin/shell"
	"github.com/ydtg1993/papa/v4/admin/taskflow"
)

var templateFS = shell.Assets()
var staticFS = templateFS
var TaskFlowScript = taskflow.Script
var TaskFlowCSS = taskflow.CSS
