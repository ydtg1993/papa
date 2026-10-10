package sitesource

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/ydtg1993/oao"
	"github.com/ydtg1993/papa/v3/engine"
)

// stubActions 记录调用并按预设返回错误，用来在没有数据库时测处理函数。
type stubActions struct {
	forceErr error
	autoErr  error
	gotSite  []string
	gotOn    []bool
}

func (s *stubActions) ForceRepollSiteRepeatableTasks(site string) (int, error) {
	s.gotSite = append(s.gotSite, site)
	return 3, s.forceErr
}

func (s *stubActions) SetSiteAutoRepeat(site string, on bool) error {
	s.gotSite = append(s.gotSite, site)
	s.gotOn = append(s.gotOn, on)
	return s.autoErr
}

// buildTable 走一遍组件注册，顺带验证声明能被 oao 校验通过。
func buildTable(t *testing.T, acts SiteActions) *oao.TableInfo {
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

func handlerFor(t *testing.T, acts SiteActions, key string) oao.ActionHandler {
	t.Helper()
	for _, a := range siteActions(acts) {
		if a.Key == key {
			return a.Handler
		}
	}
	t.Fatalf("action %q not declared", key)
	return nil
}

// 声明：主键是站点键、列名是库里的真实列名、三个动作各带二次确认。
func TestTableDeclaration(t *testing.T) {
	info := buildTable(t, &stubActions{})

	if info.Key != "site" || info.Group != "数据" {
		t.Fatalf("key/group = %q/%q", info.Key, info.Group)
	}
	if info.IDField != "key" {
		t.Fatalf("主键字段应当是 key（动作要的就是站点键）：%q", info.IDField)
	}

	byKey := make(map[string]oao.ActionInfo, len(info.Actions))
	for _, a := range info.Actions {
		byKey[a.Key] = a
	}
	for _, want := range []struct {
		key, label string
		tone       oao.Tone
	}{
		{"repeat_now", "轮询任务", oao.ToneInfo},
		{"repeat_pause", "暂停自动轮询", oao.ToneInfo},
		{"repeat_resume", "恢复自动轮询", oao.ToneInfo},
	} {
		a, ok := byKey[want.key]
		if !ok {
			t.Fatalf("action %q missing: %+v", want.key, info.Actions)
		}
		if a.Label != want.label || a.Tone != want.tone {
			t.Errorf("action %q = %q/%q, want %q/%q", want.key, a.Label, a.Tone, want.label, want.tone)
		}
		if a.Confirm == "" {
			t.Errorf("action %q 应有二次确认（三个动作都会真的改东西）", want.key)
		}
	}

	// 列名必须是库里的真实列名（行数据的键来自 SELECT *）：挑几个易错的一一钉住
	cols := make(map[string]oao.ColumnInfo, len(info.Columns))
	for _, c := range info.Columns {
		cols[c.Name] = c
	}
	if _, ok := cols["key"]; !ok {
		t.Errorf("缺主键列 key：%+v", info.Columns)
	}
	if c, ok := cols["auto_repeat"]; !ok || c.Kind != oao.KindBool {
		t.Errorf("auto_repeat 列应为 KindBool: %+v", cols["auto_repeat"])
	}
	for _, f := range []string{"last_repeat_at", "breaker_paused_at", "updated_at"} {
		if c, ok := cols[f]; !ok || c.Kind != oao.KindTime {
			t.Errorf("%s 列应为 KindTime: %+v", f, cols[f])
		}
	}
	if c, ok := cols["base_url"]; !ok || c.Render != oao.RenderLink {
		t.Errorf("base_url 应当是可以点开的链接：%+v", cols["base_url"])
	}
}

// acts 为 nil 时退化为只读：不注册任何写路由。
func TestTableReadOnlyWithoutActions(t *testing.T) {
	if info := buildTable(t, nil); len(info.Actions) != 0 {
		t.Fatalf("actions = %+v, want none", info.Actions)
	}
}

// 三个动作：站点键原样透传、方向写死（暂停 = false / 恢复 = true）、哨兵错误映射成状态码。
func TestActionHandlers(t *testing.T) {
	cases := []struct {
		name   string
		stub   *stubActions
		action string
		id     string
		want   int
	}{
		{"轮询任务成功", &stubActions{}, "repeat_now", "huangguo", 0},
		{"轮询任务：站点不存在", &stubActions{forceErr: engine.ErrUnknownSite}, "repeat_now", "ghost", http.StatusNotFound},
		{"暂停自动轮询成功", &stubActions{}, "repeat_pause", "huangguo", 0},
		{"暂停：本来就没开", &stubActions{autoErr: engine.ErrSiteAlreadyManual}, "repeat_pause", "huangguo", http.StatusConflict},
		{"恢复自动轮询成功", &stubActions{}, "repeat_resume", "huangguo", 0},
		{"恢复：已经在自动", &stubActions{autoErr: engine.ErrSiteAlreadyAuto}, "repeat_resume", "huangguo", http.StatusConflict},
		{"默认 scope 不能改", &stubActions{autoErr: engine.ErrDefaultScopeNoAuto}, "repeat_pause", "", http.StatusBadRequest},
		{"并发改动", &stubActions{autoErr: engine.ErrTaskChanged}, "repeat_pause", "huangguo", http.StatusConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := handlerFor(t, tc.stub, tc.action)(context.Background(), oao.ActionRequest{
				Table: "site", Action: tc.action, ID: tc.id,
			})
			if tc.want == 0 {
				if err != nil {
					t.Fatalf("err = %v, want nil", err)
				}
				if len(tc.stub.gotSite) != 1 || tc.stub.gotSite[0] != tc.id {
					t.Fatalf("透传的站点 = %v, want [%q]", tc.stub.gotSite, tc.id)
				}
				switch tc.action {
				case "repeat_pause":
					if len(tc.stub.gotOn) != 1 || tc.stub.gotOn[0] {
						t.Fatalf("「暂停」的方向应当写死成 false，实得 %v", tc.stub.gotOn)
					}
				case "repeat_resume":
					if len(tc.stub.gotOn) != 1 || !tc.stub.gotOn[0] {
						t.Fatalf("「恢复」的方向应当写死成 true，实得 %v", tc.stub.gotOn)
					}
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
		})
	}
}

// 非哨兵错误原样返回，由组件统一按 500 处理。
func TestActionHandlerPassesUnknownError(t *testing.T) {
	boom := errors.New("db down")
	err := handlerFor(t, &stubActions{forceErr: boom}, "repeat_now")(context.Background(), oao.ActionRequest{ID: "a"})
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the original error", err)
	}
}
