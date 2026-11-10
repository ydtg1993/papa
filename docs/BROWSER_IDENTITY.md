# 浏览器身份档案、绑定与本地诊断

Papa 使用 Go + Rod 管理稳定的浏览器身份。未绑定身份、未显式传入 Profile 时保留原来的 Device、Headers 和 Cookie 行为；创建档案本身不会启用或切换账号身份。没有全局 `stealth: true` 开关，也不依赖 Node 或外部指纹浏览器。

## 档案包含什么

`pkg/browser/identity.Profile` 是不可变版本的声明：站点、ID、版本、语言与 locale、时区、窗口和屏幕尺寸、DPR、触摸设置及可选适配器。`ua_policy` 首期只接受 `native-chromium`，UA 和完整 Client Hints 从实际 Chromium 和操作系统派生，保留真实原生 UA，包括浏览器原生的 HeadlessChrome 标记；不冒充其它浏览器内核或系统。

`ResolvedProfile` 是解析后的冻结快照。运行时与快照不兼容会报错，不在一次任务中静默换身份。普通 Header/RemoveHeaders、Device、账号 UserAgent、启动 UA/language 参数与身份字段冲突时在导航前拒绝；Authorization、其它业务头、Cookie 仍按原隔离规则使用。`ConfigurePage` 也执行同样的保护，不能中途换档案。

唯一可选适配器是 `{id:"webdriver", version:"1", enabled:true}`，只处理 `navigator.webdriver`。脚本嵌入 Go，注册与实际执行分别验证；不随机化字体、Canvas、WebGL 或硬件，不提供验证码服务或通过率承诺。未启用适配器时保留原生行为。

## 迁移和后台操作

升级后在业务项目显式执行 `go run . -migrate` 或 `papa migrate`。新增两个框架表始终登记，普通启动与查询不执行 DDL：

| 表 | 内容 |
| --- | --- |
| `crawler_browser_profiles` | 站点、档案 ID、不可变版本、声明 JSON、结构版本、创建时间 |
| `crawler_browser_identity_bindings` | 主体、独立修订号、活动/待生效引用及状态 |

两表不保存 Cookie、Token、代理凭据或诊断原始数据。凭据沿用账号 Store；加密文件仍需业务妥善保存密钥。历史档案只追加，首期不提供删除功能。

在「系统管理 → 浏览器诊断」操作：

1. 创建档案；修改时追加新版本。保存以预期最新版本做 CAS，冲突保留草稿，核对最新状态后再保存。
2. 设置绑定主体与目标档案，保存为待生效。主体为 `account` 或 `session`，只能引用同站点的档案。
3. 点击「复验并生效」。等待该主体旧使用自然结束，再以新身份复验和发布；此等待不占引擎全局任务许可。
4. 失败保留待生效状态，新使用继续等待。可修正后复验，或取消改绑；新版本不会自动应用到其它账号。

账号池成员显示活动/待生效档案及本进程最近本地检查摘要，并可跳转到档案页。同账号跨池共用 `(site, account_id)` 绑定；池外 `Manager.Do` / `WithBrowser` 也遵守该绑定。身份修订独立于账号管理版本，不将普通档案保存当作账号失效。

`GET /api/browser-identities` 只读，不开浏览器、不登录、不诊断。鉴权的 POST 支持 `profile`、`stage`、`cancel`、`reverify`、`diagnose`；写操作使用 `expected`、操作审计和严格 JSON。`profile` 携带 `profile`；绑定操作携带 `subject:{site,kind,key}`、`reference:{site,id,version}`（stage）及绑定预期修订。诊断携带已保存的 `reference`，可选账号 `subject`。HTTP 超时或连接中断后先刷新核对，不自动重发写操作。

## 在途任务和凭据

引擎在任务认领和全局放行前固定账号身份，任务结束释放引用；同一任务中后续页面沿用同一快照。显式账号 API 的每次调用也固定身份。引用不占账号并发名额，不改变现有无 Close 契约的 `Pools.Acquire`。

复验采用独立候选认证状态。账号 Store 的可选 `pending_identity` 保存候选与身份元数据，原凭据字段保持到成功发布后的正常状态保存；绑定写入失败、结果不确定或进程重启时，按确切档案和运行环境元数据复核。候选不能被旧绑定加载；取消改绑可恢复原身份复验。首次启用旧无身份元数据的状态也必须复验。

真实账号过期、停用或人工验证仍按原规则处理。网站挑战、身份不兼容和绑定复验失败不会被当成账号失效自动换号。账号失效后的任务补偿仍由错误队列等既有机制承担。

关停时拒绝新身份操作、取消在途上下文，并限时等待作用域、复验和持久化收尾。公开运行时的 `Close` 发出取消，`Shutdown(ctx)` 返回是否排空；返回 false 时宿主应保留它仍需使用的资源。App 已接入该约束，未排空则跳过关闭数据库和浏览器池。

## 匿名会话由谁提供键

`session_key` 是业务提供并持久保存的非敏感字符串，例如会话记录主键或创建逻辑会话时生成的 UUID。同一站点、同一逻辑会话跨任务、重启、重建 `htmlfetch.Session` 时复用原键；新逻辑会话使用新键。不能把 URL、Cookie、Token 或它们的哈希作为键。

`htmlfetch.Session` 仍只是 CookieJar，不新增或自动生成 ID。`htmlfetch.WithSession` 只注入 CookieJar；身份键不自动恢复 Cookie，Cookie 保存恢复仍由原有业务/凭据存储负责。缺少会话键时持久绑定作用域报错；新未绑定键不继承旧键的身份。

业务在 App 运行后使用 `Engine.WithSessionIdentity`，或 `App.BrowserIdentities().WithSession`。回调中的已解析快照需显式传入低层页面选项：

```go
err := engine.WithSessionIdentity(ctx, "fixture", stableBusinessSessionKey,
    func(ctx context.Context, frozen *identity.ResolvedProfile) error {
        return engine.GetBrowserPool().WithBrowser(ctx, func(b *browser.Browser) error {
            return b.WithNavigatedPage(ctx, targetURL,
                browser.PageOptions{Identity: frozen},
                func(page *rod.Page) error {
                    // 解析本次页面。
                    return nil
                })
        })
    })
```

上述代码需导入 `context`、`github.com/go-rod/rod`、`pkg/browser`、`pkg/browser/identity`。账号受管入口自动传入快照。显式 `BrowserIdentities().WithProfile` 是另一条单次操作路径，回调同样获得快照，但不创建 `session` 绑定，不作为绑定失败的自动回退。低层 `identity/runtime.NewManager` 可配置 Store、Resolve 和 Verify 供独立库使用；构造不迁移。

## 两类诊断

「启动诊断」保留原有 Chromium 启动、Rod 连接和空白页检查。「配置一致性检查」使用与采集相同的受管页面工厂，在独立临时环境内访问随机回环地址的合成页。该实例只能访问这个 origin 和服务的随机路径；不扩大生产出口授权，不调用登录，不载入账号 Cookie/Token/localStorage，不影响代理健康。

公共检查复制公共池的允许环境项；从账号池进入或选择账号后，复制该账号专用池的二进制、允许的环境参数及身份覆盖约束。被省略的配置、代理和认证条件列为未验证，不能据此推断目标站通过率。生产中会冲突的 UA、设备或受保护头不会因诊断去敏而被忽略。

报告同时展示声明、解析快照、首个请求的身份头、页面第一段同步脚本、加载后结果、浏览器/检查器版本及差异。高熵 CH 不可用时明确标注；品牌 GREASE 和顺序差异不算不一致。字体仅探测固定有限样本；Canvas/WebGL 不可用时报告不可用。环境变化与配置差异分开显示。

本进程仅保留最近 128 个摘要，重启清空；完整报告可复制。浏览器/系统版本变化需重新诊断，不承诺跨环境指纹字节不变。

配置一致性检查使用有超时和进程清理边界的独立启动器，正常采集池的启动方式不变。首期诊断不支持 `leakless: true`，会明确报不支持，不会悄悄更改该配置。

## 网站挑战与覆盖边界

`core.ErrSiteChallenge`（门面 `papa.ErrSiteChallenge`）和 `SiteChallengeError` 的类别是 `site_challenge`，`Retryable()==false`。明确的 `cf-mitigated: challenge` 响应在 200/401/403 成功或账号判定前处理；普通状态码、普通 HTML 和验证码字样不会自动升级成网站挑战。文本启发式仍由业务调用原有 RestrictedReason/RestrictedError。

浏览器在导航前观察主文档及当前捕获操作选中的响应；加载/选择器超时不覆盖已观察到的挑战。无关第三方资源不会让主任务失败。`CaptureRequest` 默认仍可在发出请求时返回，不能排除尚未收到的响应挑战；`CaptureResult.ResponseObserved` 与引擎 trace 的 `response_observed` 表示是否观察到主文档响应，不保证捕获请求的响应已到达。

网站挑战停止当前执行内立即重试，不修改账号选择/管理版本、不清除有效凭据、不计为代理本身故障；429/Retry-After 沿用站点治理。错误队列仍可能按 failed 状态及补偿上限在以后重投。

首期保证受管 page target 主文档。弹窗、跨进程 iframe、独立 Worker、业务原始 Rod/CDP 修改不在自动保证范围，同目标 frame 也需独立验证。HTTP 身份头不自动联动，Go net/http 的 TLS/HTTP2 指纹不模拟；没有 uTLS、自动验证码或特定站点规则。页面去重仍按原有站点/规范 URL，不加入账号或 Profile 维度。
