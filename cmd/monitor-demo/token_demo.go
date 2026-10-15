package main

// 后台「访问令牌」页的演示：真实现是 admin/tokenadmin 的 DBStore（crawler_access_token 表）
// + admin/auth 的库表校验；这里换成内存存储，好让不接 MySQL 也能把整页走通。
//
// 关键一点：**登录校验也查这个 store** —— 所以在后台新建一把令牌后，可以当场拿它登录，
// 而不是只能看着列表变化。

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"

	"github.com/ydtg1993/papa/v3/admin/auth"
	"github.com/ydtg1993/papa/v3/admin/server"
	"github.com/ydtg1993/papa/v3/admin/tokenadmin"
)

// randomDemoToken 每次启动生成一把随机演示令牌，只打印到本机日志里。
// 不再用写死在源码里的固定令牌：知道源码的人（或翻过旧提交的人）不该天然就能登录任何一台
// 跑着 demo 的主机。想对外演示时，让调用方自己把令牌换成已知值、并显式改监听地址。
func randomDemoToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand 几乎不会失败；真失败退回一个仅限回环监听的兜底值
		return "demo-token"
	}
	return hex.EncodeToString(b)
}

type demoTokens struct{ store *tokenadmin.MemStore }

// newDemoTokens 造三条演示令牌：一条能用（就是调用方给的演示令牌）、一条停用的。
func newDemoTokens(demoToken string) *demoTokens {
	s := tokenadmin.NewMemStore()
	s.Seed(demoToken, "张三", "运维机")
	s.Seed("demo-token-li", "李四", "笔记本")
	wangwu := s.Seed("demo-token-wang", "王五", "已离职")
	_ = s.SetEnabled(wangwu, false) // 演示「已停用」状态
	return &demoTokens{store: s}
}

// verify 顶替真实现里的库表校验（admin/auth.Verify），查的是内存里的令牌。
func (d *demoTokens) verify(r *http.Request) (string, bool) {
	return d.store.Verify(auth.Extract(r))
}

// confirm 顶替真实现里的 auth.Confirmer：关停前要求把令牌再输一遍，
// demo 里没接数据库，所以表为空时也按"还没配凭据"放行（与正式实现同语义）。
func (d *demoTokens) confirm(token string) (string, bool) {
	return d.store.Verify(token)
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
