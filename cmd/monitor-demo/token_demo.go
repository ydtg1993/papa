package main

// 后台「访问令牌」页的演示：真实现是 internal/tokenadmin 的 DBStore（crawler_access_token 表）
// + internal/auth 的库表校验；这里换成内存存储，好让不接 MySQL 也能把整页走通。
//
// 关键一点：**登录校验也查这个 store** —— 所以在后台新建一把令牌后，可以当场拿它登录，
// 而不是只能看着列表变化。

import (
	"fmt"
	"log"
	"net/http"

	"github.com/ydtg1993/papa/v2/internal/auth"
	"github.com/ydtg1993/papa/v2/internal/server"
	"github.com/ydtg1993/papa/v2/internal/tokenadmin"
)

// demoToken 演示用的固定令牌；登录框里填它（后台新建的令牌同样能登录）。
const demoToken = "demo-token"

type demoTokens struct{ store *tokenadmin.MemStore }

// newDemoTokens 造三条演示令牌：一条能用（就是 demoToken）、一条停用的。
func newDemoTokens() *demoTokens {
	s := tokenadmin.NewMemStore()
	s.Seed(demoToken, "张三", "运维机")
	s.Seed("demo-token-li", "李四", "笔记本")
	wangwu := s.Seed("demo-token-wang", "王五", "已离职")
	_ = s.SetEnabled(wangwu, false) // 演示「已停用」状态
	return &demoTokens{store: s}
}

// verify 顶替真实现里的库表校验（internal/auth.Verify），查的是内存里的令牌。
func (d *demoTokens) verify(r *http.Request) (string, bool) {
	return d.store.Verify(auth.Extract(r))
}

// register 挂上「访问令牌」页的三条接口 —— 与 internal/app 的装配方式一致，
// 只是写操作不写库、改成打一行日志冒充"操作日志入库"。
func (d *demoTokens) register(mux *http.ServeMux, mon *server.Monitor) {
	tokenadmin.NewAPI(d.store, func(ev tokenadmin.Event) {
		log.Printf("[demo] 审计入库：%s/%s id=%d 操作人=%s 提交值=%v 结果=%s",
			ev.Table, ev.Action, ev.ID, auth.OperatorFrom(ev.Req.Context()), ev.Values, result(ev.Err))
	}, stdLogger{}).Register(mux, mon.Auth)
}

func result(err error) string {
	if err == nil {
		return "成功"
	}
	return fmt.Sprintf("失败(%s)", err.Error())
}
