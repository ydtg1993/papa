package app

import (
	"github.com/ydtg1993/oao"
	"github.com/ydtg1993/papa/v4/admin/auth"
	"github.com/ydtg1993/papa/v4/admin/oplog"
	"github.com/ydtg1993/papa/v4/admin/tokenadmin"
	"net/http"
	"strconv"
	"time"
)

// tokenWriteHook 接「访问令牌」页的写操作：成功后让令牌数量缓存失效，同时按开关记审计。
//
// 成功写入后更新管理计数；失败事件不失效（数据没变）。鉴权独立逐请求查库，不依赖此 hook。
//
// 抽成独立函数是为了能离线断言"写成功确实 Invalidate"——它的调用点在 httpServer 里，
// 那一步要真库真端口。
func tokenWriteHook(counter *auth.TokenCounter, rec *oplog.Recorder) tokenadmin.Hook {
	return func(ev tokenadmin.Event) {
		if ev.Err == nil {
			counter.Invalidate()
		}
		if rec == nil {
			return
		}
		rec.RecordEvent(oplog.Event{
			Table: ev.Table, Action: ev.Action,
			RowID:    strconv.FormatUint(uint64(ev.ID), 10),
			Values:   ev.Values,
			Err:      ev.Err,
			IP:       ev.IP,
			Operator: auth.OperatorFrom(ev.Req.Context()),
			At:       ev.At,
		})
	}
}

// operatorOf 从操作事件带的请求里取「操作人」：身份是鉴权中间件（admin/auth）解析访问令牌后
// 写进请求上下文的。宿主自己构造事件时 Req 可能为 nil —— 那就记空，不影响审计写库。
func operatorOf(ev oao.ActionEvent) string {
	if ev.Req == nil {
		return ""
	}
	return auth.OperatorFrom(ev.Req.Context())
}

func (a *App) runtimeAudit(r *http.Request, table, action, id string, err error) {
	if a.oplog != nil {
		a.oplog.RecordEvent(oplog.Event{Table: table, Action: action, RowID: id, Operator: auth.OperatorFrom(r.Context()), IP: r.RemoteAddr, At: time.Now(), Err: err})
	}
}
