package taskflow

import "embed"

// Assets contains this feature's browser controller and view resources.
//
//go:embed static
var Assets embed.FS

//go:embed static/taskflow.js
var Script string

//go:embed static/taskflow.css
var CSS string
