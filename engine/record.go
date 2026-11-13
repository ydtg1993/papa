package engine

import (
	"context"
	"fmt"

	"github.com/ydtg1993/papa/v4/pkg/browser"
)

// RecordNetwork observes one browser page under a bounded recording budget.
// Raw network evidence stays in memory; no automatic credential-bearing archive.
func (e *Engine) RecordNetwork(ctx context.Context, rawURL string, opts browser.RecordOptions) (*browser.NetworkRecording, error) {
	if err := browserProxyError(ctx); err != nil {
		return nil, err
	}
	if e.GetBrowserPool() == nil {
		return nil, fmt.Errorf("browser pool is not configured (browser.enable=false)")
	}
	opts.Page = mergeBrowserOptions(ctx, opts.Page)
	var record *browser.NetworkRecording
	err := e.GetBrowserPool().WithBrowser(ctx, func(b *browser.Browser) error {
		var err error
		record, err = b.RecordNetwork(ctx, rawURL, opts)
		return err
	})
	data := map[string]any{}
	if record != nil {
		data["entries"] = len(record.Entries)
		data["dropped"] = record.Dropped
	}
	if err != nil {
		traceFrameworkFailure(ctx, traceFromCtx(ctx), err, "网络录制", fmt.Errorf("network recording failed"), data)
	} else {
		traceFromCtx(ctx).Step("网络录制", data)
	}
	return record, err
}
