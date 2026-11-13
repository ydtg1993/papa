package config

import (
	"fmt"
)

// TraceMode selects retention for all task attempts in this process.
type TraceMode string

const (
	TraceOff    TraceMode = "off"
	TraceErrors TraceMode = "errors"
	TraceAll    TraceMode = "all"
)

// ModeOrDefault returns the effective mode and rejects invalid configuration.
// A zero Go configuration defaults to errors; use Mode: TraceOff to disable.
func (c TraceConfig) ModeOrDefault() TraceMode {
	switch c.Mode {
	case "":
		return TraceErrors
	case TraceOff, TraceErrors, TraceAll:
		return c.Mode
	}
	panic(fmt.Sprintf("config: crawler.trace.mode: invalid mode %q (off/errors/all)", c.Mode))
}
