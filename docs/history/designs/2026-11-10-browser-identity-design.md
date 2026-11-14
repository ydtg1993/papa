# 浏览器身份管理与一致性诊断设计

<!-- version-scope -->
> **适用版本/阶段：Papa v4 演进过程中的历史工作区快照（具体基线以正文为准；未记录精确发布版的条目不推定版本号）。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 本文为设计/计划记录，不能仅凭计划步骤或复选框认定已实现或已验收。 当前使用方式以 [v4 手册导航](../../README.md) 为准。
<!-- /version-scope -->


状态：首期已实施并通过验证，继续使用 Go + Rod。当前用法见 [浏览器身份手册](../../accounts/BROWSER_IDENTITY.md)，结果与边界见 [验证记录](../reviews/2026-11-10-browser-identity-verification.md)。以下保留设计时的代码基线与决策。

日期：2026-11-10。核对基线：Papa 工作区 HEAD `1cbda61`（v4.5.1），Rod `v0.116.2`。保留单实例架构、现有账号/Cookie/Session、任务去重、请求治理和代理健康；不恢复 Agent，不执行 Git 写操作。

## 1. 代码复核结论

| 原提案中的判断 | 核实结果与修正 |
| --- | --- |
| 页面创建已统一 | 框架的 NewPage/WithPage/FetchOnce、Record、Capture 和账号 WithStatePage 已汇入 [newPageBaseContext](../../../pkg/browser/rod.go)。原始 Rod API、弹窗和独立 target 不在此保证内；现有启动诊断仍直接调用 Rod.Page。已有页面还可通过 [ConfigurePage](../../../pkg/browser/navigation.go) 再次修改设备和请求头，必须同时保护这一入口。 |
| Emulate 只改 navigator | 不准确。锁定版本的 Rod.Emulate 调用设备尺寸、触摸和 SetUserAgent；后者发送 Network.setUserAgentOverride。Device.UserAgentEmulation 填写 UA 和 AcceptLanguage，但没有提供完整的 UA Client Hints 元数据。问题是缺少统一派生和覆盖冲突校验，不是完全没有网络 UA 接线。 |
| PageIdentity 可作 Session ID | 不准确。[core.PageIdentity](../../../core/identity.go) 是页面 URL 的规范化去重身份；[htmlfetch.Session](../../../pkg/htmlfetch/session.go) 当前只是独立 CookieJar，没有可跨重启引用的稳定 ID。不能用 URL、Cookie 哈希或指针地址替代会话键。 |
| 直接复用 ManagementState.Version 保存档案绑定 | 有在途任务风险。[ValidateAttempt](../../../pkg/account/pools.go) 比较 ManagementVersion；普通配置保存如果增加该版本，会使旧 Attempt 返回 ErrAttemptChanged。身份配置的版本必须与账号失效/停用版本分开。 |
| 网站挑战当前总是 Unknown | 不准确。defaultClassify 依状态返回 Expired（401）、Forbidden（403）、Valid（正常 2xx）或 Unknown，未识别挑战标记。挑战可能被当作正常内容或认证失败。框架也已有 [RestrictedReason](../../../pkg/htmlfetch/restricted.go)、[RestrictedError](../../../engine/restricted.go) 与 [不可重试错误契约](../../../core/errors.go)，应复用。 |
| IsAttemptFailure 是触发换号的错误集 | 不完全准确。它主要参与阶段立即重试的停止判断；实际失效报告发生在账号请求包装及 runAttempt。SiteChallenge 既不能进入账号失效报告，也必须通过独立的 Retryable=false 契约阻止本次立即重试。 |
| 导航完成后检查即可 | 会漏掉挑战导致的加载/选择器超时，以及 API 响应中的挑战。必须导航前注册观测，响应后分类；先于业务成功判定和认证失效判定，并处理加载失败路径。 |
| stealth 与状态恢复是同一种暂停脚本 | 两者都可用新文档注入，但机制不同：状态恢复使用命名隔离世界、一次性 Debugger 握手；go-rod/stealth 在新文档注入普通脚本，没有同样的暂停协议。可以共存，不能直接共用一次性恢复脚本的清理逻辑。 |

## 2. 方案选择与首期范围

采用“稳定身份档案 + 统一页面初始化 + 可选适配器 + 本地诊断”。只增加 stealth 开关不能解决身份绑定和一致性；引入外部指纹浏览器服务会扩大运行时依赖，本期不采用。

首期交付：版本化档案、账号与显式匿名会话绑定、受管页面的导航前应用、网站挑战独立分类、OA 档案管理和本地一致性诊断。HTTP 的 TLS/HTTP2 指纹模拟、自动验证码服务接入、随机化字体/Canvas/WebGL、真实站点专用规则不在首期。

新增能力默认关闭。未指定身份时保留现有 Device/Headers/Cookies 行为；已启用但配置不兼容或初始化失败时明确失败，不能静默改用另一种身份。

## 3. 档案、实际环境与观察结果分开

`pkg/browser/identity` 定义三类数据：

- `Profile`：版本化的声明，包含档案 ID、版本、UA 生成策略、语言/locale、时区、viewport/screen/DPR、触摸设置、适配器 ID/版本及开关。首期只支持与实际 Chromium 和操作系统兼容的模板；不把 Chrome 模拟成另一种浏览器内核。
- `ResolvedProfile`：依据声明和实际浏览器版本解析出的只读快照，包含实际 UA/Client Hints 等导航前设置。所有 map/slice 在边界深拷贝；不能保存调用者可继续修改的指针作为运行状态。
- `Observation`：只读的实际环境与诊断结果，包括硬件并发、平台、受限字体样本检测、Canvas 摘要、WebGL renderer、浏览器版本及检查器版本。它不进入凭据存储，也不反向自动修改 Profile。

UA、Accept-Language、navigator 相关值和 UA Client Hints 从同一声明与实际环境派生。使用浏览器支持的 CDP UA override/metadata、locale、timezone、device metrics 和 touch 接口；缺少必要能力时报不兼容。具体选择 Network 或 Emulation 域按锁定 Rod/Chromium 能力验证，不能把某一个域名当成一致性保证。

已启用 Identity 时，不允许普通 Headers/RemoveHeaders、Device、账号 UserAgent 或启动 UA/language 参数再次覆盖身份字段。兼容迁移可接受语义一致的重复声明并归一；冲突在导航前明确指出来源。Authorization、业务请求头和 Cookie 继续按原隔离规则处理，不能因合并身份步骤而丢失。

字体只探测框架固定的有限候选集合，不承诺枚举完整系统字体。Canvas/WebGL 摘要用于前后比较，不承诺跨系统、驱动或浏览器升级保持不变。浏览器版本、操作系统、检查器版本变化均显示为环境变化，不能把它们自动归因为配置错误。

## 4. 持久化与绑定

绑定粒度为账号 `(site, account_id)`，同账号跨池共享一份绑定；匿名业务必须提供显式、稳定的 `(site, session_key)`。不改造 CookieJar，不从页面去重键推断会话，不为每个匿名请求自动建档案。没有稳定会话键时，可使用显式 Profile 做单次操作，但不能声称其拥有跨重启的会话绑定。

`session_key` 是业务提供的非敏感字符串，例如已有业务会话记录的主键，或业务在创建逻辑会话时生成并持久保存的 UUID；业务负责在同一站点内唯一标识逻辑会话，并在跨任务、进程重启或重建 `htmlfetch.Session` 对象时复用原键。框架不自动为 `htmlfetch.Session` 增加或生成 ID。业务显式传入 `(site, session_key)` 解析身份绑定，原有 `htmlfetch.WithSession(ctx, session)` 仍只负责注入 CookieJar；复用键不自动恢复 Cookie，Cookie 的保存与恢复继续走原有业务/凭据存储。同一逻辑会话沿用原键，新逻辑会话使用新键，不把 Cookie、Token 或它们的哈希作为键。

匿名持久绑定入口要求非空会话键，缺失时明确报错；“显式 Profile 单次操作”是另一条显式调用路径，只在本次受控调用内固定身份，不创建 `session` 类型绑定，也不作为绑定失败后的静默回退。

建议新增两个框架表，而不是把档案塞入账号管理 JSON：

| 实体 | 保存内容与规则 |
| --- | --- |
| `crawler_browser_profiles` | 站点范围内的 Profile ID、不可变版本、声明 JSON、schema_version、操作元数据。新版本追加写入；expected_version 与唯一约束保证并发创建只有一个成功。已有版本不被原地改写。 |
| `crawler_browser_identity_bindings` | site、subject_kind（account/session）、subject_key、活动档案引用、待生效引用、独立 binding_revision、状态和更新时间。更新采用 CAS；不保存 Cookie、Token、完整代理 URL。 |

档案可以被多个同站主体引用，绑定彼此独立；不同账号不要求每一个可观察字段都不同。修改模板不会自动改绑所有账号。首期不自动清理历史版本；被活动/待生效绑定或在途快照引用的版本禁止删除，停用/删除同样保留版本冲突保护。

相较复用 ManagementState，这多一张独立绑定表，但保持了已验证的账号失效版本语义，也能服务不属于账号池的匿名会话。GORM 模型与 store 适配放在持久化层，Profile 定义、校验、CDP 应用不依赖 GORM。迁移仍是显式命令，构造 store 或普通查询不得执行 DDL。

## 5. 版本生效、认证状态与在途任务

每次任务执行冻结 `(ProfileID, ProfileVersion, BindingRevision, ResolvedProfile)`，随后创建的页面都使用该快照。不能在每次请求时重新读取“最新档案”。独立账号 API 的一次调用也固定快照；池外显式 `Manager.Do` / `WithBrowser` 使用 `account.Key`，仍属于 `account` 类型绑定，与是否加入账号池无关。

新增进程内身份使用引用，用于等待旧身份自然结束；它不占账号 MaxConcurrent 名额，不替换已有认证单飞或资源许可。引擎在任务执行准备/结束处自动获得与释放；低层接口提供有界作用域回调，避免要求现有无 Close 契约的 `Pools.Acquire` 调用者突然承担释放职责。取消、错误、panic、准备阶段重试和未认领退出都必须释放。

切换规则：

1. 创建新 Profile 版本不影响现有绑定。
2. OA 显式请求改绑，CAS 写入待生效引用。该主体停止接纳新的身份使用，等待已有引用自然结束；等待不占全局任务许可。
3. 排空后，在新档案下执行有时限的兼容性与认证复验，再发布新绑定和对应浏览器状态。不能让新档案直接读取、改写旧档案正在使用的浏览器状态。
4. 复验失败保留旧活动绑定与待生效原因，保持该主体的新工作等待，允许取消改绑或修正后重试；不能把配置不兼容当成账号失效触发轮换。原始凭据不因改绑被删除。
5. Cookie/浏览器状态仍使用现有存储；只增加其对应身份版本的元数据。既有无身份元数据的状态只有在显式启用身份并复验成功后才标记，不迁移或重写历史任务 ID。
6. 进程重启时恢复活动/待生效引用；没有在途引用也不跳过认证复验。运行时浏览器升级导致的实际差异需记录，并按兼容性规则复验；不保证声明不变时指纹字节也不变。

`PageOptions.Identity` 只传递已解析的只读快照。账号身份由任务执行准备或 Manager 的受控作用域解析，再填入 PageOptions；context 可携带已有 Attempt/作用域引用，但不成为第二份可修改档案来源。档案切换、账号停用和真实失效是不同事件；账号停用/失效仍沿用原有版本失效规则。

每次借用浏览器时检查实际运行时与冻结快照是否兼容；在一次任务内发现二进制版本等条件不兼容时返回身份环境错误，不能偷偷重新解析成另一份身份。身份改绑等待发生在任务认领与全局执行许可之前，网络复验不持有引擎全局锁或数据库事务。

绑定数据库与既有凭据 Store 不假设具备跨存储事务。发布新活动状态前核对持久写入结果；崩溃后通过待生效引用和凭据身份元数据恢复，任何版本不匹配均先停用该次身份使用并复验，不把新身份状态误装到旧档案上。

## 6. 页面初始化与适配器

在 `newPageBaseContext` 改造原有设备/请求头步骤，统一执行：

```text
创建空白受管页面
→ 校验并应用冻结身份（未启用则走旧设备配置）
→ 合并普通业务请求头和 Cookie
→ 安装可选的新文档适配器
→ 安装现有请求治理与资源拦截、挑战观测
→ WithStatePage 按需安装一次性状态恢复
→ 首次导航与站点脚本
```

`ConfigurePage`、NavigatePage 和已有页面的 Capture 入口必须识别页面已绑定身份。允许继续操作相同身份；拒绝中途更换身份、Device 或受保护请求头。原始 Rod 对象上的业务直接修改无法由框架完全拦截，必须在能力边界中说明。

适配器在 Go 进程内使用固定版本的嵌入脚本，不要求部署 Node。`go-rod/stealth` 是候选来源，不直接调用其 Page 工厂；直接另建页面会跳过 Papa 的受管初始化。

适配器声明会修改哪些字段。首期明确由 Profile 管理的字段及字体/Canvas/WebGL 等只读诊断字段，不能被整包脚本偷偷覆写；无法关闭冲突模块的脚本不纳入首期兼容适配器。诊断分别记录脚本注册状态和可验证的初始化结果，不能把 CDP 注册成功等同于全部脚本已成功执行。

身份/适配器脚本与一次性 localStorage 恢复分别管理 script ID、world 和清理职责。状态恢复脚本自删时不能移除持续的新文档适配器，适配器也不能占用恢复握手的 Debugger 暂停协议。任一必需步骤失败时关闭新页面，返回结构化初始化错误，保留旧凭据。

首期保证框架受管 page target 的主文档；同目标 frame 的覆盖需独立验证并如实报告。新 popup、跨进程 iframe、独立 worker 和业务原始 Rod 页面不自动纳入保证，不能声称“所有浏览器页面都覆盖”。

## 7. 网站挑战独立分类

复用 core 错误分类与不可重试契约，增加传输中立的 `SiteChallengeError` / `ErrSiteChallenge`，类别为 `site_challenge`，`Retryable() == false`。HTTP 路径不应为了引用此错误而依赖 browser 包。

| 结果 | 处理 |
| --- | --- |
| Expired | 原有账号失效报告与替补调度 |
| 账号 Challenge / ErrChallenge | 原有人工验证/暂停语义 |
| SiteChallenge | 本次任务失败并停止立即重试，记录脱敏证据；不调用账号 ReportFailure、不修改账号凭据/选择版本、不清空有效 Cookie |
| 429 / 有效 Retry-After | 原有站点冷却处理，不能被身份模块绕过 |
| 代理连接/认证故障 | 原有代理健康处理；目标挑战不累积为代理本身故障 |

SiteChallenge 不加入 `account.IsAttemptFailure`。`Retryable=false` 只阻止当前执行内重试，既有错误队列按 failed 状态和补偿上限仍可能以后重投；不把它描述为永久禁止重投。站点已有熔断规则继续适用，本期不另造自动切号/切指纹策略。

检测分两层：明确响应证据自动分类；页面文本等启发式证据只作为建议或由业务显式确认。复用 RestrictedReason/RestrictedError 的已有业务入口，不将词表中的普通验证码字样升级成全局必定拦截。

明确的协议证据（例如 Cloudflare `cf-mitigated: challenge`）在账号 401/403/2xx 分类之前处理。不能把普通 401、403、HTML 响应本身判为网站挑战；外部响应标记只能说明遇到对应挑战响应，不能证明整个站点由某厂商保护。

浏览器响应监听在导航前安装，优先检查主文档及当前操作所选择的响应。挑战已明确时，即使后续 WaitLoad/WaitSelector 超时也保留挑战分类；第三方子资源的挑战不能直接使整个任务失败。已有 Fetch/CDP 治理监听不得被新处理器覆盖。

CaptureRequest 默认可以在请求发出时成功返回，这时尚未观察到响应，不能承诺已经排除挑战，也不静默改成等待响应。其 WaitLoad/受管账号路径与 CaptureResponse 分别定义可观察范围；返回值和诊断需区分“已观察响应”与“仅观察请求”。自定义业务 Login/Check 内部的请求继续由业务使用框架受管 API，框架不能修正任意业务回调内部的错误推断。

## 8. 诊断与 OA

保留现有启动诊断，用于排查 Chromium 启动和 Rod 连接；新增“配置一致性检查”模式，使用受管 Pool/Page 和完全相同的身份初始化入口。不能直接把当前原始 Rod about:blank 诊断结果当成身份已生效的证明。

身份诊断创建独立临时环境与本地合成服务，服务仅监听回环地址，只允许当前诊断的随机 origin/路径，拒绝外部重定向；这份出口授权仅对诊断实例有效。只加载身份声明和允许展示的环境设置，不加载账号 Cookie、Token 或 localStorage，不调用业务登录回调，不修改代理健康状态。

所选账号使用专用浏览器池时，诊断按该池的实际二进制和允许复制的环境配置创建实例；无法复现的条件明确列为未验证。不能使用公共池的不同配置得出该账号生产环境已经一致的结论。诊断不验证目标站网络出口信誉或认证是否有效。

HTTP 回显端只返回身份相关请求头白名单；页面最早的内联脚本采集 navigator/屏幕/语言等值。Client Hints 检查在支持的安全上下文进行，区分缺失、不支持与实际不一致；不把 CH 品牌列表的 GREASE/顺序差异直接判错。字体、Canvas、WebGL 检查可能不可用，此时显示不可用或无法比较，不伪造通过。

报告展示：配置值、解析值、实际值、检查器版本、实际浏览器版本、差异原因、适配器注册/执行结果以及覆盖范围。报告标题固定为“配置一致性检查”，不提供反爬通过率分数。采样报告保留有界摘要，仍沿用原有脱敏、单次诊断互斥、执行/清理超时。

OA 在现有浏览器诊断页管理档案版本和执行检查；账号池成员详情显示 Profile ID/版本、待生效状态、最近诊断摘要，并链接到对应档案。池本身不拥有唯一 Profile。所有配置写操作继续鉴权、CAS、草稿保留与操作审计；GET 不创建浏览器、不登录、不做诊断。档案更改不自动改绑账号，批量改绑不在首期。

## 9. HTTP 与业务边界

浏览器受管路径应用 Profile；本期不自动把浏览器 Profile 写入 HTTP 客户端。HTTP 保留现有账号 UA/请求头配置，Go 的 TLS/HTTP2 特征也不因 Profile 改变。诊断必须区分“HTTP 身份头未联动”和“传输层指纹未模拟”。本期不引入 uTLS，不改变 egress、代理反馈与共享请求预算的传输契约。

框架只保留通用协议分类、可配置适配器和本地合成 fixture。真实站点特征、验证码服务凭据、目标站通过率验收放在独立业务模块。本期不要求外部浏览器服务，也不承诺稳定通过 Cloudflare/DataDome。

## 10. 验收要求与文档同步

以下验收要求已执行，对应测试及边界记录在上述验证记录中：

1. 同一绑定跨任务/重启读取相同档案版本；匿名会话重建 Session 对象但复用原业务键时仍命中原绑定，新键不复用旧绑定；缺键的持久绑定调用报错，显式 Profile 单次操作不写会话绑定。并发保存/绑定只允许一个 CAS 成功；未知版本与损坏记录明确失败。
2. 合成页第一段同步内联脚本与服务端首个请求头已符合身份声明。仅在 DOMContentLoaded 检查不足以证明导航前生效。
3. 两账号使用明确不同的测试档案时互不串值；并不要求真实生产账号的所有指纹必须不同。
4. 改绑等待旧引用结束；旧任务保持旧快照；待生效复验失败不轮换账号、不丢凭据；取消/panic/等待阶段退出不泄漏引用或许可。
5. Profile 与普通 Header/Device/ConfigurePage 冲突被拒绝；未启用身份的旧行为保持兼容。
6. 适配器和 localStorage 恢复同时启用，覆盖首次脚本、同源重定向、重复导航、清理及初始化失败；受保护字段没有被脚本覆盖。
7. 网站挑战在 HTTP 200/401/403、浏览器主文档/选定响应及加载超时情形正确分类；不会报告账号失效、不会立即重试。普通 401、账号人工验证、429 与代理故障各走原路径。
8. CaptureRequest 早返回语义保持不变；第三方资源挑战不误伤主任务；错误队列以后补偿的行为有明确回归。
9. Profile/适配器开启后，受管 HTTP/浏览器限速、代理出口、资源放行、取消及账号隔离回归仍通过；任务去重不加入 Profile 或账号维度。
10. 诊断与生产页面走相同初始化逻辑，最早脚本与回显头吻合；不发送凭据、不访问真实站点、不扩大生产出口授权。
11. 两张新表做真实隔离 MySQL 迁移/CAS 验证；Go 单元/集成/race/vet/build、真实 Chromium 合成页、脚手架生成验证按改动范围执行。

实施时同步 ACCOUNT_RUNTIME、CORE_CONFIG、MONITOR、RUNTIME_CONTROL、FETCHER_WRITING_GUIDE、DATA_LANDING_GUIDE、DEVELOPMENT 和脚手架配置。本文不替代当前用户手册，也不将计划能力写成现有功能。

## 参考来源

- [Rod v0.116.2 Page.Emulate / SetUserAgent](https://github.com/go-rod/rod/blob/v0.116.2/page.go)：设备模拟同时设置尺寸、触摸和网络 UA。
- [Rod v0.116.2 Device.UserAgentEmulation](https://github.com/go-rod/rod/blob/v0.116.2/lib/devices/device.go)：UA/AcceptLanguage 派生，没有完整 CH metadata。
- [go-rod/stealth Page 实现](https://github.com/go-rod/stealth/blob/main/main.go)：新建 page 后通过 EvalOnNewDocument 注册脚本；引用用于机制核实，不代表已选定依赖版本或承诺检测效果。
- [Cloudflare 挑战响应标记](https://developers.cloudflare.com/cloudflare-challenges/challenge-types/challenge-pages/detect-response/)：明确响应证据的通用示例。
