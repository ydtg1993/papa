package engine

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/models"
	"github.com/ydtg1993/papa/v3/pkg/browser"
)

// 等待渲染失败：trace 里一条「等待渲染」失败步骤（带卡住的步骤/选择器/耗时），
// 现场 HTML 登记进归档缓冲。
func TestRecordRenderWaitFailure(t *testing.T) {
	e, _, _ := archiveEngine(t, config.ArchiveModeFailure)
	tr := newTrace(e.db, logrus.New(), 31, 0)
	ar := e.newArchiveBuffer(&Task{ID: 31, Stage: "stub", Site: "site"}, 0)
	ctx := withArchive(withTrace(context.Background(), tr), ar)

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
	if r.Step != renderWaitTraceStep || r.Status != models.TraceFailed {
		t.Fatalf("step = %+v, want %q failed", r, renderWaitTraceStep)
	}
	data := decodeTraceData(t, r)
	if data["wait_selector"] != "#detail" || data["wait_stage"] != "selector" {
		t.Fatalf("data = %#v", data)
	}
	if data["elapsed_ms"] != float64(3000) {
		t.Fatalf("elapsed_ms = %v, want 3000", data["elapsed_ms"])
	}
	if len(ar.pages) != 1 || ar.pages[0].html != html {
		t.Fatalf("等待失败的现场没进归档缓冲：%+v", ar.pages)
	}
}

// 导航失败（不是 WaitError）没有页面可看，不该写「等待渲染」步骤。
func TestRecordRenderWaitFailureIgnoresOtherErrors(t *testing.T) {
	e, _, _ := archiveEngine(t, config.ArchiveModeFailure)
	tr := newTrace(e.db, logrus.New(), 32, 0)
	ar := e.newArchiveBuffer(&Task{ID: 32, Stage: "stub", Site: "site"}, 0)
	ctx := withArchive(withTrace(context.Background(), tr), ar)

	recordRenderWaitFailure(ctx, "https://site.example/", browser.PageOptions{}, errors.New("navigate: connection refused"))
	if got := tr.flush(); len(got) != 0 {
		t.Fatalf("导航失败不该写等待步骤：%+v", got)
	}
	if len(ar.pages) != 0 {
		t.Fatalf("导航失败没有页面可归档，实得 %d 页", len(ar.pages))
	}
}
