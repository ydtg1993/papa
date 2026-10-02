package tasksource

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/ydtg1993/oao"
	"github.com/ydtg1993/papa/v2/crawler"
)

// stubActions 记录调用并按预设返回错误，用来在没有数据库时测处理函数。
type stubActions struct {
	retryErr  error
	failErr   error
	deleteErr error
	gotID     []uint
	gotVer    []int    // RetryTask 收到的版本号
	gotReason []string // MarkTaskFailed 收到的原因
}

func (s *stubActions) RetryTask(id uint, was int) error {
	s.gotID = append(s.gotID, id)
	s.gotVer = append(s.gotVer, was)
	return s.retryErr
}

func (s *stubActions) MarkTaskFailed(id uint, reason string) error {
	s.gotID = append(s.gotID, id)
	s.gotReason = append(s.gotReason, reason)
	return s.failErr
}

func (s *stubActions) DeleteTask(id uint) error {
	s.gotID = append(s.gotID, id)
	return s.deleteErr
}

// buildTable 走一遍组件注册，顺带验证声明能被 oao 校验通过。
func buildTable(t *testing.T, acts TaskActions) *oao.TableInfo {
	t.Helper()
	o, err := oao.New(oao.Config{Tables: []oao.Table{Table(nil, acts)}})
	if err != nil {
		t.Fatalf("oao.New rejected the declaration: %v", err)
	}
	ts := o.Tables()
	if len(ts) != 1 {
		t.Fatalf("tables = %d, want 1", len(ts))
	}
	return ts[0]
}

// 三个操作与新增列/筛选都必须声明出来。
func TestTableDeclaration(t *testing.T) {
	info := buildTable(t, &stubActions{})

	if info.Key != "task" || info.Group != "数据" {
		t.Fatalf("key/group = %q/%q", info.Key, info.Group)
	}

	byKey := make(map[string]oao.ActionInfo, len(info.Actions))
	for _, a := range info.Actions {
		byKey[a.Key] = a
	}
	for _, want := range []struct {
		key, label string
		tone       oao.Tone
	}{
		{"retry", "重投", oao.ToneInfo},
		{"fail", "标失败", oao.ToneWarn},
		{"remove", "删除", oao.ToneErr},
	} {
		a, ok := byKey[want.key]
		if !ok {
			t.Fatalf("action %q missing: %+v", want.key, info.Actions)
		}
		if a.Label != want.label || a.Tone != want.tone {
			t.Errorf("action %q = %q/%q, want %q/%q", want.key, a.Label, a.Tone, want.label, want.tone)
		}
		if a.Confirm == "" {
			t.Errorf("action %q 应有二次确认", want.key)
		}
	}

	cols := make(map[string]oao.ColumnInfo, len(info.Columns))
	for _, c := range info.Columns {
		cols[c.Name] = c
	}
	if c, ok := cols["repeatable"]; !ok || c.Kind != oao.KindBool {
		t.Errorf("repeatable 列应为 KindBool: %+v", cols["repeatable"])
	}
	if c, ok := cols["pid"]; !ok || !c.NoEdit {
		t.Errorf("pid 列应存在且 NoEdit: %+v", cols["pid"])
	}

	filters := make(map[string]oao.FilterInfo, len(info.Filters))
	for _, f := range info.Filters {
		filters[f.Name] = f
	}
	if f, ok := filters["created_at"]; !ok || f.Op != oao.OpBetween || f.Widget != oao.WidgetDateRange {
		t.Errorf("created_at 筛选 = %+v, want between/daterange", filters["created_at"])
	}
	if f, ok := filters["stage"]; !ok || f.Op != oao.OpEq {
		t.Errorf("stage 筛选应为 OpEq: %+v", filters["stage"])
	}
	if f, ok := filters["retry"]; !ok || f.Op != oao.OpGt {
		t.Errorf("retry 筛选应为 OpGt: %+v", filters["retry"])
	}
	if f, ok := filters["status"]; !ok || f.Op != oao.OpIn || len(f.Options) != 4 {
		t.Errorf("status 筛选应为 OpIn + 4 个选项: %+v", filters["status"])
	}
}

// acts 为 nil 时退化为只读：不注册任何写路由。
func TestTableReadOnlyWithoutActions(t *testing.T) {
	if info := buildTable(t, nil); len(info.Actions) != 0 {
		t.Fatalf("actions = %+v, want none", info.Actions)
	}
}

// 处理函数把 crawler 的哨兵错误映射成带状态码的业务错误；入参校验（版本号、原因）也在这一层。
func TestActionHandlerErrorMapping(t *testing.T) {
	ver := map[string]any{"reprocess": "3"}
	reason := map[string]any{"reason": "内容违规"}
	cases := []struct {
		name   string
		stub   *stubActions
		action string
		id     string
		row    map[string]any
		values map[string]any
		want   int
	}{
		{"重投成功", &stubActions{}, "retry", "7", ver, nil, 0},
		{"重投时任务不存在", &stubActions{retryErr: crawler.ErrTaskNotFound}, "retry", "7", ver, nil, http.StatusNotFound},
		{"重投时阶段未注册", &stubActions{retryErr: crawler.ErrStageNotRegistered}, "retry", "7", ver, nil, http.StatusConflict},
		{"重投被并发改动", &stubActions{retryErr: crawler.ErrTaskChanged}, "retry", "7", ver, nil, http.StatusConflict},
		{"重投缺版本号", &stubActions{}, "retry", "7", nil, nil, http.StatusBadRequest},
		{"重投版本号非整数", &stubActions{}, "retry", "7", map[string]any{"reprocess": "abc"}, nil, http.StatusBadRequest},
		{"标失败成功", &stubActions{}, "fail", "7", nil, reason, 0},
		{"标失败原因空", &stubActions{}, "fail", "7", nil, map[string]any{"reason": "   "}, http.StatusBadRequest},
		{"标失败但任务已结束", &stubActions{failErr: crawler.ErrTaskTerminal}, "fail", "7", nil, reason, http.StatusConflict},
		{"删除处理中的行", &stubActions{deleteErr: crawler.ErrTaskProcessing}, "remove", "7", nil, nil, http.StatusConflict},
		{"无效 ID", &stubActions{}, "retry", "abc", ver, nil, http.StatusBadRequest},
		{"ID 为 0", &stubActions{}, "retry", "0", ver, nil, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := handlerFor(t, tc.stub, tc.action)(context.Background(), oao.ActionRequest{
				Table: "task", Action: tc.action, ID: tc.id, Row: tc.row, Values: tc.values,
			})
			if tc.want == 0 {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				if !reflect.DeepEqual(tc.stub.gotID, []uint{7}) {
					t.Fatalf("handler 透传的 ID = %v, want [7]", tc.stub.gotID)
				}
				if tc.action == "retry" && !reflect.DeepEqual(tc.stub.gotVer, []int{3}) {
					t.Fatalf("重投透传的版本号 = %v, want [3]", tc.stub.gotVer)
				}
				if tc.action == "fail" && !reflect.DeepEqual(tc.stub.gotReason, []string{"内容违规"}) {
					t.Fatalf("标失败透传的原因 = %v", tc.stub.gotReason)
				}
				return
			}
			var ae *oao.ActionError
			if !errors.As(err, &ae) {
				t.Fatalf("err = %v (%T), want *oao.ActionError", err, err)
			}
			if ae.Status != tc.want {
				t.Fatalf("status = %d, want %d (msg=%q)", ae.Status, tc.want, ae.Message)
			}
			if ae.Message == "" {
				t.Error("业务错误应带提示语")
			}
			// 入参不合法的几条在解析阶段就返回了，不该走到后端
			if tc.want == http.StatusBadRequest && len(tc.stub.gotID) != 0 {
				t.Errorf("入参非法时不该调用后端：%v", tc.stub.gotID)
			}
		})
	}
}

// 非哨兵错误原样返回，由组件统一按 500 处理（细节不进前端）。
func TestActionHandlerPassesUnknownError(t *testing.T) {
	boom := errors.New("db down")
	err := handlerFor(t, &stubActions{retryErr: boom}, "retry")(context.Background(), oao.ActionRequest{
		ID: "1", Row: map[string]any{"reprocess": "1"},
	})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the original error", err)
	}
}

func handlerFor(t *testing.T, acts TaskActions, key string) oao.ActionHandler {
	t.Helper()
	for _, a := range taskActions(acts) {
		if a.Key == key {
			return a.Handler
		}
	}
	t.Fatalf("action %q not declared", key)
	return nil
}
