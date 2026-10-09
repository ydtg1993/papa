package core

// Site 站点声明的快照：业务侧取用（`engine.Site(task.Site)` → BaseURL 等）。
//
// 框架**只把它当标签**：熔断按 Key 分组、日志/监控/任务表按 Key 分维度；
// 它**不**拿 BaseURL 做域名校验、也不替谁补相对 URL —— 那些是业务站点包的事
// （各站点包自己 AbsURL / Hosts 校验）。
type Site struct {
	Key     string `json:"key"`
	BaseURL string `json:"base_url"`
	// Entries 本站的入口路由表：key → 相对 BaseURL 的路径（或绝对地址），如
	// `SubmitEntries(engine, site)` 拿它投入口任务，handler 拿它反推任务的 key。
	Entries map[string]string `json:"entries,omitempty"`
	// Headers 本站的请求头（UA / Referer / Cookie / Accept-Language…）。
	// 引擎在跑这个站的任务时自动挂到 ctx 上（静态抓取与浏览器渲染都带上），
	// handler 里还能用 `papa.WithHeaders(ctx, …)` 对单次请求再覆盖。
	// 空值表示删掉那个头（比如不想要框架默认的 User-Agent）。
	Headers map[string]string `json:"headers,omitempty"`
	// RestrictedKeywords 本站自己的"受限页"文案（追加到框架默认词表之后），
	// 交给 `htmlfetch.RestrictedReason(page, site.RestrictedKeywords...)` 用。
	RestrictedKeywords []string `json:"restricted_keywords,omitempty"`
}
