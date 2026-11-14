package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestTraceFileNegativeBudgetsRejected(t *testing.T) {
	for _, files := range []TraceFilesConfig{{MaxFileBytes: -1}, {MaxTotalBytes: -1}, {MaxFiles: -1}} {
		t.Run("negative", func(t *testing.T) {
			defer func() {
				if got := recover(); got == nil || !strings.Contains(fmt.Sprint(got), "crawler.trace_files") {
					t.Fatalf("expected trace budget validation, got %v", got)
				}
			}()
			c := &Config{}
			c.Crawler.TraceFiles = files
			Validate(c, nil)
		})
	}
}
