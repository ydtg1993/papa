# 账号登录态与采集运行

> 面向：需要按账号登录、跨 HTTP/浏览器采集和恢复会话的业务。配套：[Fetcher 手册](FETCHER_WRITING_GUIDE.md)、[采集工具](CRAWL_TOOLKIT.md)、[规则验收](CRAWL_RULES.md)。

## 1. 入口与边界

`pkg/account.Manager` 是显式注册的单进程账号管理器，以 `account.Key{Site, ID}` 定位账号。HTTP/API 和 HTML 使用同一 CookieJar、Token 与 CSRF 头；Rod 使用同账号的隔离浏览器状态。它不自动扫描账号、不改任务表，也不启动后台刷新定时器。

| 入口 | 用途 |
| --- | --- |
| `account.NewManager(client)` / `papa.NewAccountManager(client)` | 创建注册表；HTTP client 可由 Engine.GetHTMLClient 提供 |
| `Register(key, Config)` | 注册站点＋账号，一次注册后配置不再外部修改 |
| `Manager.Do` / `Engine.FetchAccountHTTP` | 有限认证恢复的 HTTP 请求 |
| `Manager.FetchHTML` / `Engine.FetchAccountHTML` | 同账号抓 HTML，不进入旧原始 HTML 归档 |
| `Manager.WithBrowser` / `Engine.WithAccountBrowser` | 恢复、检查、必要时登录，执行一次浏览器回调并保存状态 |
| `Pause` / `Resume` / `Logout` | 停止新操作、恢复并重新检查、清除本地状态并暂停 |
| `Status` / `Engine.AccountStatus` | 无凭据状态列表，可接业务自定义后台 |
| `NewFileStore` | 绑定账号身份的加密本地存储 |

旧 `FetchHTTP` / `FetchHTML` / `WithIsolatedBrowser` / `SessionGuard` 行为仍可使用，**不会因为注册了管理器就自动选账号**。任务可在既有 `Task.Meta["account_id"]` 保存非敏感账号标识，执行时用 `task.Site` 和该标识构造 Key；用户名密码、Cookie、Token 不放进任务 Meta。

## 2. HTTP 登录、检查与使用

初始化时创建一次管理器和账号配置。`Origins` 为允许的 HTTP origin（协议＋主机＋有效端口，不带路径或查询），业务请求只允许这些 origin，且不自动跟随重定向。

```go
client := engine.GetHTMLClient() // 必须已启用 html.enable
accounts := account.NewManager(client)
key := account.Key{Site: "books", ID: "reader-01"}
err := accounts.Register(key, account.Config{
    Origins: []string{"https://books.example"},
    CheckInterval: time.Minute,
    Timeout: 30 * time.Second,
    FailureCooldown: 5 * time.Second,
    MaxConcurrent: 1,
    Login: loginAccount,
    Check: checkAccount,
})
if err != nil { return err }
engine.SetAccountManager(accounts)

response, err := engine.FetchAccountHTTP(ctx, key, papa.HTTPRequest{
    URL: "https://books.example/api/items",
})
if err != nil { return err }
page, err := engine.FetchAccountHTML(ctx, key, "https://books.example/list")
```

上例 `loginAccount` / `checkAccount` 是站点实现，签名分别为 `func(context.Context, *account.Credentials) error` 和 `func(context.Context, *account.Credentials) (bool, error)`。它们用 `credentials.Context(ctx)` 向自己的 HTTP client 发出登录/检查请求；该 context 绑定候选 CookieJar 和凭据头。Login 可设置 `credentials.Headers["Authorization"]`、`credentials.Headers["X-CSRF-Token"]`；refresh token 等私有状态放入 `Values`，它不会自动发送。协议和刷新接口由站点定义。

只允许 Authorization、X-CSRF-Token 和 `AuthHeaders` 中额外声明的凭据头。请求本身不能覆盖这些头或直接塞 Cookie。账号入口清除继承的这些凭据和 Proxy-Authorization，再绑定当前账号；共享配置中其他自定义认证头必须列入 AuthHeaders。`core.WithHeaders` 现在按 HTTP 规则执行大小写不敏感的覆盖/删除。

Check 必须验证真实账号身份。明确未登录返回 `false, nil`；网络错误返回 error，不能触发误登录。新候选始终使用新的 Jar，登录后复验通过才发布。每轮认证按账号合并并发；首调用者的取消会结束该轮，等待者可独立取消。回调必须配合 ctx，不能重入同一账号或保留候选继续修改。

CheckInterval 为 0 时逐次检查，正数允许近期有效结果复用；实际业务请求仍分类处理失效。MaxConcurrent 默认 1，设大后同账号 HTTP 可以并发，但站点必须允许同时操作和 Cookie 更新。需要严格 Token 顺序时保持 1。浏览器回调始终在账号内串行执行。

自动 Rod→HTTP 登录桥接与 `WithBrowser` 共用同一账号浏览器锁；桥接持锁直到 HTTP 复验、快照保存和状态发布完成。等待者拿锁后读取最新浏览器状态，避免重复登录或覆盖刚发布的状态。缓存命中和仅 HTTP 检查不占用浏览器锁，等待锁可以独立取消。

## 3. 过期、限制与 Token 轮换

默认只把完整 HTTP 401 判定为过期，403 判定为权限不足；网络/读取错误不自动当过期。站点若用 200 登录页或 JSON 业务码，可通过 `Config.Classify(response, err)` 返回 `Valid`、`Expired`、`Forbidden`、`Challenge` 或 `Unknown`。判断前应检查 err，不能将不完整正文识别为过期。

- Expired：只废弃实际使用的 HTTP 凭据版本；旧响应不能废弃刚刷新成功的版本。GET/HEAD 最多重新认证并再请求一次；第二次仍过期即返回 ErrExpired。
- POST/PUT/PATCH/DELETE：过期时记录失效并返回错误，不自动重发。业务需根据幂等键和远端执行结果决定后续操作。
- Forbidden：返回 ErrForbidden，不重新登录、不自动更换账号。
- Challenge：返回 ErrChallenge 并暂停账号；人工完成处理后显式 Resume。
- Unknown：保留底层请求错误语义，不增加重试。

配置可选 `Update(candidate, response)`，在成功响应后根据站点约定更新 Token/CSRF/refresh token。候选头校验通过才替换，发生变化时增加 HTTP 凭据版本。Update 必须快速、不重入管理器、不替换 CookieJar；并发时只接收当前版本的响应更新。普通 Set-Cookie 仍由 Jar 自动处理。浏览器状态变化不会单独增加 HTTP 凭据版本。

`Invalidate(key, version)` 适合已知版本的显式 HTTP 失效标记，未匹配版本返回 false。`Pause` 阻止新操作和过期认证结果发布，但不强杀已发送请求；暂停标志是进程内状态，长期停用应由应用账号配置控制。`Logout(ctx,key)` 清除内存凭据、删除存储并暂停；它不是网站远端退出登录 API。删除失败会返回错误，需处理后再恢复，以免旧文件仍留在磁盘。

## 4. 持久化与重启

```go
store, err := account.NewFileStore(privateDirectory, key32Bytes)
if err != nil { return err }
// 将 store 放到每个需要恢复的账号 Config.Store。
```

FileStore 使用外部 32 字节密钥、随机 nonce 和 AES-GCM；账号 Key 参与认证，文件名使用账号键哈希。文件内容包含 Cookie、凭据头、Values 和可选浏览器状态，上限 16 MiB。密钥不写入文件。先完成加密，再写临时文件、Sync、Rename；崩溃持久性和原子替换保证依赖具体操作系统/文件系统。目录由单进程拥有，Windows ACL 和密钥保管由应用负责。

首次使用从存储读取；恢复的 HTTP 会话必须重新 Check，不能直接使用之前的检查时间。Cookie 快照保存完整属性，MaxAge 转为接收时的绝对到期时间，避免重启续期；保留 host-only/domain/path/Secure 等语义，不从请求 Cookie 反推范围。恢复限制 4096 个 Cookie、4 MiB Cookie 字符串数据；不支持分区 Cookie，保存前检查是否能恢复。Session Cookie 会随快照恢复，但服务端仍可随时撤销。

保存/解密/认证失败明确返回错误，不静默切到匿名采集。HTTP 登录轮次保存失败不发布新会话。业务请求已经发生的远端状态与 Cookie/Token 更新无法靠文件失败回滚，调用方必须处理返回错误。普通内存模式不持久化。

启用 Store 后，认证发布和普通业务响应会保存快照；FileStore 包含同步磁盘 I/O。高频采集可提供自己的 Store，并明确缓冲、落盘和故障恢复语义，框架不会默认异步忽略保存失败。

低层 API 为 `Session.Snapshot() SessionState` 与 `htmlfetch.RestoreSession(state)`。SessionState 是明文凭据对象，不属于可发布的脱敏样本。

## 5. Rod 账号流程与 HTTP 桥接

Config.Browser 配置账号专用或明确共享策略的 Pool、URL、Login、Check。URL 是稳定的同源 bootstrap 页面。Manager.WithBrowser 为每次操作创建新隔离上下文，恢复快照、检查登录、必要时在新的干净上下文登录，然后执行业务回调一次；成功后保存当前页面 Cookie/LocalStorage。失败的业务回调不会自动重放。

`Browser.WithStatePage` 支持 Cookie 在首次导航前设置、LocalStorage 在主 frame 第一段脚本前恢复。初始化完成通过独立执行环境中的 CDP 握手确认，先卸载初始化脚本再继续网站脚本；网站清空 sessionStorage 不影响确认，立即重定向也不会重新覆盖新 token。握手监听和 Debugger 在交互及业务回调前关闭。nil state 也检查最终 origin，跨源重定向不能进入账号登录回调。Login、Check、Export 和业务回调返回时须回到 bootstrap origin；跨域 SSO 可由业务编排中间导航，但本工具不提供全浏览器的网络出口隔离。

只有 Browser 配置、没有 HTTP 回调的账号可以纯 Rod 使用。如果提供 HTTP Check 与 Browser，而不提供 HTTP Login，管理器会自动恢复并检查已有 Rod 状态，必要时登录，再按 `Browser.CookieURLs` 复制适用 Cookie 到候选 HTTP 会话（默认仅 Browser.URL）。`Browser.Export(ctx,page,credentials)` 可显式提取 LocalStorage Token 或 CSRF，随后 HTTP Check 必须复验桥接是否有效。恢复的浏览器状态通过检查但 HTTP 明确未登录时，额外尝试一次干净上下文登录；检查网络错误不当作过期，也不触发这次重登。

```go
err := engine.WithAccountBrowser(ctx, key, func(page *rod.Page) error {
    // page 已恢复并通过该账号的登录检查。
    // 在此完成导航、点击和提取；返回前回到配置的 bootstrap origin。
    return collectAccountPage(ctx, page)
})
```

HTTP 和浏览器后续凭据变化不自动双向同步；自动桥接仅发生在该 HTTP 账号需要登录时。账号快照包含当前 URL 及同源 `Browser.CookieURLs` 适用 Cookie 的并集、当前 origin LocalStorage；低层 `ExportPageState` 仍只导出当前 URL 适用 Cookie。快照不含 IndexedDB/sessionStorage/多 origin 状态。这里复用的是账号状态，不是长期占用池中的常驻 Chrome。回调结束即销毁上下文，页面不能带出回调。

HTTP 的 ProxyURL 固定该账号出口，空值固定直连；与 context 的显式代理冲突会报错。浏览器使用 Browser.Pool 的出口，Direct 可选直连池；HTTP ProxyURL 不会应用到 Rod。需要两端同 IP/设备时，应由应用配置相应专用池和 UA/设备，框架不会猜测或偷偷轮换。

## 6. 状态、归档和边界

Status 给出账号键、HTTP 版本、是否暂停、是否正在认证、近期检查/登录时间、冷却截止和失败类别。Ready 表示已有通过检查的本地状态，不保证服务端此刻有效。不会包含 Cookie、Token、Values、代理密码或回调原始错误。浏览器操作的时间范围和凭据可用性与 HTTP 分开记录。业务后台可用 `engine.AccountStatus()` 挂只读自定义数据接口；暂停/注销等写操作应沿用业务后台权限。

认证 HTTP/HTML 入口不进入旧原始 HTML 归档，避免登录后页面凭据随失败证据落盘。需要回归材料时显式使用 WriteSample 脱敏；浏览器截图和回调自行记录的数据仍由业务控制。Failure.Error 只返回类别，Unwrap 仍可供 errors.Is/As 检查，不能把解包的原始错误直接写日志。

本轮没有跨进程账号租约、自动验证码处理、自动任务账号分配、模型接入或规则自动发布；也不保证生产站点允许 Cookie 桥接。测试使用本地 HTTP 和真实 Chromium，站点登录协议与业务规则仍需自己的验收样本。
