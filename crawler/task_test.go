package crawler

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ydtg1993/papa/v2/models"
)

func TestTaskUnique(t *testing.T) {
	task := &Task{URL: "https://example.com", Stage: "mock"}
	if got := task.Unique(); got != "mock|https://example.com" {
		t.Fatalf("default unique = %q", got)
	}
	task.IdempotencyKey = "catalog:cat:2"
	if got := task.Unique(); got != "catalog:cat:2" {
		t.Fatalf("idempotency key unique = %q", got)
	}
}

func TestTaskDeliverAt(t *testing.T) {
	task := &Task{}
	if !task.deliverAt().IsZero() {
		t.Fatal("no delay should be zero time")
	}

	task.Delay = time.Minute
	if task.deliverAt().IsZero() {
		t.Fatal("Delay should produce a non-zero deliver time")
	}

	// NotBefore 优先于 Delay
	at := time.Now().Add(2 * time.Minute)
	task.NotBefore = at
	if !task.deliverAt().Equal(at) {
		t.Fatal("NotBefore should take precedence over Delay")
	}
}

// UpdateStatus 只能动 status / error 两列。
//
// 回归点：原来是「SELECT 整行 → 改字段 → Save 整行」，会把读到的 content 旧快照盖回库里 ——
// 业务刚 SaveResult 写进去的内容就这么没了（两条并发的路互相覆盖）。
func TestUpdateStatusOnlyTouchesItsOwnColumns(t *testing.T) {
	f := newFakeTaskDB()
	db := openFakeTaskDB(t, f)
	task := &Task{ID: 7, Stage: "stub", URL: "https://example.com"}

	if !task.UpdateStatus(db, models.TaskStatusSuccess, nil) {
		t.Fatal("正常路径应返回 true")
	}
	if !task.UpdateStatus(db, models.TaskStatusFailed, errors.New("boom")) {
		t.Fatal("失败路径应返回 true")
	}

	got := f.written()
	if !strings.Contains(got, "`status`") {
		t.Fatalf("应写入 status：\n%s", got)
	}
	for _, never := range []string{"`content`", "`title`", "`url`", "`stage`", "`idempotency_key`", "`retry`"} {
		if strings.Contains(got, never) {
			t.Fatalf("不该碰 %s（整行写会盖掉业务刚写的 content）：\n%s", never, got)
		}
	}
	if strings.Contains(got, "SELECT") {
		t.Fatalf("不该先把整行读回来：\n%s", got)
	}
	if !strings.Contains(got, "CONCAT") {
		t.Fatalf("失败时应在 SQL 里追加 error，而不是读出来拼字符串：\n%s", got)
	}
	if !f.writtenArgs_hasError("boom") {
		t.Fatalf("追加的错误内容应作为参数传给 SQL：%s", f.writtenArgs())
	}
}
