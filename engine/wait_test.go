package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v4/pkg/browser"
)

// 等待渲染失败：trace 里一条「等待渲染」失败步骤（带卡住的步骤/选择器/耗时），
// 现场 HTML 登记进页面快照缓冲。
func TestRecordRenderWaitFailure(t *testing.T) {
	tr := newTrace(nil, 31, 0)
	ctx := withTrace(context.Background(), tr)

	const html = `<html><body><p id="captcha">安全验证</p></body></html>`
	waitErr := &browser.WaitError{
		Stage:   "selector",
		Err:     errors.New("element not found"),
		HTML:    html,
		Elapsed: 3 * time.Second,
	}
	recordRenderWaitFailure(ctx, "https://site.example/detail/1",
		browser.PageOptions{WaitSelector: "#detail"}, waitErr)

	records := tr.flush()
	if len(records) != 1 {
		t.Fatalf("steps = %d, want 1", len(records))
	}
	r := records[0]
	if r.Step != renderWaitTraceStep || r.Status != "failed" {
		t.Fatalf("step = %+v, want %q failed", r, renderWaitTraceStep)
	}
	data := decodeTraceData(t, r)
	if data["wait_selector"] != "#detail" || data["wait_stage"] != "selector" {
		t.Fatalf("data = %#v", data)
	}
	if data["elapsed_ms"] != float64(3000) {
		t.Fatalf("elapsed_ms = %v, want 3000", data["elapsed_ms"])
	}
	if len(tr.snapshots) != 1 || string(tr.snapshots[0].Body) != html {
		t.Fatalf("等待失败的现场没进页面快照缓冲：%+v", tr.snapshots)
	}
}

// 导航失败（不是 WaitError）没有页面可看，不该写「等待渲染」步骤。
func TestRecordRenderWaitFailureIgnoresOtherErrors(t *testing.T) {
	tr := newTrace(nil, 32, 0)
	ctx := withTrace(context.Background(), tr)

	recordRenderWaitFailure(ctx, "https://site.example/", browser.PageOptions{}, errors.New("navigate: connection refused"))
	if got := tr.flush(); len(got) != 0 {
		t.Fatalf("导航失败不该写等待步骤：%+v", got)
	}
	if len(tr.snapshots) != 0 {
		t.Fatalf("导航失败没有页面可页面快照，实得 %d 页", len(tr.snapshots))
	}
}
