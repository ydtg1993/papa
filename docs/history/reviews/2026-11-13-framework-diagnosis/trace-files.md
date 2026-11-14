> 当前兼容性约定已由 [v4 独立清理](../2026-11-13-v4-independent-cleanup.md) 取代：旧 enabled、SQL 模型及旧查询接口全部删除。本文以下保留各批次验证历史。

# Trace 文件存储实施与验证

<!-- version-scope -->
> **适用版本/阶段：v4 文件 Trace 切换阶段的实施快照；其中旧接口兼容约定已由独立 v4 清理替代。**
> 文件存储方向继续适用，但旧 enabled、SQL 查询回退及手动追踪不再存在；当前三模式与文件限额见 [配置手册](../../../configuration/CORE_CONFIG.md)。 当前使用方式以 [v4 手册导航](../../../README.md) 为准。
<!-- /version-scope -->


当前采用 `crawler.trace.mode: off/errors/all`，默认 errors。用户已取消主动按钮及待选状态；旧手动追踪方法已彻底删除。旧 YAML enabled 无 mode 时兼容映射。

日期：2026-11-13。用户确认改为纯文件、默认异常追踪、预留主动追踪入口、默认七天清理。本记录取代历史 D9 的 SQL 存储方案；[MySQL 验收](trace-mysql.md) 仍作为旧版本验证证据保留。

## 实现结果

- Engine 的 Trace 不再读写数据库。常规迁移、启动 schema 提示及通用数据后台目录排除旧 Trace 表。旧表不删除、不自动导出，不回退读取；旧模型/检查函数仅保留源码兼容。
- 默认保留 handler 错误、panic、显式 Fail/Warn 所在尝试；干净成功不产生 Trace 文件。失败重试后成功不会抹掉失败证据。正常取消本身不触发保留，超时仍作为异常。
- 全量模式保留成功及同轮重试；空尝试补完成标记。off 不分配 Trace。errors 的取消/失败边界、容量和默认七天清理不变。
- `internal/tracestore` 拥有版本化文件、容量、受限路径、原子发布、读取与过期清理。任务 ID 分片目录内按身份/执行分组，每次尝试独立发布，避免追加半条 JSON 或反复重写整个执行。
- `Trace.Step/Fail/Warn`、`ListTrace`、`ListTraceExecution` 调用方式及公开值布局保留。后台明确展示最新保留证据，而非当前任务状态。
- 新增 `crawler.trace_files`；默认目录 `./traces`、单文件 4 MiB、总量 256 MiB、10000 文件。尝试缓冲最多 500 步/约 512 KiB，数据单步 16 KiB，超限标记截断。保留期开关仍默认七天、负数停用清理。

## 复查中修正

1. 损坏文件不能阻断其余过期文件清理，逐文件汇总错误；清理逐文件释放锁，允许写入穿插。
2. 调低预算后仍可在有限恢复上限内读取/清理已有文件，不因超过新写入额度永久卡死。
3. 使用当前 `identity_hash`，标准哈希直接作为目录组件；不依赖已弃用的 `idempotency_hash`。
4. 真实超时不能与正常取消一起被丢弃；主动追踪空步骤成功也生成最小完成记录。
5. 有界数据预检保留原 JSON 编码语义；限制普通输入遍历，序列化 panic 转成证据标记，序列化置于 Trace 锁之外。

## 全仓验证额外发现：浏览器创建中断清理

首轮普通全仓测试的 `TestWithStatePageCleanupAfterFailureCancelAndPanic/navigation` 报 `state page leaked`；同一用例随后连续十次通过，原失败与具体缺陷的因果关系尚未证实。

检查发现 Rod 在 target 已创建、attach 取消时使用原已取消 context 清理。新增 `TestPageCreationCancellationClosesPartialTarget` 在修复前确定性报 `created=true closed=false bounded=false`，修复后通过。修复归 `pkg/browser/page_creation.go`：创建后立即拥有 target，失败时用独立三秒清理 context；保留原取消错误，不改变成功导航。该用例 race、完整清理场景及真实导航连续五次通过。

## 验证

分包测试覆盖文件发布、配额、并发、取消、重启读取、损坏、过期清理、异常判定、主动选择、成功重试、旧 SQL 表不注册、公开快照布局与后台文案。

- 最终普通 `go test -json ./...`：78 个包通过，5 个包无测试；2860 项测试/子测试通过、91 项跳过，无失败。
- 首轮全仓 race：78 个包通过，2859 项通过、91 项跳过；该批次尚不包含额外的浏览器创建清理回归。
- 浏览器修复后的并行全仓 race 出现 `TestAccountBrowserHTTPBridgeRejectsUnverifiedToken` 的 `unverified bridge token accepted: account: login-failed`；未报告数据竞争。同一用例独立 race 连续三次通过，底层原因被账号错误脱敏，不能据此认定是超时或 Trace 回归；不为消除偶发结果改动账号生产逻辑。最终 `CGO_ENABLED=1 go test -race -p 1 -json ./...` 通过：78 个测试包、2860 项测试/子测试通过，91 项跳过，5 个包无测试；无失败或数据竞争报告。串行的是包级执行，包内并发测试仍然运行。成功包可能复用 Go 测试缓存；Engine 和此前失败的账号包已完成本轮执行。
- `go vet ./...`、`go build ./...`、gofmt 与 diff 空白检查通过。CLI 测试包含生成项目、go mod tidy、go build 与声明检查。
- 跳过项主要为需要显式环境的 MySQL、诊断浏览器及平台用例；本轮未连接或改动业务库。日志在 `.review-tmp/trace-files/`，保留首轮失败与复跑结果。

## 剩余边界与未验证项

- 尚在内存的尝试遇进程硬杀会丢失；本轮不实现成功路径临时落盘或崩溃 WAL。
- Sync/Close/Rename 提供文件发布完整性，不承诺断电级目录持久性。未做断电、真实磁盘满、生产长期高负载及网络文件系统实验。
- 目录由单个 Store 独占；外部改写文件需停组件。崩溃 `.pending` 和损坏文件保守保留、计入配额，需人工排查。超出恢复盘点上限需先恢复足够配置。
- 自定义 JSON/Text 序列化方法属于业务代码，其执行时长、瞬时分配无法由框架硬限额；返回内容受限，panic 被隔离。不要向 Trace 传入大对象或有副作用的序列化器。
- 文件 I/O 同步纳入 worker 收尾，慢盘可能增加关停耗时。context 可取消锁等待和操作之间的工作，不能强制中断 OS 文件调用。
- 容量满时拒绝新证据并记录告警，任务继续原有结算；Trace 不是可靠事务日志。旧 SQL 历史无自动迁移到文件路径。

框架画像仍是通用单实例爬虫框架：任务事实与结算留在数据库，调度使用有界内存工作集，Trace 是有限、按异常保留的文件证据组件。此次不改变任务原子性、调度锁序或数据库排空协议。

## 三模式收尾验证（2026-11-13）

- 全量普通测试通过：2871 项通过、91 项按环境条件跳过，78 个包通过。之后补充全量取消回归并再次运行 Engine 全包测试通过。
- 最终全量 `go test -race -p 1 -json ./...` 通过：2872 项通过、91 项跳过，78 个包通过、5 个无测试包，无竞态报告。日志 `.review-tmp/trace-modes-race.jsonl`。
- `go vet ./...`、`go build ./...` 通过；cmd/papa 的生成项目构建测试通过。
- 默认启用错误模式后，合成持久化测试使用 t.TempDir 隔离 Trace 文件；本轮中间产物已移入 `.review-tmp/trace-modes-generated-traces`。
- 未连接或改动业务数据库，未执行 Git 写操作；保留已有改动及用户暂存的图。未新增真实生产负载/断电演练，原文件存储故障与容量边界仍适用。
