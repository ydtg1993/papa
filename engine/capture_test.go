package engine

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/go-rod/rod/lib/proto"
	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/models"
	"github.com/ydtg1993/papa/v3/pkg/browser"
)

// decodeTraceData 把 trace 步骤的 JSON 列解出来断言。
func decodeTraceData(t *testing.T, r models.TaskTrace) map[string]any {
	t.Helper()
	var data map[string]any
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatalf("步骤 data 不是合法 JSON：%v\n%s", err, r.Data)
	}
	return data
}

// 成功捕获：写一条 ok 步骤，data 带命中的 URL / 方法 / 资源类型 / elapsed。
func TestRecordCaptureSuccessStep(t *testing.T) {
	e, _, _ := archiveEngine(t, config.ArchiveModeFailure)
	tr := newTrace(e.db, logrus.New(), 7, 0)
	ctx := withTrace(context.Background(), tr)

	res := &browser.CaptureResult{
		Request: &browser.CapturedRequest{
			URL:          "https://cdn.example/videos/a.m3u8?auth_key=xyz",
			Method:       "GET",
			ResourceType: proto.NetworkResourceTypeMedia,
		},
		Timeout: 30 * time.Second,
		Elapsed: 1200 * time.Millisecond,
	}
	e.recordCapture(ctx, "https://site.example/play/1", res, nil)

	records := tr.flush()
	if len(records) != 1 {
		t.Fatalf("steps = %d, want 1", len(records))
	}
	r := records[0]
	if r.Step != captureTraceStep || r.Status != models.TraceOK {
		t.Fatalf("step = %+v, want %q ok", r, captureTraceStep)
	}
	data := decodeTraceData(t, r)
	if data["matched_url"] != res.Request.URL {
		t.Fatalf("matched_url = %v, want %s", data["matched_url"], res.Request.URL)
	}
	if data["method"] != "GET" || data["resource_type"] != string(proto.NetworkResourceTypeMedia) {
		t.Fatalf("method/resource_type = %v/%v", data["method"], data["resource_type"])
	}
	if data["elapsed_ms"] != float64(1200) {
		t.Fatalf("elapsed_ms = %v, want 1200", data["elapsed_ms"])
	}
}

// 失败捕获：写一条 failed 步骤（带候选样本与超时），并把 HTML 登记进归档缓冲。
func TestRecordCaptureFailureStep(t *testing.T) {
	e, _, _ := archiveEngine(t, config.ArchiveModeFailure)
	tr := newTrace(e.db, logrus.New(), 9, 1)
	ar := e.newArchiveBuffer(&Task{ID: 9, Stage: "stub", Site: "site"}, 1)
	if ar == nil {
		t.Fatal("归档开着时 newArchiveBuffer 不该返回 nil")
	}
	ctx := withArchive(withTrace(context.Background(), tr), ar)

	const html = `<html><body><h1>安全验证</h1></body></html>`
	res := &browser.CaptureResult{
		HTML: html,
		Seen: []browser.CapturedRequest{
			{URL: "https://site.example/api/health", Method: "GET", ResourceType: proto.NetworkResourceTypeXHR},
			{URL: "https://site.example/noise", Method: "POST", ResourceType: proto.NetworkResourceTypeFetch},
		},
		SeenTotal: 5,
		Timeout:   30 * time.Second,
		Elapsed:   30 * time.Second,
	}
	boom := errors.New("no matching request")
	e.recordCapture(ctx, "https://site.example/play/1", res, boom)

	records := tr.flush()
	if len(records) != 1 {
		t.Fatalf("steps = %d, want 1", len(records))
	}
	r := records[0]
	if r.Step != captureTraceStep || r.Status != models.TraceFailed {
		t.Fatalf("step = %+v, want %q failed", r, captureTraceStep)
	}
	if r.Message != boom.Error() {
		t.Fatalf("message = %q, want %q", r.Message, boom.Error())
	}
	data := decodeTraceData(t, r)
	if data["candidates_total"] != float64(5) {
		t.Fatalf("candidates_total = %v, want 5", data["candidates_total"])
	}
	candidates, ok := data["candidates"].([]any)
	if !ok || len(candidates) != 2 {
		t.Fatalf("candidates = %#v, want 2 条", data["candidates"])
	}
	if candidates[0] != "GET https://site.example/api/health" {
		t.Fatalf("candidates[0] = %v", candidates[0])
	}
	if data["timeout_ms"] != float64(30000) || data["elapsed_ms"] != float64(30000) {
		t.Fatalf("timeout/elapsed = %v/%v, want 30000/30000", data["timeout_ms"], data["elapsed_ms"])
	}

	// 失败现场进了归档缓冲（落盘由 runAttempt 的 defer 统一做）
	if len(ar.pages) != 1 {
		t.Fatalf("归档缓冲 = %d 页, want 1", len(ar.pages))
	}
	if ar.pages[0].html != html || ar.pages[0].url != "https://site.example/play/1" {
		t.Fatalf("归档页 = %+v", ar.pages[0])
	}
}

// trace / archive 都没开（ctx 上什么都没挂）时，recordCapture 必须安静地 no-op ——
// 这是「可选协作者真的可选」的承诺。
func TestRecordCaptureWithoutTraceOrArchive(t *testing.T) {
	e, _, _ := archiveEngine(t, config.ArchiveModeFailure)
	// 故意不带 withTrace / withArchive
	e.recordCapture(context.Background(), "https://site.example/play/1", &browser.CaptureResult{
		HTML:    "<html>noise</html>",
		Elapsed: time.Second,
		Timeout: time.Second,
	}, errors.New("boom"))
	// 没 panic 就算过；归档缓冲是 nil，holdRendered 方法本身 nil 安全
}

// 引擎层的 ctx 挂载：runAttempt 进去以后，handler 能从 ctx 取到本次尝试的 Trace 与归档缓冲。
func TestRunAttemptAttachesTraceAndArchiveToCtx(t *testing.T) {
	e, _, _ := archiveEngine(t, config.ArchiveModeFailure)
	f := &ctxProbeFetcher{}
	task := &Task{ID: 21, Stage: "stub", Site: "site", URL: "https://site.example/"}

	if err := e.runAttempt(context.Background(), f, task, 0); err != nil {
		t.Fatalf("runAttempt: %v", err)
	}
	if f.trace == nil {
		t.Fatal("handler 的 ctx 上应挂上本次尝试的 Trace")
	}
	if f.trace.taskID != 21 || f.trace.attempt != 0 {
		t.Fatalf("挂错 Trace：task=%d attempt=%d", f.trace.taskID, f.trace.attempt)
	}
	if f.trace != task.Trace {
		t.Fatal("ctx 上的 Trace 应与 task.Trace 是同一个实例")
	}
	if !f.archive {
		t.Fatal("归档开着时 handler 的 ctx 上应挂上归档缓冲")
	}
}

// ctxProbeFetcher 只把 ctx 上的协作者取出来，不做任何抓取。
type ctxProbeFetcher struct {
	trace   *Trace
	archive bool
}

func (f *ctxProbeFetcher) GetStage() string { return "probe" }

func (f *ctxProbeFetcher) FetchHandler(ctx context.Context, _ *Task, _ *Engine) error {
	f.trace = traceFromCtx(ctx)
	f.archive = archiveFromCtx(ctx) != nil
	return nil
}

// 浏览器路径不支持逐请求代理：入口处就要报错，不能悄悄从别的出口出去。
func TestEngineCaptureRejectsPerRequestProxy(t *testing.T) {
	e := &Engine{}
	ctx := core.WithProxyURL(context.Background(), "http://127.0.0.1:8080")
	_, err := e.CaptureRequest(ctx, "https://example.com/", browser.CaptureOptions{
		Match: func(browser.CapturedRequest) bool { return true },
	})
	if err == nil {
		t.Fatal("逐请求代理地址应当报错")
	}
}

// 没建浏览器池时给一句能照着改的错，而不是 nil 解引用。
func TestEngineCaptureWithoutPool(t *testing.T) {
	e := &Engine{}
	_, err := e.CaptureRequest(context.Background(), "https://example.com/", browser.CaptureOptions{
		Match: func(browser.CapturedRequest) bool { return true },
	})
	if err == nil || !containsAll(err.Error(), "browser") {
		t.Fatalf("错误应指向浏览器未启用，实得 %v", err)
	}
}
