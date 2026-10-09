package engine

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	"github.com/ydtg1993/papa/v3/config"
	"github.com/ydtg1993/papa/v3/core"
	"github.com/ydtg1993/papa/v3/pkg/loggers"
	"gorm.io/datatypes"
)

// 失败的尝试必须在 trace 里留下**原因**。
//
// 这是从一次真实抓取里发现的：一条解析失败的任务，追踪抽屉里只有「抓取分类页 ✔」「归档页面 ✔」
// 两条 —— 前几步都成功，到底为什么死掉只在引擎日志里。而"这一次为什么失败"正是点开追踪最想知道的事。
func TestFailedAttemptRecordsFailureStep(t *testing.T) {
	srv := serveBody(t, "<html>ok</html>")
	e, pool, _ := archiveEngine(t, config.ArchiveModeFailure)

	// 带分类的错误：抽屉里要能一眼分清"结构变了"还是"重试耗尽"
	boom := core.WrapNoRetryKind("structure", errors.New("catalog grid missing"))
	task := &Task{ID: 9, Stage: "stub", URL: srv.URL}

	err := e.runAttempt(context.Background(), &pageFetcher{urls: []string{srv.URL}, err: boom}, task, 0)
	if !errors.Is(err, boom) {
		t.Fatalf("runAttempt = %v, want %v", err, boom)
	}

	trace := pool.all()
	if !strings.Contains(trace, failureTraceStep) {
		t.Fatalf("trace 里应有 %q 步骤：\n%s", failureTraceStep, trace)
	}
	if !strings.Contains(trace, "catalog grid missing") {
		t.Fatalf("%q 步骤里应带上失败原因：\n%s", failureTraceStep, trace)
	}
	if !strings.Contains(trace, "structure") {
		t.Fatalf("%q 步骤里应带上错误分类：\n%s", failureTraceStep, trace)
	}
}

// panic 也是失败，而且更该在抽屉里看见（workerpool 那头照旧记栈与失败计数）。
func TestPanickingAttemptRecordsFailureStep(t *testing.T) {
	srv := serveBody(t, "<html>ok</html>")
	e, pool, _ := archiveEngine(t, config.ArchiveModeFailure)
	task := &Task{ID: 11, Stage: "stub", URL: srv.URL}

	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("runAttempt 应当把 panic 原样抛出（workerpool 靠它记栈）")
			}
		}()
		_ = e.runAttempt(context.Background(), &pageFetcher{
			urls: []string{srv.URL}, panicWith: "boom-in-handler",
		}, task, 0)
	}()

	trace := pool.all()
	if !strings.Contains(trace, failureTraceStep) || !strings.Contains(trace, "boom-in-handler") {
		t.Fatalf("panic 的尝试也要在 trace 里留下原因：\n%s", trace)
	}
}

// 成功的尝试不写这条步骤 —— 否则每条任务都挂着"任务失败"，抽屉里全是噪声。
func TestSuccessfulAttemptHasNoFailureStep(t *testing.T) {
	srv := serveBody(t, "<html>ok</html>")
	e, pool, _ := archiveEngine(t, config.ArchiveModeFailure)
	task := &Task{ID: 10, Stage: "stub", URL: srv.URL}

	if err := e.runAttempt(context.Background(), &pageFetcher{urls: []string{srv.URL}}, task, 0); err != nil {
		t.Fatalf("runAttempt = %v", err)
	}
	if strings.Contains(pool.all(), failureTraceStep) {
		t.Fatalf("成功的尝试不该写 %q 步骤：\n%s", failureTraceStep, pool.all())
	}
}

// 空 meta 列（NULL / 空串）不是"解不出来"：老行从来没写过 Meta，不带 Meta 的任务也是合法的。
//
// 这也是从真实抓取里发现的：库里 60+ 条老行，启动恢复一次就为其中几条各刷一条
// 「task meta 列解不出来（按无 Meta 继续）：unexpected end of JSON input（原值 ）」——
// 按无 Meta 继续是对的，但它是**正常状态**，不该进 WARN。
func TestMetaFromJSONTreatsEmptyColumnAsNoMeta(t *testing.T) {
	logBuf := &bytes.Buffer{}
	e := &Engine{loggerSet: &loggers.LoggerSet{Engine: func() *logrus.Logger {
		l := logrus.New()
		l.SetOutput(logBuf)
		l.SetLevel(logrus.WarnLevel)
		return l
	}()}}

	for _, raw := range []string{"", "   ", "\n"} {
		got := e.metaFromJSON(datatypes.JSON(raw))
		if got != nil {
			t.Errorf("空 meta 列（%q）应解成 nil，实得 %v", raw, got)
		}
	}
	if logBuf.Len() != 0 {
		t.Fatalf("空列不该记日志，实得：%s", logBuf.String())
	}

	// 坏值仍然要留一条 Warn：那一列只有 toModel 一处写，坏值意味着有人动过库，
	// 而 handler 那边报的会是"缺少 series_id"，排查方向会全落在业务侧。
	if got := e.metaFromJSON(datatypes.JSON(`{"series_id":`)); got != nil {
		t.Fatalf("坏值应解成 nil，实得 %v", got)
	}
	if !strings.Contains(logBuf.String(), "task meta 列解不出来") {
		t.Fatalf("坏值应当留一条 Warn，实得：%q", logBuf.String())
	}

	// 合法值照常解出来（含 `null` 与 `{}` 这两种"没有业务键"的写法，都不该报错）
	got := e.metaFromJSON(datatypes.JSON(`{"series_id":"10"}`))
	if got["series_id"] != "10" {
		t.Fatalf("正常 meta 解出来 = %v", got)
	}
	for _, raw := range []string{"null", "{}"} {
		if got := e.metaFromJSON(datatypes.JSON(raw)); len(got) != 0 {
			t.Errorf("%s 应当解成空，实得 %v", raw, got)
		}
	}
}
