package app

import (
	"net/http"
	"slices"
	"strings"
)

// Middleware 一个 HTTP 中间件：把 next 包一层。与后台鉴权（server.Monitor.Auth）同一个签名，
// 业务中间件和框架中间件可以混用、串成一条链。
type Middleware func(http.Handler) http.Handler

// Router 业务挂在监控后台上的路由表。
//
// 它只是 http.ServeMux 的一层薄封装：除了路径前缀（Group）与中间件链，没有自己的路由匹配逻辑 ——
// pattern 用的就是 Go 1.22 的 ServeMux 语法（"GET /api/x"、"POST /api/x"、"/x/" 等）。
//
// 「后台鉴权」由框架注入（guard），业务路由**默认**和后台内置接口一样过白名单 + 令牌；
// 要对外公开的路由（webhook / OAuth 回调）用 NoAuth() 显式声明。
type Router struct {
	mux    *http.ServeMux
	guard  Middleware   // 后台「白名单 + 令牌」校验；NoAuth() 出来的组为 nil
	prefix string       // 路径前缀，随 Group 累加
	mws    []Middleware // 业务中间件，对「之后注册」的路由生效
}

func newRouter(mux *http.ServeMux, guard Middleware) *Router {
	return &Router{mux: mux, guard: guard}
}

// Use 追加中间件。只对**之后**注册的路由生效（与 net/http 生态的惯例一致）。
// 执行顺序是注册顺序的正序：Use(A); Use(B) → 请求先过 A 再过 B。
func (r *Router) Use(mw ...Middleware) {
	r.mws = append(r.mws, mw...)
}

// Group 开一个子路由组：继承父组已注册的中间件，路径前缀相加。
// 在子组上再 Use 只影响子组（及其后续的分组），不动父组。
func (r *Router) Group(prefix string, fn func(g *Router)) {
	g := &Router{
		mux:    r.mux,
		guard:  r.guard,
		prefix: r.prefix + prefix,
	}
	g.mws = append(g.mws, r.mws...)
	fn(g)
}

// NoAuth 返回一个**不带后台鉴权**的路由组视图：在其上注册的路由对白名单外、没带令牌的来源也开放。
//
// 只给必须由外部调用的接口用（webhook、OAuth 回调、健康检查）。它是有意为之的逃生舱 ——
// 默认松掉鉴权才是危险的，所以这里要求显式写出来。返回的是副本，不影响原组。
func (r *Router) NoAuth() *Router {
	cp := *r
	cp.guard = nil
	return &cp
}

// Handle 注册一条路由。pattern 可以是裸路径，也可以带方法（"GET /api/x"）。
func (r *Router) Handle(pattern string, h http.Handler) {
	r.mux.Handle(r.join(pattern), r.chain(h))
}

// Get / Post / Put / Delete 是 Handle 的常用方法快捷方式。
func (r *Router) Get(pattern string, h http.HandlerFunc) {
	r.Handle("GET "+pattern, h)
}

func (r *Router) Post(pattern string, h http.HandlerFunc) {
	r.Handle("POST "+pattern, h)
}

func (r *Router) Put(pattern string, h http.HandlerFunc) {
	r.Handle("PUT "+pattern, h)
}

func (r *Router) Delete(pattern string, h http.HandlerFunc) {
	r.Handle("DELETE "+pattern, h)
}

// join 把当前前缀拼进 pattern。带方法的写法（"GET /api/x"）里方法必须在最前，
// 前缀要插在方法和方法后面的路径之间，不能直接首尾相接。
func (r *Router) join(pattern string) string {
	method, path, ok := strings.Cut(pattern, " ")
	if !ok {
		return r.prefix + pattern
	}
	return method + " " + r.prefix + path
}

// chain 把业务中间件与后台鉴权串成一条链。
//
// 中间件按注册顺序**逆序**套（最后注册的在最内层），于是请求侧是正序；
// guard 套在**最外层** —— 白名单/令牌没过的请求根本不会进业务中间件，
// 免得带副作用的中间件（写日志、计数、限流）去记录未鉴权的流量。
func (r *Router) chain(h http.Handler) http.Handler {
	for _, mw := range slices.Backward(r.mws) {
		h = mw(h)
	}
	if r.guard != nil {
		h = r.guard(h)
	}
	return h
}
