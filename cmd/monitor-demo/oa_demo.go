package main

// 后台预览用的假数据（不接 MySQL）：把这几轮新做的东西都摆出来看效果。
//
// 真实现分别在：访问令牌 admin/tokenadmin（内存版见 token_demo.go）、
// 操作日志 admin/oplog、动作接线 admin/tasksource；
// 这里只是内存数据替身，**别当参考实现看**。

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/ydtg1993/oao"
	"github.com/ydtg1993/papa/v2/admin/auth"
	"github.com/ydtg1993/papa/v2/admin/server"
)

// 演示用的动作：点了就成功，不碰任何数据。
func demoAction(label string) oao.ActionHandler {
	return func(context.Context, oao.ActionRequest) error {
		fmt.Printf("[demo] 动作 %s 被触发（演示数据，不做任何事）\n", label)
		return nil
	}
}

// fakeOrderTable 4 个动作 —— 行内只平铺前两个，其余自动收进「更多 ▾」。
func fakeOrderTable() oao.Table {
	statusEnum := map[string]string{"1": "待审", "2": "通过", "3": "驳回"}
	statusTone := map[string]string{"1": "warn", "2": "ok", "3": "err"}
	rows := make([]map[string]any, 0, 40)
	for i := 1; i <= 40; i++ {
		rows = append(rows, map[string]any{
			"id":         i,
			"order_no":   fmt.Sprintf("ORD-2026-%04d", i),
			"status":     i%3 + 1,
			"amount":     float64(i) * 12.5,
			"note":       "第 " + fmt.Sprint(i) + " 条：用来验证只读输入框",
			"created_at": time.Now().Add(-time.Duration(i) * time.Hour),
		})
	}
	return oao.Table{
		Key: "demo_order", Label: "订单（4 个动作）", Group: "演示",
		Source: fakeRowsSource{rows: rows, delay: 120 * time.Millisecond},
		Columns: []oao.Column{
			{Field: "id", Label: "ID", Kind: oao.KindNumber, Width: "70px", NoEdit: true},
			{Field: "order_no", Label: "订单号"},
			{Field: "status", Label: "状态", Kind: oao.KindNumber, Render: oao.RenderEnum,
				Enum: statusEnum, Tone: statusTone},
			{Field: "amount", Label: "金额", Kind: oao.KindNumber},
			{Field: "note", Label: "备注", Render: oao.RenderInput, MaxLen: 30},
			{Field: "created_at", Label: "创建时间", Kind: oao.KindTime, NoEdit: true},
		},
		Filters: []oao.Filter{
			{Field: "order_no", Label: "订单号", Op: oao.OpLike},
			{Field: "status", Label: "状态", Kind: oao.KindNumber, Op: oao.OpIn, Options: statusEnum},
		},
		DefaultSort: "-id",
		Actions: []oao.Action{
			{Key: "approve", Label: "通过", Tone: oao.ToneOK, Confirm: "确认通过？", Handler: demoAction("通过")},
			{Key: "reject", Label: "驳回", Tone: oao.ToneWarn, Handler: demoAction("驳回")},
			// 后两个会被收进「更多 ▾」
			{Key: "archive", Label: "归档", Handler: demoAction("归档")},
			oao.RemoveAction(demoAction("删除")),
		},
	}
}

// fakeOplogTable 「操作日志」页，重点看「操作人」列（真实现 admin/oplog.Table）。
func fakeOplogTable() oao.Table {
	ops := []string{"张三", "李四", "王五", ""}
	actions := []string{"retry", "fail", "remove", "disable"}
	rows := make([]map[string]any, 0, 30)
	for i := 1; i <= 30; i++ {
		ok := i%4 != 0
		errMsg := ""
		if !ok {
			errMsg = "该行已被他人修改，请刷新后重试"
		}
		rows = append(rows, map[string]any{
			"id":         i,
			"created_at": time.Now().Add(-time.Duration(i) * 7 * time.Minute),
			"table":      []string{"task", "access_token"}[i%2],
			"action":     actions[i%len(actions)],
			"row_id":     fmt.Sprint(i * 3),
			"operator":   ops[i%len(ops)], // 空的是"老数据"（还没有令牌的年代）
			"ok":         ok,
			"error":      errMsg,
			"ip":         "127.0.0.1",
		})
	}
	return oao.Table{
		Key: "operation_log", Label: "操作日志", Group: "演示",
		Source: fakeRowsSource{rows: rows},
		Columns: []oao.Column{
			{Field: "id", Kind: oao.KindNumber, Width: "70px"},
			{Field: "created_at", Label: "时间", Kind: oao.KindTime, Width: "170px"},
			{Field: "table", Label: "表格"},
			{Field: "action", Label: "动作"},
			{Field: "row_id", Label: "行 ID", Width: "90px"},
			{Field: "operator", Label: "操作人", Width: "110px"},
			{Field: "ok", Label: "结果", Kind: oao.KindBool, Render: oao.RenderEnum,
				Enum: map[string]string{"true": "成功", "false": "失败"},
				Tone: map[string]string{"true": "ok", "false": "err"}},
			{Field: "error", Label: "失败原因", Render: oao.RenderInput, MaxLen: 40},
			{Field: "ip", Label: "来源 IP", Width: "130px"},
		},
		Filters: []oao.Filter{
			{Field: "table", Label: "表格"},
			{Field: "action", Label: "动作"},
			{Field: "operator", Label: "操作人", Op: oao.OpLike},
			{Field: "ok", Label: "结果", Kind: oao.KindBool, Op: oao.OpIn,
				Options: map[string]string{"true": "成功", "false": "失败"}},
			{Field: "created_at", Label: "时间", Kind: oao.KindTime, Op: oao.OpBetween},
		},
		DefaultSort: "-id",
	}
}

// 演示的自定义页与注入内容 —— 真实现是 app.UsePage / UseScript / UseCSS
// （它们把内容拼进 /static/custom.js|custom.css 并给 /api/pages 提供清单）。
const demoPageScript = `
Papa.page('review', function (el, meta) {
  el.innerHTML = '<h2 class="demo-title">' + esc(meta.label) + '（自定义页）</h2>'
    + '<p class="demo-hint">这一页由 app.UsePage 的 Script 渲染，样式来自 app.UseCSS（演示数据）。</p>'
    + '<div class="demo-card"><div id="demo-who" class="demo-hint">当前用户：读取中…</div>'
    + '<p>访问令牌存在 crawler_access_token 表里，每条属于一个操作人；'
    + '审计日志因此能记下"谁干的"（去「演示 → 操作日志」看「操作人」列）。</p></div>';
  apiFetch('/api/demo/whoami')
    .then(function (r) { return r.json(); })
    .then(function (d) {
      document.getElementById('demo-who').textContent = '当前用户：' + (d.operator || '（未识别）');
    })
    .catch(function () {});
});
`

const demoPageCSS = `
.demo-title { color: var(--primary); }
.demo-hint { color: var(--muted-foreground); font-size: 13px; }
.demo-card { border: 2px solid var(--border-color); background: var(--card);
  box-shadow: 3px 3px 0 0 var(--shadow-color); padding: 14px 16px; margin-top: 12px; }
`

// demoCustomPage 挂上自定义页需要的三条路由（与 internal/app 的 mountCustomRoutes 同构）。
func demoCustomPage(mux *http.ServeMux, mon *server.Monitor) {
	pages := []map[string]string{{"key": "review", "label": "审核", "group": "演示"}}

	mux.Handle("/static/custom.js", server.NoCache(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		_, _ = w.Write([]byte(demoPageScript))
	})))
	mux.Handle("/static/custom.css", server.NoCache(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
		_, _ = w.Write([]byte(demoPageCSS))
	})))
	mux.Handle("/api/pages", mon.Auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write([]byte(`{"pages":[`))
		for i, p := range pages {
			if i > 0 {
				_, _ = w.Write([]byte(","))
			}
			_, _ = fmt.Fprintf(w, `{"key":%q,"label":%q,"group":%q}`, p["key"], p["label"], p["group"])
		}
		_, _ = w.Write([]byte(`]}`))
	})))

	// 「当前用户」由鉴权中间件写进请求上下文的操作人提供（真实现里 oao 的 OnAction 也这么取，用于审计）
	mux.Handle("/api/demo/whoami", mon.Auth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = fmt.Fprintf(w, `{"operator":%q}`, auth.OperatorFrom(r.Context()))
	})))
}
