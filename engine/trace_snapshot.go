package engine

import (
	"context"
	"errors"
	"mime"
	"strings"

	"github.com/ydtg1993/papa/v4/internal/tracestore"
	"github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

const maxTraceSnapshots = 32

// Snapshots belong to the same attempt buffer, mutex, retention decision and
// bounded file as steps. There is no independent archive lifecycle.
func (t *Trace) holdPage(page *htmlfetch.Page) {
	if t == nil || page == nil || page.URL == nil {
		return
	}
	t.holdSnapshotBody(tracestore.Snapshot{URL: page.URL.String(), Status: page.StatusCode, ContentType: page.ContentType}, page.HTML)
}
func (t *Trace) holdRendered(body, rawURL string) {
	if t == nil {
		return
	}
	t.holdSnapshotBody(tracestore.Snapshot{URL: rawURL, ContentType: "text/html"}, body)
}

// Only textual HTTP representations are useful page evidence. Binary downloads
// and account-managed exchanges are intentionally not mirrored.
func (t *Trace) holdResponse(r *htmlfetch.Response) {
	if t == nil || r == nil || r.URL == nil {
		return
	}
	kind, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if !strings.HasPrefix(kind, "text/") && kind != "application/json" && !strings.HasSuffix(kind, "+json") && kind != "application/xhtml+xml" {
		return
	}
	t.holdSnapshot(tracestore.Snapshot{URL: r.URL.String(), Status: r.StatusCode, ContentType: r.Header.Get("Content-Type"), Body: r.Body})
}
func (t *Trace) holdSnapshot(p tracestore.Snapshot) {
	t.holdSnapshotBody(p, "")
}

// Check the source length before converting or copying the body. Large pages
// must not allocate another response-sized buffer merely to be rejected.
func (t *Trace) holdSnapshotBody(p tracestore.Snapshot, text string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	bodySize := len(p.Body)
	if p.Body == nil {
		bodySize = len(text)
	}
	if t.done || bodySize == 0 {
		return
	}
	limit := t.snapshotLimit
	if limit <= 0 {
		limit = tracestore.DefaultMaxFileBytes
	}
	size := int64(bodySize) + int64(len(p.URL)) + int64(len(p.ContentType))
	if size > limit {
		t.truncated = true
		return
	}
	// Keep the most recent pages when a catalog iterates many pages. This retains
	// the failure vicinity instead of only the first pages of a long attempt.
	for len(t.snapshots) > 0 && (len(t.snapshots) >= maxTraceSnapshots || t.snapshotBytes+size > limit) {
		old := t.snapshots[0]
		t.snapshotBytes -= int64(len(old.Body) + len(old.URL) + len(old.ContentType))
		copy(t.snapshots, t.snapshots[1:])
		t.snapshots[len(t.snapshots)-1] = tracestore.Snapshot{}
		t.snapshots = t.snapshots[:len(t.snapshots)-1]
		t.truncated = true
	}
	if p.Body == nil {
		p.Body = []byte(text)
	} else {
		p.Body = append([]byte(nil), p.Body...)
	}
	t.snapshots = append(t.snapshots, p)
	t.snapshotBytes += size
}

// TraceSnapshot reads a bounded snapshot through the trace store; callers never
// receive a filesystem path and cannot address files outside published traces.
func (e *Engine) TraceSnapshot(ctx context.Context, token string) ([]byte, error) {
	if !e.traceEnabled() {
		return nil, errors.New("步骤追踪未开启（crawler.trace.mode）")
	}
	return e.traceService().files.Snapshot(ctx, token)
}
