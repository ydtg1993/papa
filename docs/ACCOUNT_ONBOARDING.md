# OA 账号接入与受控登录

`pkg/account/onboarding` 负责账号资料、临时登录过程、候选验证和发布。真实网站的密码协议、验证码发送、扫码状态、Cookie/Token 格式、第三方授权及身份识别由独立业务模块的 `Provider` 实现。Papa 只包含通用接口和本地合成测试。

## 业务接线

先按项目现有流程显式执行 `papa migrate`。`onboarding.Models()` 中的 `crawler_account_profiles` 已纳入框架迁移；`NewGORMStore`、`Service.Load` 和普通启动都不创建表。

```go
import (
    "time"
    "github.com/ydtg1993/papa/v4/pkg/account"
    "github.com/ydtg1993/papa/v4/pkg/account/onboarding"
    "github.com/ydtg1993/papa/v4/pkg/htmlfetch"
)

// app 为已经建立 DB/Engine 的 *papa.App。
// externalKey 是业务从密钥服务或受保护环境读取的 32 字节密钥。
// privateDirectory 是由业务管理 ACL 的私有目录。不要把密钥写入配置模板。
manager := app.Engine.GetAccountManager()
if manager == nil {
    // Run 前 Engine 的默认 HTML client 通常还没有初始化。
    // 显式创建受框架出口策略和 context 治理的 client，不能传 nil。
    client := htmlfetch.NewClient(htmlfetch.Config{
        Network: app.Config.Network,
        Timeout: 30 * time.Second,
        MaxBodySize: 2 << 20,
    })
    manager = account.NewManager(client)
}
secrets, err := account.NewFileStore(privateDirectory, externalKey)
if err != nil { return err }
service, err := onboarding.New(onboarding.Options{
    Manager: manager,
    Profiles: onboarding.NewGORMStore(app.DB),
    Secrets: secrets,
    MaxActive: 4,
    MaxRetained: 128,
})
if err != nil { return err }
if err = service.RegisterProvider(siteLoginProvider); err != nil {
    service.Close()
    return err
}
if err = app.UseAccountOnboarding(service); err != nil {
    service.Close()
    return err
}
```

`siteLoginProvider` 是业务模块实现的 `onboarding.Provider`。一个站点只能注册一个 Provider。`UseAccountOnboarding` 在 Engine 尚无 Manager 时安装相同 Manager，已安装不同 Manager 或不同接入服务时拒绝覆盖；同时叠加 `Engine.RuntimeContext` 请求治理，并接入账号健康通知。它返回 `error`，业务应检查。

示例 client 专用于账号 Manager；Provider 自己的请求也应使用同等出口配置的 `htmlfetch.Client` 并传递流程 context。正式账号的固定 HTTP 出口通过 `account.Config.ProxyURL` 声明。不要在 `Run` 前直接假定 `Engine.GetHTMLClient()` 已非 nil；App 在较后的启动阶段才初始化默认匿名 HTML client。

App 在建立账号池之前调用 `Load(ctx)`，加载已核验账号。未核验草稿不注册到 Manager，无法加入池或用于采集。持久资料与代码注册的同键账号冲突、Provider 缺失、密文读取失败都使加载明确失败，不静默抢占已有注册。业务不使用 App 时需要自行在请求入口开放前调用 `Load`，以及为 `Options.Context` 提供站点治理 context。

没有安装接入服务时，OA 显示业务尚未配置适配器和外部加密存储。框架不生成默认密钥，不从普通配置字段或日志恢复密码。

## Provider 和过程契约

```go
type Provider interface {
    Descriptor() onboarding.Descriptor
    Config(context.Context, onboarding.Profile,
        func(context.Context) (map[string]string, error)) (account.Config, error)
    Start(context.Context, onboarding.Profile, string,
        map[string]string) (onboarding.Flow, error)
    Verify(context.Context, onboarding.Profile,
        *account.Credentials) (onboarding.Identity, error)
}

type Flow interface {
    Next(context.Context, map[string]string) (onboarding.Step, error)
    Close() error
}
```

- `Descriptor` 声明站点和实际支持的方式。`Method.Kind` 为 `password`、`input`、`qr`、`import` 或 `remote`；没有声明的方法不出现在 OA，也不能通过 API 发起。无字段方法允许 `Fields == nil`。
- `Field` 声明 `Name`、`Label`、`Secret`、`Required` 和 `Persist`。服务拒绝未声明字段；仅 `Persist` 字段在用户明确勾选时允许加密保存。验证码不得声明为可长期保存。
- `Config` 只构造正式运行的 `account.Config`，不能在此发起登录。它要给出真实 origin、HTTP Check、必要的浏览器 Check 和固定出口配置。第三个参数读取当前正式资料中已保存的字段，返回值只供可信业务代码使用。
- `Start` 创建本次 `Flow`。传入的是整个登录过程的 context，便于隔离浏览器活到后续步骤；创建超过 `StepTimeout` 时会取消整个过程。`Next`、`Verify` 和发布分别带步骤超时。Provider 必须配合取消，不能在方法返回后异步修改已交给框架的候选。
- 初始 `Start` 成功后，服务调用一次 `Next(ctx, nil)`。后续结构化输入由 `Next` 接收，重复提交受过程 revision 保护。
- `Step.State` 为 `waiting_input`、`waiting_scan` 或 `remote`。`ImageDataURL` 只接受有大小限制的 PNG data URL，不接受远程图片 URL。
- 完成时返回 `Step.Candidate`，其 `Credentials.Session` 必须非 nil。HTTP 头、Values、Cookie 和可选 `BrowserState` 都只作为候选，不直接覆盖正式状态。
- `Verify` 必须通过可信的站点身份接口或已登录页面识别真实账号，返回稳定 `Subject`、脱敏 `Display`、权限 `Scopes` 和实际核验的 HTTP／浏览器能力。不能仅返回用户提交的账号名称。
- `Close` 回收本次流程资源，须允许取消后调用。原始站点错误不会回传 OA 或写入审计；公共字段、说明、二维码不能夹带密码、Token 或调试 URL。

`Start`、`Next`、`Verify` 不占采集任务执行许可；服务最多同时保留 `MaxActive` 个尚未完成资源回收的登录。它们仍必须通过带入的 context 使用受治理的 HTTP／浏览器入口。已有账号调用 `Manager.ReserveLogin` 共享最短登录间隔。业务自行创建的裸网络客户端不会自动获得这些预算。

## 密码、验证码、扫码和导入

密码方式由 Provider 按声明字段读取。验证码方式先返回 `waiting_input` 及本步字段；OA 只提交这些字段。扫码方式返回 `waiting_scan` 和 PNG 二维码，操作者用“检查扫码结果”继续本次步骤。业务负责二维码有效期、验证码有效期、发送/重发限制及站点错误判定；“继续”不会自动反复发送敏感操作。

`import` 方式使用文本框接收站点声明的 JSON 或其他结构化格式。Provider 必须限制大小、拒绝未知结构、检查 origin/作用域，然后转换为 `Credentials` 并执行相同身份核验。框架不把任意 JSON 直接当作任意站点通用 Cookie 文件，也不会因导入成功就宣称支持远程人工登录。

`service_test.go` 有独立的密码、验证码、扫码及 JSON 导入合成 Provider；它们用于验证框架状态机，不作为真实站点适配器或可用账号预置。

## 身份确认和发布

资料使用稳定的 `(site, id)` 键，不用密码、Cookie 或 Token 的哈希作为账号键。`SaveDraft(ctx, profile, expectedRevision)` 通过 CAS 保存资料。调用方提交的 `State`、`Identity`、密文引用不会使草稿变成已核验账号。

流程如下：

1. OA 保存资料并选择站点声明的登录方式。
2. `Start` 返回独立过程 ID；OA 查询进度或携带当前 revision 继续输入。
3. Provider 完成登录并核验真实身份。首次接入若配置 `ExpectedSubject`，必须精确一致；重新登录必须匹配已绑定的 `Identity.Subject`。
4. 服务加密暂存候选，进入 `confirm_identity`。OA 展示脱敏名称、真实 ID、权限及能力，等待明确“身份正确，保存并启用”。
5. `Manager.RegisterCandidate` 或 `Manager.PublishCandidate` 再执行自身 Check。只有 Check、凭据存储和资料 CAS 都成功才发布。
6. 已存在的健康排除通过 `Pools.CandidatePublished` 对新版本处理。人工停用不解除；HTTP 证据不能解除浏览器隔离。凭据已发布但管理状态保存失败时，OA 单独提示恢复状态待保存。
7. 已核验账号可通过账号列表加入同站账号池。加入成员不抢占当前健康账号。

取消幂等；重复输入、旧 revision 和旧身份候选被拒绝。登录失败、真实身份不符、Manager Check 失败或已确认的持久化失败保留旧可用凭据。密码保存选项对本次成功发布生效：未勾选时移除之前保存的输入引用，失败不删除原输入。

资料版本和凭据版本独立。运行期刷新也会推进正式资料的密文引用 CAS，因此长时间打开的资料表单或登录候选可能发生版本冲突；OA 保留输入，要求刷新核对，不自动重放。

## 加密、重启与资源回收

默认值：`MaxActive=4`、`MaxRetained=128`、`TTL=10 分钟`、`StepTimeout=30 秒`、`Retention=15 分钟`。`MaxRetained` 包括活跃过程，最旧终态记录在新过程进入时回收；保留时间同样在进入时清理。单条流程不超过 TTL，单账号在旧流程的浏览器及暂存文件全部回收后才能开始新流程。

候选和可保存输入先写入每账号确定的加密暂存槽；正式发布把登录态和选中的输入复制为不可变随机代次。资料表只存普通资料、核验结果和不透明引用。正式 `InputsRef`/`SecretRef` 不能指向暂存槽。`Load` 在开放登录前检查并清理已知暂存槽，终止上次进程留下的临时交互；旧过程 ID 查询返回 410，不重新使用验证码或远程控制权。

正式发布以资料 CAS 成功为持久提交点，Manager 只在 Store.Save 成功后切换内存。GORM 写入错误会尝试读回确认；如果数据库断开导致结果仍不确定，新随机密文会保留，避免数据库已经提交而清理器删除其指向的文件。此类应答不确定可能留下没有正式引用的密文代次；框架不会猜测并删除它，维护者应先核对资料引用再离线清理。密钥与目录的备份、Windows ACL 和密钥轮换由业务负责。

App 关停先调用 `Shutdown(ctx)` 取消登录并等待清理，超出 drain 预算返回错误，不提前回收仍被 Provider 使用的浏览器依赖。`Close()` 是无截止时间的关闭方法。Provider 忽略取消或事件回调永久阻塞时，框架不能强制结束该业务 goroutine；`Shutdown` 仍会按调用方截止时间返回。

## A04c：受控服务器浏览器

远程交互是独立子系统。Provider 在 `Start` 内使用整个流程 context 创建：

```go
remote, err := onboarding.NewRemote(ctx, onboarding.RemoteOptions{
    Pool: loginPool,
    URL: declaredLoginURL,
    AllowedOrigins: declaredLoginOrigins,
    Direct: useDirectPool,
    PageOptions: loginPageOptions,
    DisconnectTimeout: 30 * time.Second,
})
if err != nil { return nil, err }
// 本次 Flow 首次 Next 返回 Step{State: "remote", Remote: remote}。
// 用户点“已完成页面登录”后，Next 通过 remote.WithPage 导出候选。
// Provider.Verify 独立核对真实身份。Flow.Close 调用 remote.Close。
```

上述 URL、origin 列表、Pool、代理和 PageOptions 必须来自业务代码声明，不能来自 OA 任意 URL 输入。每次流程借用一个池内浏览器，创建独立 Chromium context；取消、完成、超时或断线均销毁该 context，清掉其 Cookie/Storage 后才归还池名额。登录用的 Pool 有自身大小限制；可用同一专用账号池，也可业务显式提供等价身份、出口的登录资源池。

服务在开始时 `Manager.PinIdentity`，把身份快照保持到资源清理结束。`NewRemote` 继承该快照，固定身份的 viewport 与设备比例不能被远程尺寸覆盖；超过远程画面预算的档案拒绝开启。Provider 仍需使登录与正式 `account.Config` 的 Pool/Direct、HTTP ProxyURL、UA、Cookie origin 和权限一致，框架不能从真实网站协议推断这些配置。

传输使用每次重新鉴权的受控 JSON 请求 `/api/accounts/remote`，不开放原始 CDP/WebSocket 地址。只有过程所属操作人可获取画面或输入，首个控制者获得随机 lease，第二个控制者不能接管。同一控制者在截止时间内携带 lease 可重连；UI 在当前标签的 sessionStorage 只暂存过程 ID 与受鉴权约束的控制 lease，不保存站点密码、Cookie 或 Token。

支持的操作是 `claim`、`frame`、`click`、`text`、受限 `key`、`scroll`、`select`、`release`。没有任意脚本执行、导航 URL、文件传输、开发者工具或原始协议转发。`WithPage` 仅是可信 Provider 的 Go 回调，HTTP 请求无法提供它。截图只抓 viewport；浏览器坐标使用 `[0,1)` 的归一化值，再映射到服务器 CSS viewport，因此 OA 缩放不改变目标点。

默认 viewport 为 1280×720；上限 1920×1080，固定身份的渲染像素预算不超过 3840×2160，单幅 PNG 不超过 4 MiB；最多枚举当前隔离 context 的 8 个页面。截图最小间隔 300ms，OA 通常每秒取一帧；单次文本不超过 4096 字节，输入操作最小间隔 15ms；OA 待发送输入最多 8 个，不重放结果未知的输入。

只展示业务 allowlist 内的页面，先按 `BrowserContextID` 隔离，再赋予不透明页面 ID。未绑定托管身份时，同一 context 内已声明 origin 的弹窗可选择。**托管身份下，原生弹窗没有经过导航前的身份初始化，当前不会提供其远程控制入口；这类 Provider 应采用受支持的同页流程。** 主页面离开允许 origin 后控制被拒绝并回收。页面自己的网络跳转、子资源和原生弹窗首个请求仍服从现有浏览器出口/治理覆盖边界，allowlist 不是新建的全浏览器防火墙。

OA 支持点击、有限键盘、文字和滚动，不承诺文件上传、系统原生对话框、任意拖拽或所有交互挑战都能完成。复杂站点必须由业务实际验证后再声明 remote 方法。

每个请求重新经过后台令牌鉴权。令牌撤销后新请求被拒绝；已发送操作自然结束，随后因无有效心跳在断线期限内回收。默认 30 秒无有效请求结束远程过程，范围可配置 1 秒到 2 分钟；整个过程仍受 TTL 限制。关闭控制面板发送 release，丢失的 release 由断线期限兜底。慢响应必须同时匹配过程 ID 和面板代次才能显示，旧画面不会进入新登录。

## 通知、审计与验证覆盖

`SetEventListener` 在独立 worker 投递脱敏 `login-failed` 与 `recovered` 事件，容量 64；慢通知不会阻塞 Provider，溢出通知丢弃。App 接到 `Engine.NotifyAccountHealthEvent`，使用引擎现有按账号/类型聚合节流。GET 进度不会触发通知。操作审计不记录输入、Cookie、Token、原始 Provider 错误或远程文本；过程状态是有界临时记录，不是永久登录事件账本。

主要回归：

- `TestLoginCandidateIdentityConfirmationEncryptionAndRestart`：候选确认、加密、运行时注册、重启。
- `TestStructuredInputQRImportDuplicateAndPermissions`：验证码/扫码/导入、重复输入及操作人隔离。
- `TestFailedUpdateAndIdentityMismatchPreserveOldCredentials`：身份不符、数据库失败、Check 失败不覆盖旧状态。
- `TestTemporaryStagingOwnershipSurvivesTerminalUntilCleanup`：同账号清理期排他、重启清理暂存且保留正式代次。
- `TestProfileMySQLExplicitMigrationConcurrentCASAndRestart`：只在独占创建的测试库迁移，12 并发 CAS、重启及无明文资料。
- `TestRemoteRealBrowserViewportInputPopupIdentityAndOwnership`、`TestRemoteRealBrowserDisconnectCancellationAndScope`：真实 Chromium 合成页面的输入、截图尺寸、弹窗、身份、控制权和资源回收。
- `TestAccountsUIRealBrowserDraftLoginIdentityAndSecretFields`、`TestAccountsUIRealBrowserNullFieldsAndLateResponseOwnership`：真实 OA DOM 流程、无字段方式、敏感输入、旧画面/旧进度和关闭后迟到响应。
- `TestProcessRetentionBoundAndPasswordOptOut`、`TestShutdownReturnsWithinDrainBudget`、`TestLoginOutcomeNotificationsAreSanitizedAndNonblocking`：有界保留、密码退出保存、关停和通知。

浏览器回归通过 `PAPA_EGRESS_BROWSER_BIN` 指定本地 Chromium。MySQL 测试使用 `PAPA_ACCOUNT_ONBOARDING_MYSQL_DSN` 或 `PAPA_RUNTIME_TEST_CONFIG`，只创建并清理随机命名的独立测试库，不迁移连接配置原本指向的业务库。真实网站的登录协议和权限验收仍属于业务模块。
