# Papa 能力层、配置与基础契约诊断（2026-11-13）

<!-- version-scope -->
> **适用版本/阶段：Papa v4 演进过程中的历史工作区快照（具体基线以正文为准；未记录精确发布版的条目不推定版本号）。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 当前使用方式以 [v4 手册导航](../../../README.md) 为准。
<!-- /version-scope -->


> 当前 v4 契约与最新验收见 [最终框架画像](v4-final-portrait.md)；本文保留对应历史审查阶段的证据，不表示旧 API/SQL Trace/旧库迁移仍受支持。

> 本文保留修复前诊断证据与当次验证。D1–D9 后续修复和最终验证见 [修复记录](remediation.md)。
审查依据：工作区 AGENTS.md、docs/package_structure/PKG_STRUCTURE.md、ROOT_STRUCTURE.md，以及当前源码。没有改生产代码、既有测试或 Git 状态。后续获主审授权，新建两个唯一命名的临时测试文件复现下述问题，跑完已删除；输出保留在 `.review-tmp/diagnosis-capabilities-reproduction.txt`。没有执行破坏性大响应/OOM 验证，也没有声称完成真实浏览器、代理、MySQL、ffmpeg 验收。全仓测试/race 由主审统一执行。

## 定向复现证据

执行：`GOWORK=off go test ./pkg/middleware/filedown ./pkg/middleware/m3u8 -run '^TestDiagnosisCapabilities' -count=1 -v`，退出码 1。测试按预期正确行为断言，以下三项均失败，证明缺陷可复现。

- filedown 和 m3u8 各运行 `TestDiagnosisCapabilitiesProxyOverlayTimeout`：通过生产 `NewDownloader`、`With` 及实际代理客户端选择函数，先用 7s 缓存同一代理，再用 1s 覆盖实例取客户端。两包均输出 `base timeout=7s derived config=1s derived proxy client=7s shared client=true`；失败原文 `overlay timeout ignored: actual=7s want=1s`。此测试检查实际 client 对象，不发网络请求。
- filedown 的 `TestDiagnosisCapabilitiesMergePreservesOldArtifact`：本机合成 HTTP Range 源、生产断点保存/读取/Download/合并函数，构造第二片缺失且断点已记完成的状态。下载确实返回缺片错误，旧文件 `PREVIOUS-GOOD-ARTIFACT` 从 22 字节变成 20 字节 `NNNNNNNNNNOOD-ARTIFA`；失败原文 `failed merge destroyed previous artifact: old bytes=22 actual bytes=20`。只使用测试临时目录，没有接触用户产物。

临时文件 `pkg/middleware/filedown/diagnosis_capabilities_temp_test.go` 和 `pkg/middleware/m3u8/diagnosis_capabilities_temp_test.go` 已删除，不会影响后续全仓测试。

## 确认问题

### P2：共享代理客户端缓存使下载覆盖层的超时失效

- 位置：`pkg/middleware/filedown/downloader.go:848-869`；同类位置 `pkg/middleware/m3u8/downloader.go:152-174`。共享入口分别在 filedown 的 98-104 行和 m3u8 的 112-118 行。
- 触发：同一个 Downloader 派生两个 `With` 实例，给它们不同的 Timeout / SegmentTimeout，再使用同一个 `DownloadOptions.Proxy` 地址。第一次使用该代理时缓存完整 `*http.Client`；第二个实例只按代理地址命中缓存，直接返回第一次实例的 Timeout。
- 影响：配置自查显示的超时与实际请求超时不同；长超时实例可能被短超时实例意外截断，短超时实例也可能长期占用任务位。行为取决于谁先访问代理，在多阶段并发中不可预测。网络授权不允许 Overlay 修改，因此本问题不扩大为授权绕过。
- 已有防护：代理客户端已经设置超时，不再是“所有代理请求无超时”的旧缺陷；`m3u8/overlay_test.go:236` 的 `TestProxyClientInheritsSegmentTimeout` 只覆盖一个实例及同配置复用，未覆盖两个覆盖层共享缓存。
- 修复归属：下载器拥有请求时限，缓存应共享 transport/连接资源，逐调用生成带本实例时限的 client，或将影响客户端语义的配置纳入缓存键；不能通过分裂路径锁规避。

### P1：filedown 的分片规划和 goroutine 数不受 MaxConcurrent 约束

- 位置：`pkg/middleware/filedown/downloader.go:426-442`；远端长度来自 790 行附近 `headFile` 的 `resp.ContentLength`。
- 触发：通过网络授权的源返回很大的 Content-Length 和 `Accept-Ranges: bytes`，或使用较小的合法 ChunkSize。代码先把整个 `[0,totalSize)` 展开到 `chunks`，随后每片创建 goroutine，goroutine 内才等待信号量。
- 影响：MaxConcurrent 仅限制正在执行的请求，不能限制元数据和等待 goroutine 的内存。单次下载可以耗尽整个单实例进程的内存；规划循环没有 ctx 检查，取消也无法及时停止该阶段。例如默认 5 MiB 分块遇到极大伪造长度时，尚未发出分块 GET 即可能 OOM。此问题不依赖多实例。
- 同类观察：`pkg/middleware/m3u8/downloader.go:714-722` 也为每个片段创建 goroutine 后再取令牌；其内存规模还受播放列表本身控制。
- 已有防护：ChunkSize 非正数会回退默认值；MaxConcurrent 非正数回退为 1；HTTP 请求与重试退避响应取消；输出路径等待可取消。这些都不能约束前置分片展开和等待 goroutine 的数量。
- 修复归属：下载器内部固定 worker 数或先以可取消方式取得许可再启动工作；filedown 可直接增量生成分片，避免完整 chunks 列表，并处理极端长度下算术溢出。不能仅靠 Engine 任务数上限补救单任务的资源膨胀。

### P2：分片合并失败会破坏已有成功产物

- 位置：`pkg/middleware/filedown/downloader.go:488-504`，后续读取分片失败在 508 行附近返回。
- 触发：同一输出目标已存在一份好文件；新下载进入分片合并，断点元数据把某片记为完成但该文件已缺失/截短，或者合并中途发生读写错误。
- 影响：代码在检查所有分片之前直接打开最终路径并 `Truncate(totalSize)`，再逐片写入；失败返回时旧文件已经被截断/部分覆盖，消费者得到损坏文件。`defer outFile.Close()` 也不向成功返回值传播关闭错误。
- 已有防护：缺失/短分片会清断点并报错，已合并的分片不会提前删除；`TestMergeFailureKeepsSegments` 已构造缺片触发，但只检查错误和分片保留，不检查旧产物。普通单线程路径已经用唯一临时文件和 rename 保留旧文件（`TestDownloadSingleKeepsPreviousOnFailure`），该保护没有覆盖分片合并。
- 修复归属：filedown 合并到同目录唯一临时文件，验证、显式 Close 成功后再发布；保留现有输出路径互斥、断点恢复及分片清理顺序。

## 当前真实能力画像

| 层 | 已存在的能力与状态所有权 | 重要边界 |
| --- | --- | --- |
| 根包 | App/Engine/任务/错误和抓取 API 的别名与委派门面 | 未发现根包另建登记表或运行状态；真实站点应继续在业务模块 |
| config | 启动 YAML、默认值、未知键拒绝、运行覆盖值校验、业务段严格解码 | Load 内 Validate panic 是明确契约；Config 含可变 map，不是并发安全配置中心；不自动迁移 |
| core/models | 任务身份的版本化站点+阶段+类型+值 SHA-256；URL 身份保留 origin；模型局部 hook 和 UTC 序列化 | 旧 PageIdentity 不应当作新任务身份；models 不拥有认领/终态事务，也不涵盖所有领域表 |
| htmlfetch | 每请求超时、受限 body、HTML/JSON、Session、条件请求、离线样本 | FetchIncremental 不自动持久化；表示身份还依赖调用方账号/头/locale 作用域；渲染页没有可声称为 HTTP 200 的状态 |
| account/onboarding | Manager 认证代次、凭据、许可；Pools 选择/健康/CAS；Service 登录流程和资料生命周期 | 登录 Provider、解密密钥、业务配置由宿主；回调须合作取消；无强杀回调能力 |
| browser/identity | 进程池、隔离上下文、页面状态、请求/响应观察；身份管理作用域和绑定版本 | Pool.Close 关闭空闲实例并禁止准入，借出资源应由宿主先排空；原始 Rod 使用须遵守受管入口边界 |
| governance/egress | 请求/浏览器/任务许可、采样缩放、站点节流；DNS/IP 校验、显式私网授权、代理目标 IP 固定 | 单实例资源治理；无上下文 controller/site 的独立请求不自动纳入站点预算；授权策略不是 robots 策略 |
| discovery/extract | 链接/XML/robots 解析、分页去循环/限页/限条目、字段与唯一性校验 | 不自动爬行，不替业务解释 robots HTTP 状态、缓存或执行 crawl-delay；数据校验不替代业务模型 |
| 下载器 | 分片/续传/内容区间验证、共享目标锁、m3u8 解析/解密/产物模式/ffmpeg 本地合并 | 仍有上述资源/配置/产物发布问题；filedown 同 URL 同长度变更无 ETag 身份，已在源码明确为既定边界 |
| events/loggers/notify/temporary | 有界事件通道、诊断副本脱敏、Webhook 出口；清理意图先持久化再删除文件及提交 | 事件丢弃被计数，非可靠事务日志；临时业务数据删除事务由 CleanupStore 实现 |

## 已核实存在、不能再报旧缺陷的防护

1. `account/onboarding/lifecycle.go` 独立 lifecycleMu 管准入和 WaitGroup，关停不跨资料事务锁等待；超时不冒充排空完成。
2. `account/pools.go:133-163` 的 Shutdown 等到后台验证结束及从 Manager 摘除；候选发布持锁阻塞摘除被纳入有界等待。
3. `browser/identity/runtime/manager.go:48-103` 区分取消与排空，并用独立生命周期锁记账；依赖不能在 Shutdown false 后被提前销毁。
4. `browser/pool.go:351-465` 已用 context.AfterFunc 广播唤醒等待者；创建失败/许可失败归还 alive 并广播；不再存在“失败释放容量却不唤醒”的旧问题。
5. `htmlfetch/request.go` 已避免 MaxBodySize+1 溢出，并在返回前关闭网络 body；错误状态正文仍受大小限制。
6. `egress/http.go` 重建 wrappers 时保存代理与治理；拒绝不支持的自定义 transport；代理出口不让代理重新解析目标域名。
7. 下载锁覆盖相同及祖先/子孙输出和断点路径，等待可取消；源隔离断点，分片偏移与 Content-Range 校验，状态中的文件路径不直接作为读路径使用。
8. `events/emitter.go` Emit/Close 有读写锁防止向已关闭 channel 发送；排空超时保留 sink；map 不复制属于明确调用契约（docs/operations/OBSERVABILITY.md:34），不能单凭浅复制重复报数据竞争。

## 需要准确表述的剩余能力边界

- m3u8 `doRequestWithURL` 在没有 Range 且 chunked 未给 Content-Length 时保留无界内存读取（downloader.go:271-288、350-355），源码明确选择该兼容行为。因而不能宣称所有下载响应都受固定内存预算；如要收紧，应新增可配置的响应/播放列表/分片预算，而非暗中截断媒体。
- 代理缓存不设条目上限、不回收（filedown 源码明确说明）；动态无限代理地址不在稳定资源上界保证之内。
- OnProgress 在多个分片 worker 中调用，业务回调需并发安全、及时返回；测试已用 atomic 标注此契约。
- events 排空超时、Provider/Store 不响应取消、浏览器真实进程回收、数据库时区和网络错误需分别验证，不能从普通单元测试通过推导成生产全部验收通过。

本轮未在 config/core/models/root 的抽查路径确认新的高置信功能缺陷；这不等于对每个文件和每种输入作了形式证明。

当前收尾状态见 [闭环清单](closure.md)；本文的修复前发现和当次未验证项作为历史证据保留，不能代替最新验证结论。
