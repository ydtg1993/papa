# 账号登录态与采集运行

> 面向：需要按账号登录、跨 HTTP/浏览器采集和恢复会话的业务。配套：[Fetcher 手册](../guides/FETCHER_WRITING_GUIDE.md)、[采集工具](../guides/CRAWL_TOOLKIT.md)、[账号接入与登录流程](ACCOUNT_ONBOARDING.md)。

## 1. 入口与边界

浏览器身份通过独立 `(site, account_id)` 绑定，池内与池外账号 API 都遵守；同账号跨池共享绑定，任务或显式调用内固定快照。改绑排空后复验，候选凭据独立保存，失败不删除旧凭据、不触发换号。`SiteChallenge` 在账号分类器之前处理，区别于账号 `ErrChallenge`；详见 [浏览器身份手册](BROWSER_IDENTITY.md)。HTTP 仍沿用原有 UA/头配置，不自动模拟浏览器 TLS/HTTP2。

`pkg/account.Manager` 是显式注册的单进程账号管理器，以 `account.Key{Site, ID}` 定位账号。HTTP/API 和 HTML 使用同一 CookieJar、Token 与 CSRF 头；Rod 使用同账号的隔离浏览器状态。Manager 不自动发现凭据；可选 `account.Pools` 在其上维护站点内池、独立补偿调度和持久管理状态，见 §7。

| 入口 | 用途 |
| --- | --- |
| `account.NewManager(client)` / `papa.NewAccountManager(client)` | 创建注册表；HTTP client 可由 Engine.GetHTMLClient 提供 |
| `Register(key, Config)` | 注册站点＋账号，一次注册后配置不再外部修改 |
| `RegisterCandidate` / `PublishCandidate` / `Version` | 核验独立候选后注册或按版本更新；失败保留原状态，见 §8 |
| `Manager.Do` / `Engine.FetchAccountHTTP` | 有限认证恢复的 HTTP 请求 |
| `Manager.FetchHTML` / `Engine.FetchAccountHTML` | 同账号抓 HTML，不进入原始 HTML 页面归档 |
| `Manager.WithBrowser` / `Engine.WithAccountBrowser` | 恢复、检查、必要时登录，执行一次浏览器回调并保存状态 |
| `Pause` / `Resume` / `Logout` | 停止新操作、恢复并重新检查、清除本地状态并暂停 |
| `Status` / `Engine.AccountStatus` | 无凭据状态列表，可接业务自定义后台 |
| `NewFileStore` | 绑定账号身份的加密本地存储 |
| `NewPools` / `Manager.Pools` | 站点内账号池与独立调度；App 为已注册 Manager 接入 GORM 池存储 |
| `Manager.DoPool` / `Engine.FetchPoolHTTP`、`FetchPoolHTML`、`WithPoolBrowser` | 使用池当前账号，失败不在本次尝试换号重发 |

仅注册管理器、未配置任务池时，原有显式账号调用仍可使用，不会自动选账号。启用任务池归属后，普通 Engine HTTP/HTML/渲染等受支持入口会使用本次尝试固定的账号，不能靠普通入口悄悄匿名请求，具体覆盖见 §7。非池任务仍可在 `Task.Meta["account_id"]` 保存显式账号标识；用户名密码、Cookie、Token 不放进 Meta。

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

上例 `loginAccount` / `checkAccount` 是站点实现，签名分别为 `func(context.Context, *account.Credentials) error` 和 `func(context.Context, *account.Credentials) (bool, error)`。它们用 `credentials.Context(ctx)` 向受管 `htmlfetch.Client` 发出登录/检查请求；该 context 绑定候选 CookieJar 和凭据头，自建的普通 `net/http.Client` 不会自动读取这些 context 值。回调自己约束目标 origin 和重定向，不能假定 `Credentials.Context` 已替它完成范围检查。Login 可设置 `credentials.Headers["Authorization"]`、`credentials.Headers["X-CSRF-Token"]`；refresh token 等私有状态放入 `Values`，它不会自动发送。协议和刷新接口由站点定义。

只允许 Authorization、X-CSRF-Token 和 `AuthHeaders` 中额外声明的凭据头。请求本身不能覆盖这些头或直接塞 Cookie。账号入口清除继承的这些凭据和 Proxy-Authorization，再绑定当前账号；共享配置中其他自定义认证头必须列入 AuthHeaders。`core.WithHeaders` 现在按 HTTP 规则执行大小写不敏感的覆盖/删除。

Check 必须验证真实账号身份。明确未登录返回 `false, nil`；网络错误返回 error，不能触发误登录。新候选始终使用新的 Jar，登录后复验通过才发布。每轮认证按账号合并并发；首调用者的取消会结束该轮，等待者可独立取消。回调必须配合 ctx，不能重入同一账号或保留候选继续修改。

CheckInterval 为 0 时逐次检查，正数允许近期有效结果复用；实际业务请求仍分类处理失效。Timeout 默认为 30 秒，FailureCooldown 默认为 5 秒；这两项的 0 表示使用默认值，不表示关闭。MaxConcurrent 默认 1，范围 1..1024，设大后同账号 HTTP 可以并发，但站点必须允许同时操作和 Cookie 更新。上述预算不接受负值。需要严格 Token 顺序时保持 1。浏览器回调始终在账号内串行执行。

自动 Rod→HTTP 登录桥接与 `WithBrowser` 共用同一账号浏览器锁；桥接持锁直到 HTTP 复验、快照保存和状态发布完成。等待者拿锁后读取最新浏览器状态，避免重复登录或覆盖刚发布的状态。缓存命中和仅 HTTP 检查不占用浏览器锁，等待锁可以独立取消。

## 3. 过期、限制与 Token 轮换

默认将完整 HTTP 401 分类为 `NeedsVerify`（可疑、待核验），403 为 `Forbidden`；网络/读取错误保持 `Unknown`。站点的 200 登录页或模糊 JSON 认证码也应返回 `NeedsVerify`。`Expired` 仍表示业务已明确确认登录态失效，不能继续把所有 401 当作强证据。旧自定义 `Classify` 的 401 分支需要业务检查并按证据强度迁移；框架不会静默改写自定义分类结果。有效分类为 `Valid`、`NeedsVerify`、`Expired`、`Forbidden`、`Challenge`、`Unknown`。判断前检查 err，不用不完整正文作失效证据。

- 显式非池 `Manager.Do` 对 `NeedsVerify` 和 `Expired` 支持自动恢复：只废弃实际使用的 HTTP 版本，GET/HEAD 最多重新认证并重发一次。第二次异常分别返回 `ErrNeedsVerify` 或 `ErrExpired`。旧响应不能废弃已刷新的版本。
- 池请求对 `NeedsVerify` 返回 `ErrNeedsVerify`，保留凭据和当前选择，进入独立核验；`Expired` 返回 `ErrExpired` 并隔离对应会话。两者都不在本次尝试重发，详见 §7。
- POST/PUT/PATCH/DELETE：返回相应认证错误，不自动重发。业务按幂等键和远端执行结果决定后续操作。
- Forbidden：返回 ErrForbidden，不重新登录、不自动更换账号。
- Challenge：返回 ErrChallenge 并暂停账号；人工完成处理后显式 Resume。
- Unknown：保留底层请求错误语义，不增加重试。

配置可选 `Update(candidate, response)`，在成功响应后根据站点约定更新 Token/CSRF/refresh token。候选头校验通过才替换，发生变化时增加 HTTP 凭据版本。Update 必须快速、不重入管理器、不替换 CookieJar；并发时只接收当前版本的响应更新。普通 Set-Cookie 仍由 Jar 自动处理。浏览器状态变化不会单独增加 HTTP 凭据版本。

`Invalidate(key, version)` 适合已知版本的显式 HTTP 失效标记，未匹配版本返回 false。`Pause` 阻止新操作和过期认证结果发布，但不强杀已发送请求；暂停标志是进程内状态，长期停用使用持久的 `Pools.SetDisabled`；仅检查模式不解除暂停。`Logout(ctx,key)` 清除内存凭据、删除存储并暂停；它不是网站远端退出登录 API。删除失败会返回错误，需处理后再恢复，以免旧文件仍留在磁盘。

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

Status 给出账号键、HTTP 版本、是否暂停、是否正在认证、近期检查/登录时间、冷却截止和失败类别。`HTTPReady` / `BrowserReady` 分别表示已有通过检查且未被暂停或失败阻断的本地状态，不保证服务端此刻有效；配置能力由 `HTTPConfigured` / `BrowserConfigured` 单独表达。不会包含 Cookie、Token、Values、代理密码或回调原始错误。业务后台可用 `engine.AccountStatus()` 挂只读自定义数据接口；暂停/注销等写操作应沿用业务后台权限。

认证 HTTP/HTML 入口不进入原始 HTML 页面归档，避免登录后页面凭据随失败证据落盘。需要回归材料时显式使用 WriteSample 脱敏；浏览器截图和回调自行记录的数据仍由业务控制。Failure.Error 只返回类别，Unwrap 仍可供 errors.Is/As 检查，不能把解包的原始错误直接写日志。

账号运行时服务于单实例采集；验证码处理、池内账号能否互换和生产站点是否允许 Cookie 桥接由业务负责。测试使用本地 HTTP 和真实 Chromium，站点登录协议与业务规则仍需独立业务模块验收。

## 7. 站点账号池与异步补偿

池键是 `account.PoolKey{Site, Name}`；成员引用同站已经 `Register` 的账号 ID，不复制凭据。每站可以有多个权限相同的命名池，不把不同权限的账号混在一起。成员顺序独立保存，替补按该顺序选择；哈希索引不决定选择顺序。同账号加入同站多个池时，共享管理状态，停用或隔离会对这些池同时生效。

### 接入顺序

1. 在 App.Run 前注册账号 Manager、Login/Check、Store 和可选 Browser 配置；先执行 `papa migrate`。
2. 在“数据与资源 → 账号池”创建池，选择已注册账号及顺序、HTTP/browser/both 传输要求。也可在业务初始化中使用 `NewPools` + `Upsert` 创建，但须提供持久 Store、站点预算 context 和任务引用检查。
3. 在“采集控制”设置站点/阶段 `pool`，或提交 `Task{Site: "fixture", Stage: "detail", AccountPool: "readers", URL: ...}`。持久策略不能引用不存在的池，因此应先建池再保存归属默认值。
4. FetchHandler 继续传递收到的 ctx，调用普通 Engine HTML/HTTP/渲染入口，或显式 `FetchPoolHTTP` / `FetchPoolHTML` / `WithPoolBrowser`。在任务外先用 `RuntimeContext` 绑定站点。

```go
ctx = engine.RuntimeContext(ctx, "fixture")
response, err := engine.FetchPoolHTTP(ctx,
    account.PoolKey{Site: "fixture", Name: "readers"},
    papa.HTTPRequest{URL: "https://fixture.example/items"},
)
if err != nil { return err }
_ = response
```

示例站点和 URL 仅为合成占位，账号 Origins、认证与模型仍由业务注册。低层 `Pools.Acquire` 返回固定身份的 Attempt，不另占一份账号并发许可；每次实际交换复用 Manager 的 `MaxConcurrent` 与认证合并。业务不要在持有自己的账号许可时再嵌套申请同一账号。

### 正常执行与失效

当前账号正常时持续使用，不因一条请求结束、阶段变化或普通重试轮换。并发已满只等待当前账号名额。可疑认证、明确过期、人工验证阻断或固定出口不可用时，本次尝试返回失败。可疑认证先保留当前成员并独立核验；强失效与人工验证进入隔离；固定出口先缓冲探测，间隔确认持续不可用后才能选备用，凭据保留。失败任务不等待换号、不在当前 FetchHandler 重放，也跳过当前阶段的立即重试；错误队列或人工重投以后再补偿。

每次尝试固定账号、凭据与选择版本；已发请求自然结束，切换后旧尝试不能继续发送使用新身份的请求。旧响应不覆盖新凭据或已验证状态。403、整站 429、普通目标网络失败不直接失效账号。显式 `Manager.Do` 的 GET/HEAD 最多一次认证重试仍按 §3 保留；`DoPool` 不在明确失效后沿用这个隐式重放。

`Task.AccountPool` 保存到 `crawler_tasks.account_pool`，去重仍只认既有页面/自定义幂等键，不增加账号/池维度。同一已有任务被显式指定为其他池时返回冲突；重复提交不覆盖归属。以后补偿复用原任务，取所属池的新当前账号；更改站点默认池不迁移旧任务。账号专属游标或资源是否可跨账号补偿由业务判断。

### 持久化、验证和预算

| 配置/状态 | 行为 |
| --- | --- |
| `PoolConfig.Transport` | `http`（默认）、`browser`、`both`；配置能力与已验证状态分别判定，纯浏览器账号不因 HTTP 未就绪被排除 |
| `Members` | 有序 ID 列表，最多 1024 个，同站且不可重复 |
| `VerifyAttempts` | 默认 1，范围 1..10；恢复登录模式在单次超时预算内的最大尝试数，仅检查模式每轮始终只检查一次 |
| `SwitchLimit` | 默认 16，范围 1..1024；候选扫描上限。异步协调器按成员顺序提交候选，每轮至多启动一个新的候选检查，不并发抢选 |
| `Cooldown` | 默认 5s，范围 1ms..24h；到期仅允许复验，不直接恢复 |
| `PoolOptions.ScanInterval` / `VerifyTimeout` | 默认 1s / 30s；独立调度扫描及验证超时上限；实际超时取此上限与账号 Policy.VerifyTimeout 较小值 |
| `PoolOptions.VerifyConcurrency` | 默认 4，范围 1..64；全局核验工作上限。同站最多一个，慢站点不会占满全部槽位 |
| `StandbyTarget` / `StandbyTTL` | 目标默认 0（关闭）；启用时 TTL 默认 15 分钟。当前账号健康时按成员顺序补近期通过核验的备用 |
| `PoolOptions.ReverifyInterval` | 默认关闭，作为协调器的全局恢复间隔；账号 Policy.RecoveryInterval 为正时覆盖它。人工验证阻断、持久停用和 Manager.Pause 均不自动解除 |

`crawler_account_pools` 保存成员顺序、池开关、当前账号/选择版本和最近 100 条切换记录；`crawler_account_management` 的现有 JSON 保存账号停用、隔离、可疑状态、核验模式、凭据版本、证据计数、下次核验时间、政策、用量及有界健康事件；不新增凭据列。凭据仍在既有 Store，普通配置 JSON 不含 Cookie/Token。重启重新验证认证状态，持久停用/隔离不被自动清除。

管理状态保存失败时当前进程仍阻止新请求使用失效账号，后台显示未保存/持久化错误，并由调度器重试；不能把失败保存当作已经安全落盘。重新验证成功才恢复候选，恢复不自动切回。删除池必须先停用，并确认没有策略引用、待处理/处理中/失败可补偿或轮询任务引用。

### 后台与证据

池页展示当前账号、成员状态、HTTP/浏览器能力、等待数与原因、选择版本、最近切换；支持创建/启停池、排序、停用成员和重新验证。写动作带版本检查、令牌鉴权与审计；GET 查询不触发认证。验证会实际访问配置站点，并受超时与站点预算约束。

每条任务通过 `crawler_tasks.account_attempts` 独立于 Trace 保存最近 8 次认领的账号关联，含账号 ID、选择/凭据版本、时间、轮询轮次和补偿代数，不含凭据；后台可按该有界记录查找受影响任务。更早关联会被淘汰，保留窗口之外或未记录的证据不会补造，空结果不代表从未使用该账号。Engine 自动记录“账号池身份”步骤，无需业务重复埋点；`crawler.trace.mode` 默认 `errors`，仅保留异常尝试，`all` 才保留正常成功尝试，`off` 关闭文件追踪。标准 Render、CaptureRequest/CaptureResponse 自动绑定本次账号；Block 与 GovernanceOrigins 在 bootstrap 状态恢复页创建时安装。采集目标仍必须与账号 bootstrap 同源，不能用治理 origin 列表扩大账号认证范围。明确的固定 HTTP 407/代理连通故障与浏览器固定出口故障先保留凭据并暂停新操作，独立核验确认持续故障后隔离出口并触发备用；`ProxyChecks` 与 `InvalidChecks` 分开计数，普通 Rod 超时不能据此认定代理坏。完整治理覆盖见 [RUNTIME_CONTROL.md](../runtime/RUNTIME_CONTROL.md)，不能将登录态隔离解释为全浏览器目标的请求计量。


### 可疑状态与两种核验

`Pools.CheckExisting(ctx,key,managementVersion)` 与 `Pools.Verify(ctx,key,managementVersion)` 复用同一协调器、并发合并、版本守卫和结果发布。`CheckExisting` 仅使用对应版本的现有 HTTP/浏览器状态，实际调用 Check，不命中近期成功缓存，也不调用 Resume、ensure、WithBrowser 或 Login。`Verify` 保留原有允许恢复登录的语义，适合明确恢复操作。两种入口提交的是异步工作，成功回执表示请求已保存，不表示核验成功。

默认可疑缓冲 10 秒；一次独立检查成功即恢复原账号。`false,nil` 是确认无效证据，需要两次至少间隔 10 秒的检查才隔离。同一凭据版本的并发失败只创建一轮；检查次数不由失败请求数累加。超时、网络或其他未知错误不计入无效次数，按 10 秒起、最长 5 分钟退避，等待 5 分钟后产生等待过久事件。明确人工验证停止自动恢复。冷却结束只允许再次检查。

异常记录的 `CheckTransport` 说明证据来自 HTTP、browser 或 both；HTTP 失效不会删除浏览器快照，浏览器失效不会删除 HTTP 凭据。管理上的暂停/隔离仍以账号为粒度，统一暂缓该账号所属池；不能将这一门控理解为已证明另一传输的凭据也失效。固定出口使用独立标志与计数，失败确认不删除任何凭据。

新凭据发布后，旧核验结果不恢复或撤销新状态；协调器将重新检查新版本。健康恢复只让账号重新成为候选，不抢回其他健康的当前账号。已发送请求自然结束；同一旧 Attempt 后续操作被版本门控拒绝。

`Readiness(PoolKey) (bool,string)`、`Get`、`Snapshot` 都是只读，不登录、不选账号、不扣预算。任务补偿应依据 Readiness 等待条件恢复，沿用原任务和池归属；不调用 Verify 来“测试是否就绪”。

### 账号政策与用量

`Policy(key) (Policy,managementVersion,error)` 读取有效值；`SetPolicy(ctx,key,policy,expectedVersion)` 使用管理版本 CAS 保存。注册但尚未创建管理记录的账号返回默认政策和版本 0。保存失败会返回错误，账号呈现未保存状态；不能向操作人报告设置已持久生效。直接 JSON 里的 `time.Duration` 使用纳秒，后台应在提交前显式换算。

持久管理状态必须包含有效 Generation 和 LastControlVersion；缺失边界明确报错，不进行旧 JSON 版本回填。

| Policy 字段 | 默认值 / 单位 | 作用 |
| --- | --- | --- |
| Buffer | 10s | 可疑信号后的首轮检查等待 |
| InvalidConfirmations | 2 次 | 间隔无效确认阈值；代理确认使用独立计数 |
| CheckInterval | 10s | 确认无效/代理故障后的下一轮间隔 |
| VerifyTimeout | 30s | 单轮核验超时，另受协调器上限限制 |
| UnknownBackoff / MaxBackoff | 10s / 5m | 未知结果指数退避 |
| UnknownAlertAfter | 5m | 可疑等待过久事件门槛，不是判失效门槛 |
| RecoveryInterval | 0，关闭 | 隔离后的自动恢复间隔；为 0 时使用池协调器的全局 ReverifyInterval |
| OperationInterval | 0，无限制 | 同账号两次公开业务操作的最小间隔 |
| DailyLimit | 0，无限制 | 按账号、按配置时区自然日的操作额度 |
| Timezone | UTC | 额度重置及工作时段的 IANA 时区 |
| WorkStartMinute / WorkEndMinute | 0 / 0，全天 | 本地日内分钟 0..1439；相等全天，开始大于结束表示跨午夜 |
| LoginInterval | 0，无限制 | 两次登录尝试之间的最小间隔，失败也计入 |
| TrackUsage | false | 没有额度/间隔限制时，显式启用严格持久操作统计 |

`Usage.Unit` 固定为 `operation`。DailyLimit、OperationInterval 或 TrackUsage 启用时开始统计；默认不统计操作数。一次 `Do`/`DoPool` 的实际 HTTP 交换计一次；显式 GET/HEAD 恢复后的第二次交换再计一次。一次浏览器业务回调/导航捕获操作计一次，无论它触发多少子请求，不能解释为网站网络请求配额。协调器健康检查不扣业务操作额度，仍使用 `PoolOptions.Context` 注入的站点请求预算；实际 Login 受 LoginInterval 限制。接入适配器可在开始外部登录前调用 `Manager.ReserveLogin`。

启用额度、操作间隔或 TrackUsage 时，在发送业务操作前跨池原子预留并持久保存，失败不退额度，避免重放和并发超额。用量记录保存失败时请求不会发送，但保守保留该次预留并继续重试保存。用尽、间隔未到或工作时段之外返回 `ErrBudget`/就绪等待原因，不更换健康账号。操作额度与账号并发许可分开；计数与控制代数分开，普通用量更新不会使同一 Attempt 失效。

默认政策不增加逐次管理 CAS 或全局写许可等待；只配工作时段也仅做内存门控。启用 DailyLimit、OperationInterval 或 TrackUsage 后，每次业务操作增加管理 JSON 的 CAS 写入；LoginInterval 为正才持久预留登录尝试。高频场景需把这些可选数据库写入和凭据 Store 写入一起纳入单实例资源预算。备用核验受相同站点预算、核验并发与登录间隔约束，默认不产生备用维护流量。

### 健康事件与停止

`Events(key,afterSequence)` 返回序号之后最多 100 条事件；`Key{}` 查询全部账号。每账号持久保存最近 100 条，池事件包含 PoolKey，事件只包含框架原因码、账号/池键、任务 ID、版本及时间，不包含 URL、Cookie、Token 或提供方错误原文。这是有界排障记录，不是永久审计账本。

`AddEventListener(func(HealthEvent))` 返回退订函数，每个订阅有 32 条有界队列，慢订阅丢弃后续通知，不阻塞核验；回调在池锁之外执行。业务可按序号补查 Events，但被保留窗口淘汰的数据无法补回。事件包括 needs-verification、verification-invalid、verification-unknown、verification-overdue、proxy-unavailable、manual-verification、recovered、pool-exhausted 与 standby-shortage。通知按账号/池聚合节流；不要把每次事件等同一次独立登录或永久失效。

`Shutdown(ctx)` 停止新工作并等待协调器和所有核验回调结束。超时返回 ctx 错误，Manager 仍保留该协调器，调用者必须继续保留 Store、浏览器等依赖，之后重试 Shutdown；`Close()` 等待全部结束，无截止时间。取消后的迟到核验结果不发布。回调必须遵守 context；忽略取消的第三方回调无法被 Go 安全强杀，只能有界等待并保留其依赖。

## 8. 已验证候选发布

`RegisterCandidate(ctx,key,config,credentials)` 在注册表可见之前，克隆候选、检查 HTTP/可选浏览器身份并保存凭据；失败不会留下可采集的注册。`PublishCandidate(ctx,key,expectedCredentialVersion,credentials)` 先独立 Check，再按版本保存发布；不会调用 Login，验证失败、过时版本或保存失败都保留旧状态。候选必须包含独立 Session；BrowserState 可选，存在时也要通过 Browser.Check，并检查浏览器状态代数防止覆盖并发更新。HTTP-only 候选保留同一身份档案下的旧浏览器快照，但不声称重新验证了它，不能靠 HTTP 证据解除 browser-only 健康排除。候选验证与发布全程固定身份快照，并保存其身份元数据；已固定的同账号 context 复用该快照。业务适配器仍负责真实账号与预期账号匹配，通用框架不猜测站点身份。

取消在保存前到达时不提交；若 Store.Save 已返回成功，保存是提交点，之后到达的取消不撤销成功结果，内存与持久状态保持一致。Store 必须遵守保存成功/失败语义；若自定义 Store 在写入后返回错误，调用方需要依据其读回/不确定提交协议核对，不能仅凭错误假定磁盘仍是旧值。

凭据发布与池管理恢复是两个提交：已有管理状态时，在成功发布后调用 `Pools.CandidatePublished(ctx,key,newCredentialVersion)`。它确认新凭据版本后清理可疑/失效排除并保存健康结果，保留人工 Disabled；若此步保存失败，显示“凭据已更新，健康恢复待保存”，不能反过来宣称凭据发布失败或账号已可用。新成员加入池仍不会抢占健康当前成员。账号资料、验证码/扫码过程、导入与远程交互见 [账号接入手册](ACCOUNT_ONBOARDING.md)。
