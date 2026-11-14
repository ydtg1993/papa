# Papa v4 独立版本清理记录

<!-- version-scope -->
> **适用版本/阶段：独立 Papa v4 清理阶段及正文追加的验证批次。**
> 不兼容 v3 及更早版本的边界继续适用；具体测试结果、耗时和遗留风险仅对应正文记录的代码与环境。 当前使用方式以 [v4 手册导航](../../README.md) 为准。
<!-- /version-scope -->


日期：2026-11-13。用户明确要求逐模块删除以前的兼容与废弃层，v4 不再兼容 v3 或更早版本。本文取代此前“保留公开旧接口/兼容空壳/旧配置映射”的实施约定。保留现有工作区改动，不执行 Git 写操作，不修改业务数据库。

最新结论见文末“活跃排空、持续饱和及 App 失败清理（2026-11-14）”及 [最终框架画像](2026-11-13-framework-diagnosis/v4-final-portrait.md)。此前独立 MySQL 验收、百万行测量、浏览器共享目录修复及整体收尾结果仍按各轮次记录；以最新一节为当前验证范围。

## 逐模块结果

| 模块 | 删除与统一 | 有意保留的当前能力 |
| --- | --- | --- |
| 根包 / core / config / models | 删除 Engine 对 core 的历史类型/函数转发；TaskKey 旧前缀键；Trace enabled 字段及 YAML 映射；SQL TaskTrace 模型与状态常量；任务 URLHash、IdempotencyKey/Hash。Task 使用 ResourceID 或规范 URL，Trace 仅 off/errors/all | 根包面向当前 v4 的门面别名；PageIdentity 可作显式 ResourceID；core 当前快照值；第三方模块路径中的 v2/v3 不属于 Papa 兼容层 |
| admin | 删除无 context 字符串读取适配及白名单包装；Trace 单一详细查询，无 setter/旧结果回退；Oao 旧只读适配删除；全局操作桥只读；重投强制补偿版本 | 当前 Oao 1.4 销毁/取消流程；公开装配类型；令牌轮换/补偿动作的可选能力返回明确不可用，无旧行为回退 |
| engine | 删除旧 ListTrace、NextProxy、Task.IncRetry/UpdateStatus/公共 Insert、RetryTask；采用 ListTraceExecution、带错误代理选择、内部插入及 RetryTaskVersioned；删除旧字段预热查询 | 当前版本化认领/终态/重投/补偿、原子提交和关停排空；无持久任务的直接执行、调度预算零值是当前约定 |
| internal | 删除旧身份回填与旧索引删除迁移、Trace schema检查；熔断删除 LegacySnapshot/UpgradeRecovery、近似旧计数。CheckTaskSchema 在启动/迁移写入前拒绝旧列、旧索引、缺身份列或非法身份数据 | 当前 v4 精确窗口、未结算失败和追加历史恢复；运行策略历史格式不匹配明确拒绝；App 只装配，schema 校验归 database |
| pkg | 删除 proxy.Next、无效 RampConcurrency、account GORM 转发、strategymigrate、SessionGuard；账号缺版本状态拒绝；filedown 只读当前 v2，下载无版本状态拒绝 | account/contract 共享契约；gormstore 独立存储；Session/WithSession 被当前 account 使用；m3u8 自身仍写 v1/v2，二者均保留；Chrome flags 和协议/平台支持 |
| obs | 删除把缺失模式解释为旧报告的描述；缺字段报告明确“未指定”；事件日志仅接受当前 v=1，其他版本计坏行并继续读取 | 当前历史窗口查询、流式读取、证据不足提示，不能将缺数据猜成已恢复 |
| cmd | 删除 strategy-migrate、旧目录布局回退、旧 QueueSpec 升级检查、旧模型密钥/检查目录忽略项；模板只生成 v4 入口 | 当前 new/check/migrate/upgrade/debug/act/report/token；report --run 是实际筛选；旧模块拒绝保护防止向 v3 工程写 v4 文件 |

## 状态与安全边界

- 任务身份仅由 current identity_type/value/hash 表达；旧表即使已经补过 identity 列，只要仍含 url_hash/idempotency_key/idempotency_hash 或旧唯一索引，也拒绝使用，避免 NOT NULL 旧列及旧约束破坏新写入。
- 普通启动执行只读 schema/数据检查，拒绝后关闭刚建立的数据库连接，不创建 Engine；显式迁移同样先检查，不偷偷转换或删除旧数据。未做业务库迁移。
- 熔断仅接受 Control.Version=1 且有精确 Window 的快照；旧裸报告、未知版本或缺失窗口明确报错，不从展示报告推断控制准入。快照与历史仍同事务保存。
- 任务重投始终校验 reprocess 和 compensation_version；取消、终态 CAS 与排空边界没有放宽。
- Trace 保留默认 errors/七天清理及文件容量约束。旧 enabled 键现在报未知配置，不再映射；没有手动追踪接口或按钮。

## 验证与发现

新增/调整回归包括 TestTraceRejectsRemovedEnabled、TestMigrationRejectsPreV4SchemaBeforeWrites、TestCurrentTaskColumnsRejectBackfilledOldTable、TestTaskIdentityDataRejectsMissingValuesAndPropagatesFailure、TestDecodeRejectsUnsupportedSnapshotFormats、TestBreakerRejectsUnsupportedSnapshotBeforeRestore，以及版本化后台重投、Oao 生命周期、下载旧格式拒绝和账号当前版本重启测试。

- 定向验证发现去重预热仍选择旧列，真实输出 `SQL missing "`idempotency_key`"`；已移除生产查询和旧断言，当前模型/假库列清单同步。
- 收尾复核发现旧表残留 NOT NULL 列/唯一索引与 identity_type=NULL 漏检；已补只读拒绝和测试，未执行真实数据库 DDL。
- 后台空记录文案更新后，旧断言未同步；已更新。
- 浏览器测试一次报 `navigate: navigation failed: net::ERR_UNSAFE_PORT`，修复合成夹具仅放行其本地服务端口，生产浏览器配置不变。另早期并行浏览器测试一次 `state page leaked`，独立三次复跑和后续全量浏览器测试通过，未改页面清理算法。
- 最终独立模块检查均使用 `GOWORK=off`：`go test -p 1 -json ./...` 通过，77 个包通过、2856 个测试通过、89 个测试跳过，零失败；日志 `.review-tmp/v4-cleanup-verified.jsonl`。
- `CGO_ENABLED=1 go test -race -p 1 -json ./...` 通过，77 个包通过、2855 个测试通过、89 个测试跳过，零失败；日志 `.review-tmp/v4-cleanup-race.jsonl`。全量 race 启动后新增的事件版本测试及调整的账号浏览器夹具，另以 `go test -race ./obs/eventlog ./pkg/account -run 'TestReadRejectsMissingZeroAndFutureVersionsAndContinues|TestAccountBrowserBridgeRetriesHTTPExpiredSnapshotOnce' -count=1` 补验通过。
- `go vet ./...`、`go build ./...`、`git diff --check` 均通过；CLI 全量测试包含临时工程生成、升级与构建。跳过的测试不计作通过，真实数据库验证仍属下述未验证项。

## 未验证与升级代价

- 本轮不连接业务数据库，不演练生产升级，不承诺旧库/旧检查点可直接使用；这是明确的版本边界。
- 真实 MySQL schema拒绝/DDL 与极端断电、慢盘、生产负载未在本轮重新演练；合成/假库/浏览器测试各自只证明对应边界。
- 旧调用方必须改用当前 API、当前目录与独立 v4 数据库；不修改兄弟业务模块，不提供兼容包装。
- docs/issues 与 docs/superpowers 的历史记录保留当时事实；当前目录说明书和手册以本轮契约为准。

## 二次逐模块复核（2026-11-13）

审查依据为工作区实际差异、当前调用方与写入/读取路径，不以名称中有“兼容”或版本号就认定应该删除。以下结论不包含对真实业务数据库、生产环境或兄弟业务模块的验收。

| 模块 | 核对结果及处理 |
| --- | --- |
| 根包 / core / config / models | 当前公开门面仍有实际装配与调用用途；身份创建、恢复和去重统一使用当前身份契约。修正 FETCHER_WRITING_GUIDE 残留的 idempotency_hash/NULL/1000 字符旧索引说明，改为当前复合唯一约束。Trace 三种模式及未知键拒绝保持不变 |
| admin | 发现 Store.LoadStrings 无调用方的无 context 转发仍在，删除，保留实际使用的 Load/LoadContext 和 LoadStringsContext。清除导航控制器旧 Oao 注释及可选 destroy 判断，统一采用已声明依赖的当前销毁接口；会话代次、页面代次、迟到响应保护保留。页面 renderer 的三种返回形式与单文件日志下载是有效能力，不删除 |
| engine | 版本化重投、批量补偿仍通过无 context 的 loadTask 读取；冲突回查同样遗漏取消。这是原有缺口在入口收口后仍未解决，不能把清理称作已打通完整取消链路。统一内部读取签名传入 context，版本化请求及批量补偿传递调用方 context；无 context 的当前操作保持原语义。CAS 成功后的投递/溢出收尾不新增取消中断，不丢弃已持久化任务；锁序、认领和终态 SQL 未改 |
| internal | 核对旧表只读拒绝、迁移入口、当前熔断快照保存和恢复；未恢复旧数据转换。精确失败窗口、未结算失败及追加历史仍由原组件负责。未发现本次删除近似旧计数造成当前写入格式不能恢复的证据 |
| pkg | 核对账号管理状态生产者确实设置 Generation/LastControlVersion，当前关闭/自动恢复与下载检查点写入格式保留。filedown 当前写 v2；m3u8 当前仍会写 v1/v2，不能机械删除 v1。账号全局 ReverifyInterval 默认选择、无期限 Close 与非池认证重试属于当前可用能力，文档改为明确的当前语义 |
| obs | 当前事件写入版本与读取版本校验一致；坏行/不支持版本统计、预算及流式读取保持。run_id/window-* 也是当前显式报告目录规则，修正文档称其为升级兼容的描述 |
| cmd | 核对旧模块拒绝、新项目生成、当前目录和模板；修正 upgrade/help 中残留的旧业务目录提示。当前 main.go 位于根目录，业务包位于 internal，框架手册位于 docs/framework。全量测试包含合成工程构建与升级检查 |

其他文档修正：ERROR_QUEUE 删除已经不存在的 RetryTask 入口，并补齐 RetryTaskVersioned 的取消契约；同步 ENGINE_STRUCTURE、ADMIN_STRUCTURE 及当前手册。保留真正的版本拒绝检查和第三方协议/平台版本支持，不把它们作为旧 Papa 兼容代码删除。

### 二次验证

- 新增 `TestVersionedRetryReadsHonorCancellation`：覆盖读取前取消、查询开始时取消、冲突回查取消，断言返回 context.Canceled、无状态写入、无任务入队。
- `GOWORK=off go test -p 1 -json ./...`：77 个包、2860 项测试通过，89 项跳过，零失败；日志 `.review-tmp/v4-cleanup-review-test.jsonl`。计数包含子测试，不将跳过项算作通过。
- `GOWORK=off CGO_ENABLED=1 go test -race ./engine ./admin/configstore -run 'TestVersionedRetryReadsHonorCancellation|TestRetryTask|TestLoadTask|TestCompensation|TestLoadStrings|TestCached' -count=1` 通过。本轮为改动路径定向 race；上轮全量 race 结果见前文，不混称为本轮重新跑过全量 race。
- 最后调整导航控制器后，`go test ./admin/server ./admin/configstore` 通过；`go vet ./...`、`go build ./...`、`git diff --check` 通过。
- 未执行业务数据库操作或 Git 写操作。暂存区已有 mermaid-diagram.png 保持不变。

本次二次复核结束时的验证边界：真实 MySQL 的结构拒绝及 DDL 未复验；启动身份数据校验包含全表条件计数，大库启动耗时未测。下节已补齐独立测试库验证，并调整非法数据的检查方式。生产负载、慢盘、断电及真实站点仍未验证；不能据此承诺所有外部业务调用方无需调整。

## 真实 MySQL 验收与启动校验测量（2026-11-13）

环境：MySQL 8.1.0，单实例、本机合成数据；连接配置只用于获得服务器连接参数，连接前清空原 DBName。测试通过 `pkg/testdb.Create` 独占创建随机 `papa_v4_test_<uuid>`，不使用 IF NOT EXISTS、不接管已有库；仅本次成功创建的库获得清理权限。每个测试关闭连接后删库，再只读查询 information_schema 确认不存在。未读取、迁移或修改业务库表，也未修改业务连接配置。日志不含 DSN/凭据。

### 验收范围与实际修正

- `TestV4MySQLFreshRepeatMigrationAndTaskRound`：空库通过只读校验；开启 task_flow/operation_log 后完整建表并重复迁移；验证所有当前框架模型表存在。通过实际 MySQL 1062 验证身份唯一约束，执行认领、重复认领拒绝、终态结算及重复确认、版本化重投和陈旧版本拒绝；带数据再次迁移后整行保持一致，取消 context 的校验返回取消错误。
- `TestV4MySQLRejectsOldTablesWithoutWrites`：12 个独立子场景覆盖缺身份列、三个旧列、两个旧索引、NULL/非法身份类型、NULL/空哈希和 NULL/空身份值。启动检查和显式迁移均拒绝；拒绝前后 SHOW CREATE TABLE、行值及表/行数量均一致，没有创建其他框架表。
- Engine 的 `TestTaskIdentityMySQLDistinctIDsSameURLAndRestart`、`TestCompensationMySQLPersistenceRecoveryCASAndIdentity` 在真实独立 MySQL 库及 race 下通过，覆盖当前任务提交、同 URL 不同资源 ID、重启去重、持久补偿限制及版本竞争。
- 发现校验只需判断是否存在非法行，原 COUNT 却统计所有非法行。`checkTaskIdentityData` 改为对 `SELECT 1 ... LIMIT 1` 子查询计数，找到首个非法行即拒绝；不再声称返回坏行总数。原有 NULL/空值/类型判据、只读性质和 context 传递保持，正常库仍扫描全表。假库 SQL/参数断言与真实 MySQL 拒绝测试共同验证生产查询。
- 首次规模夹具错误假定 INSERT SELECT 分配的自增 ID 连续，触发 `Error 1062 (23000): Duplicate entry ... for key 'crawler_tasks.idx_task_identity'`；这是测试造数问题，未修改生产唯一约束。夹具改用独立的连续资源身份生成并限制每批 1 万行；检查总行数，首尾哈希与 core.NormalizeTaskIdentity 一致。失败测试库同样已清理。

### 耗时实测

计时为 `CheckTaskSchema` 完整调用（列/旧索引查询及身份数据检查），不包含造数或其他 App 组件启动。连续三次测量，未重启 MySQL 或清空缓存，不能称为严格冷缓存测试。

| 合成任务行数 | 修正后第 1 / 2 / 3 次 |
| --- | --- |
| 10,000 | 15.94 / 17.99 / 16.20 ms |
| 100,000 | 74.29 / 73.27 / 72.91 ms |
| 1,000,000 | 1.095 / 1.194 / 1.213 s |

百万行、单条非法记录比较：

| 非法行位置 | 当前完整校验 | 原全量 COUNT 数据查询 |
| --- | --- | --- |
| 最小 ID | 31.23 ms | 1.331 s |
| 最大 ID | 1.391 s | 1.329 s |

比较列计时边界不同：当前包含 schema 元数据查询，原 COUNT 列仅包含数据查询；这足以说明提前拒绝收益，不作为精确倍率基准。初始未修正版本的正常百万行完整校验为 3.790 / 1.475 / 1.385 s；两轮缓存/造数条件不同，不以此宣称正常库的性能提升。最终从 10 万增长至百万行造数约 116 秒，与启动校验耗时分开记录。

结论：正常库及末尾才发现非法行的最坏成本仍为 O(N)，本机百万行约 1–1.4 秒；首个非法行可以提前拒绝。没有增加跨进程通过缓存，也没有跳过数据检查。更大数据量、不同磁盘/缓存、宽记录及生产并发负载不能从这些结果直接推算。

### 可复跑接线与验证记录

测试文件：`internal/database/v4_mysql_test.go`。显式设置 `PAPA_V4_TEST_DSN` 提供允许建删独立测试库的连接；测试会丢弃 DSN 中的原库名。百万行测试还需 `PAPA_V4_SCALE_TEST=1`，未设置时跳过；测试连接有 10 分钟工作超时，删库使用独立清理 context。

- `GOWORK=off go test -race ./internal/database -run 'TestV4MySQLFresh|TestV4MySQLRejects' -v -count=1` 通过，记录 `.review-tmp/v4-mysql/acceptance-final.log`。
- Engine 两项真实数据库测试的 race 记录：`.review-tmp/v4-mysql/engine.log`。
- 百万行普通测试通过：`.review-tmp/v4-mysql/scale-final.log`；初始基线：`.review-tmp/v4-mysql/scale.log`。两轮均已记录对应测试库清理成功。
- `GOWORK=off go test -p 1 -json ./...` 通过：77 个包、2861 项测试通过，103 项跳过，零失败；日志 `.review-tmp/v4-mysql/full-test.jsonl`。这轮全量命令不注入数据库环境变量，因此新真库子测试也按设计跳过；它们的实际执行结果以上述独立真库命令为准，不把跳过当作通过。
- `go vet ./...`、`go build ./...`、`git diff --check` 通过。未执行 Git 写操作，工作区改动保留。
- 验证范围说明：收尾发现 `pkg/browser/pool.go` 与 `pkg/browser/temp_dir_test.go` 出现本轮未编辑的并行改动；原样保留。上述全量测试/vet/build 在这些新增改动出现前启动，不能据此为它们提供验证结论。

剩余未验证项：其他 MySQL 版本、严格冷缓存/重启场景、千万级或生产大小记录、线上竞争负载、慢盘/断电恢复；不提供旧业务库原地升级或兼容。验证结果只适用于上述合成夹具，不扩大为生产验收结论。

## 浏览器临时目录并行改动复核（2026-11-13）

本节接续上一轮未覆盖的 `pkg/browser/pool.go` / `temp_dir_test.go` 改动，不改动其他已有工作。原改动在显式配置 TempDir 时，于 Pool.Close 尾部执行 `os.RemoveAll(p.cfg.TempDir)`，存在两类已复现的阻断问题：

1. **共享目录误删**：PrepareTempDir 只创建/验证可写性，不授予独占所有权；两个池、诊断实例甚至其他文件可以共用根目录。`TestPoolClosePreservesSharedTempDirRoot` 在原实现失败：`closing one pool removed shared data ... The system cannot find the path specified.`
2. **在途实例被删目录**：Pool.Close 停止准入、唤醒等待者并回收空闲实例，不等待所有借出/创建中的实例。创建中的实例会在晚到返回路径自行关闭，借出实例由归还/关闭路径回收。原实现先删整个根，破坏这些实例的资源；`TestPoolClosePreservesCreatingProfile` 失败：`Close removed in-flight creation directory ... The system cannot find the path specified.`

修复：删除 removeRootOnClose 及根目录递归删除。配置根始终作为共享父目录保留，无论显式设置还是 Rod 默认值；每个实例独占的随机 profile 仍由原实例关闭/启动失败路径清理。没有新增等待锁、改变锁序、放宽准入或改变超时策略。Pool.Close 注释明确它不替代 App/Engine 的使用者排空。CORE_CONFIG 与 PKG_STRUCTURE 同步此所有权契约。

### 验证

- 两项不依赖浏览器的回归先在原实现复现失败，再在修复后通过：共享根和外部文件保留、其他池仍能创建 profile、创建途中 Close 不删目录、晚到 Get 不发布已关闭池的实例。
- `TestLivePoolTempDirCleanup` 验证正常归还后的 profile 确实删除，而配置根保留。
- `TestLivePoolClosePreservesBorrowedAndSharedProfiles` 使用两个真实 Chromium 池共用一个根：关池后借出实例仍存活且被 Stats 计为 Borrowed；另一池仍能创建浏览器；晚归还实例清理自己的 profile，不影响另一实例；最终空闲实例清理后共享根仍在。
- `CRAWLER_ROD_DIAGNOSTIC_LIVE=1 CGO_ENABLED=1 GOWORK=off go test -race ./pkg/browser -run 'Test.*TempDir|TestPoolClosePreservesCreatingProfile|TestLivePoolClosePreservesBorrowedAndSharedProfiles|TestClose' -v -count=1` 通过。记录 `.review-tmp/browser-temp-review-targeted.log`；真实用例没有跳过。
- `CRAWLER_ROD_DIAGNOSTIC_LIVE=1 CGO_ENABLED=1 GOWORK=off go test -race -json ./pkg/browser -count=1` 通过：149 项测试（含子测试）通过、9 项跳过、零失败，耗时约 132 秒；本节两项 live 用例实际通过。日志 `.review-tmp/browser-temp-review-race.jsonl`。
- `go test ./engine ./internal/app ./internal/browserdiagnostics -run 'Stop|Shutdown|Drain|Close' -count=1` 通过，记录 `.review-tmp/browser-temp-review-lifecycle.log`。`go vet ./...`、`go build ./...`、`git diff --check` 通过。本轮未重新运行全仓所有测试；验证聚焦受影响的浏览器包及上层关停路径。

规范核对范围：按 hdxy-code-review 检查 ENG-04 的 diff 密钥风险及 ENG-11 相关异步/生命周期风险，结合后端评审清单的并发、资源归属与重试视角。此次未引入密钥、认证、业务数据访问或部署脚本；共享目录删除属于上述明确的代码安全缺陷，不虚构为规范中不存在的条款。未执行全仓 gitleaks 扫描或生产发布，不作相应验收承诺。

剩余边界：测试在 Windows 本机 Chromium 上执行；未补做 Linux/macOS、强杀/断电及权限故障演练。异常进程收尾超过既定超时时，原有异步 cleanup 及其遗留风险未被改写。根目录及不属于实例 profile 的文件不会自动擦除，不能把“显式设置路径”理解为允许框架删除整棵目录。

## 整体收尾复核（2026-11-13）

本次重新核对当前源码和未提交差异，并结合此前真实 MySQL 与 Chromium 结果；没有把历史日志当成当前代码检查的替代。确认过的清理/修复事项已闭环，未发现新的可确认生产代码缺陷，未为凑审查项改动稳定行为。

| 检查面 | 当前结论 |
| --- | --- |
| 旧兼容与废弃 | 生产源码未找到已删除 TraceNextExecution/ListTrace/RetryTask 旧入口、旧身份字段输入、旧 Trace 配置映射和旧熔断转换。保留的根包门面、当前协议版本、操作系统/第三方库版本及策略历史有效性校验不是旧 Papa 兼容线路 |
| 当前能力与职责 | 核对七层职责及声明→提交→认领→执行→终态→补偿/恢复→停止主链；HTTP/浏览器/账号/下载、后台与报告仍按当前接口接线。此前 D1–D9/R1–R7 的历史闭环不扩大为每个外部环境已验收 |
| MySQL 与数据边界 | 当前 CheckTaskSchema 与验收代码一致；只读拒绝旧结构/非法身份，不迁移业务旧数据。首个非法行提前拒绝，正常库仍全表扫描。真实测试只创建自己的随机库，已有清理确认；本次没有重复造库或访问业务数据 |
| 关停与浏览器 | 核对 App 多组件排空门控、Engine 共用 Stop 截止时间及未确认执行、runtime 先关闭准入再等待、报告/审计收尾、Pool 借出/晚到路径。共享 DB/浏览器未排空不提前关闭；配置临时根不删除，只有实例 profile 由实例清理 |
| 文档 | 初诊 README 的功能矩阵和“最终画像”仍混用历史 Trace SQL/旧迁移口径，本轮修正并指向新的 v4 当前画像；历史 capabilities/closure 加当前状态入口。ARCHITECTURE 明确当前扩展契约与浏览器目录所有权；16 份相关 Markdown 本地链接检查无缺失 |

### 本轮验证

- `GOWORK=off go test -p 1 -json ./...` 通过：77 个包、2863 项测试（含子测试）通过、104 项跳过、零失败；`TestPackageResponsibilityBoundaries` 通过。日志 `.review-tmp/v4-final-review-test.jsonl`。本命令不注入真库/live 门控，跳过项不计为通过，单独实测证据见下条及前两节。
- `GOWORK=off CGO_ENABLED=1 go test -race ./engine ./internal/app ./internal/workerpool ./internal/breakerstore ./internal/tracestore ./admin/configstore -run 'Stop|Shutdown|Drain|Cancel|Retry|Snapshot|Cleanup' -count=1` 六包通过，记录 `.review-tmp/v4-final-review-race.log`。
- `go vet ./...`、`go build ./...` 通过；目录/报告修改另以本地链接检查和 `git diff --check` 核验。
- MySQL 的 12 类拒绝、新库/重复迁移/当前任务 CAS、百万行测量及浏览器全包/live race 使用前两节已完成的实测证据；相关生产路径本轮没有再改动。不将它们写成此轮重新运行，不叠加不同批次计数。

### 当前闭环范围与保留风险

当前可以确认的是已识别问题完成所属模块修复、相应回归和文档同步；不能承诺“全仓不存在未知缺陷”。最终画像统一列出：合法大库启动 O(N) 扫描；没有全进程硬资源沙箱或统一硬退出时限；不合作回调/I/O 会影响排空；Trace/事件/报告不是可靠业务事务；共享临时根和异常退出文件不自动擦除；其他 MySQL 版本、跨平台、严格冷缓存、千万级/长稳/慢盘/断电、真实业务与部署仍未验证。旧业务库原地升级不属于 v4 支持范围，多实例也不属于待补能力。

已保留 MySQL、浏览器及其他现有工作区改动，不执行 Git 索引或历史写操作，不触碰业务数据。

## 单实例持续负载与真实进程崩溃恢复（2026-11-13）

### 范围与安全边界

新增 `engine/resilience_mysql_test.go`，验证真实 Engine 的提交、驻留预算、持续数据库补充、延迟、认领、结算、启动恢复和 Stop。使用本机 MySQL 8.1.0 的独立随机库与 `example.invalid` 合成任务；业务结果、执行次数和控制表仅为测试夹具，不加入框架模型目录。未改变公开 API、生产 schema、锁序或关停契约，未访问业务数据。

父进程通过 `pkg/testdb.Create` 独占创建随机库（不采用已有库），丢弃连接配置中的原数据库名；子进程核验随机库名和父进程写入的所有权 token 后才执行。强杀目标只取本次 `exec.Command` 返回的 Process，确认退出后才启动恢复进程。正常/断言失败结束按次序回收子进程、关闭连接、删除自有库，DROP 失败会使测试失败。没有按前缀扫库删除，也没有停止 MySQL 服务。

### 真实强杀与关停超时

启用 RecoverQueue；每组 64 条任务，驻留上限 16、2 个 worker、队列容量 8，其中 16 条带未来五秒的 NotBefore。先显式暂停播种并核验全部 pending，再由独立子进程执行：

| 切点 | 核对内容 |
| --- | --- |
| Handler 已认领、业务结果尚未写入 | 强杀时仍有 processing 与未来 pending；新进程重新认领，最终 64 条 success、64 份对应业务结果 |
| 合成幂等结果已提交、Handler 尚未返回 | 强杀后该 Handler 被重放；按 TaskID 幂等写结果，任务数与结果数均不增加，证明必须允许业务重放 |
| Handler 不响应取消，Stop(50ms) 返回后 | 必须 drained=false，旧进程仍有 processing；没有因为取消信号或超时就谎报排空，再强杀并恢复全部任务 |

最终核验任务总数、所有终态、业务值逐项对应、执行时间顺序、延迟任务未提前执行、结果唯一性、驻留归零与 Stop 排空。结果检查采用每页 256 行，不在长时测试尾部全量加载任务。执行次数记录覆盖包括“认领后尚未写结果”的 Handler 入口；重放不被误报为 exactly-once。

### 持续负载

初始明确制造 64 条 pending，随后持续提交每批 64 条、批间至少两秒的合成任务，四分之一延迟 250ms，Handler 模拟 5ms 工作。每轮采样 durable pending/processing、驻留数、Go 堆分配与 goroutine；结束后核对所有任务、业务结果和执行次数，再验证排空。

| 实测 | 结果与证据 |
| --- | --- |
| 10 分钟持续负载，普通运行 | 10m0.128s，12,992 条任务、12,992 次结果写入路径、12,992 份唯一结果；驻留峰值 16，最终全部成功并排空。`.review-tmp/resilience-soak.log` |
| 10 分钟持续负载，race | 10m1.167s，12,224 条任务、12,224 次执行、12,224 份唯一结果；pending/processing 峰值 64，驻留峰值 16，采样 HeapAlloc 峰值 4,302,488 bytes（约 4.10 MiB）、goroutine 峰值 19；全部成功并排空，无 race 告警。`.review-tmp/resilience-final-race.log` 中 `TestSingleInstanceMySQLSoak` 通过 |
| 最终强杀夹具，race 三轮 | `go test -race ./engine -run '^TestSingleInstanceMySQLCrashRecovery$' -count=3 -timeout 4m` 通过，9 组独立库；每组强杀时 2 processing、16 future pending，66 次 Handler 入口、64 success、64 份唯一结果。Stop 三轮分别 50/50/51ms 且 drained=false。`.review-tmp/resilience-crash-final.log` |
| 分批结果核验和延迟时间断言，联合 race | 强杀三切点及 30 秒负载通过；短负载实际 31.248s、704 条任务/结果、驻留峰值 16。`.review-tmp/resilience-confirm.log`；其后补齐未写结果就中断的入口次数记录，已由上一行最终三轮覆盖 |

证据口径：十分钟 race 日志的**整体命令退出码是 1**，原因是同一次命令前置强杀子项使用了下文第 2 条错误时间谓词，随后持续负载子项独立通过；不将整体命令写成通过。修正后的强杀已单独三轮通过，联合短时 race 也通过。长时运行期间未修改生产实现，后续夹具只补强分批核验、延迟时间断言及入口计数。两轮负载在共享本机 MySQL 上运行，不作严格隔离的吞吐基准比较。

任务历史、尝试记录和业务结果随已提交数量增长属于测试预期；待执行积压能消化、驻留不超额，不能推导“数据库历史或整个进程磁盘有硬上限”。Go HeapAlloc 采样也不是 RSS 或泄漏证明。

### 发现与修正

本轮未发现需要修改生产实现的可确认缺陷；修正的是新增验收夹具的两个错误假设：

1. 不能假设 ApplyRegisterStage 前不会执行任务：Submit 会懒建工作池。早期播种夹具使用空 Handler，导致 `panic: runtime error: invalid memory address or nil pointer dereference` 并使恢复等待超时。改为显式暂停播种，且在子进程启动前断言 64 条均为 pending。
2. UTC DATETIME 的检查不能直接绑定 `time.Now().UTC()`，驱动 Loc 仍可能转换时区。早期夹具错误得到 `cut must retain durable future tasks: 0 <nil>`；改用与生产认领一致的 `UTC_TIMESTAMP(3)` 比较，实测强杀时保留 16 条未来任务。

校准失败不作为生产缺陷或通过证据。未通过放宽任务总数、删除失败行、关闭恢复检查来“修绿”。

### 剩余风险及未验证项

- 本轮是 Windows、MySQL 8.1.0、单合成站点/阶段、Trace off、无账号/浏览器/下载依赖的 Engine 集成验证。不能替代完整 App/HTTP 生命周期、真实站点、多阶段混合业务和外部 Provider 的生产验收。
- 十分钟持续运行不能替代 24/72 小时长稳、百万级持续积压、磁盘耗尽、长时间慢 SQL 或内存压力测试。工作集预算不是整进程资源硬隔离。
- 强杀的是应用测试子进程，不是 MySQL 服务崩溃、断电、文件系统损坏或丢失已确认事务；这些仍未验证。测试父进程被强杀时清理回调也无法执行。
- 启动恢复是可重放语义；结果唯一来自合成业务自己的 TaskID 唯一键与幂等写入，框架不保证外部非幂等副作用 exactly-once。
- 50ms 验证只针对 Engine.Stop 的预算和真实 drained 返回值，不改变 App 无统一硬退出总时限、不合作回调可能继续存活及未排空时保留共享依赖的既有边界。

### 最终回归与文档验证

- `GOWORK=off go test -p 1 -count=1 -json ./...` 禁用缓存全仓通过：77 个有测试包通过、5 个无测试包；2864 项测试（含子测试）通过、109 项跳过、零失败。日志 `.review-tmp/resilience-all-final.jsonl`。常规全仓命令未注入真实 MySQL 门控，不能用其中跳过项替代上述独立实库结果。
- 最终强杀三轮 race、联合短负载 race 与十分钟持续负载子项的结果及校准失败区别见上表，不累计不同批次的测试数。
- `go vet ./...`、`go build ./...`、`git diff --check` 通过；本次五份修改文档的本地链接路径检查无缺失。

运行方式已同步 `docs/architecture/DEVELOPMENT.md`，职责已归入 `docs/package_structure/ENGINE_STRUCTURE.md`，最终画像与诊断入口同步当前证据和剩余边界。保留现有改动，不执行 Git 写操作。

## 长稳与崩溃验证二次审查（2026-11-13）

结论：上一轮确实执行了真实子进程强杀、重启和十分钟负载，并非用同进程对象重建代替崩溃。但若干验收断言不足以阻止后续夹具退化或错误证据通过，已在 `engine/resilience_mysql_test.go` 内补强；本轮未修改生产实现、公开 API、锁序或生产数据结构。

| 审查发现 | 修正与验证方式 |
| --- | --- |
| Stop 的默认 false/0 也可能被当成超时证据；未检查未完成统计 | 增加 StopCalled、Unfinished，要求确实调用、未完成数大于零、drained=false，耗时须为 40–2000ms。该宽容上界是本机测试容差，不是生产退出保证 |
| Ready 没有绑定任务，未直接检查结果是否已提交 | 控制记录保存 CutTaskID；父进程核对该任务确为 processing，after-effect 已存在结果，其余切点无结果；避免三个切点意外退化成相同场景 |
| 只看 65–66 次总量，不能定位谁被重放 | 旧子进程被 Kill 且 Wait 完成后，冻结已持久化尝试集合；恢复后逐任务核对 1+中断前次数，同时检查总数、无孤儿和中断任务存在 |
| 任务 URL 与结果彼此一致，不等于属于原始提交集合 | 检查值必须属于本轮 0..N-1 集合、规范整数编码及 URL 唯一，结合 N 条总数覆盖完整输入集合；保留业务值、终态、执行时间和 NotBefore 断言 |
| 开启启动恢复的负载不能单独证明持续回灌；结束前集中完成也可能掩盖中途停滞 | 持续负载关闭 RecoverQueue，保留初始 64 条积压；有积压且完成量连续十秒不增加即失败。这是轮询观测门槛，不是独立硬实时 watchdog，也没有声称数据库调用永远不会阻塞 |
| 清理仅检查 DROP 返回值，没有独立缺库证据 | 在分配器之前注册最终 cleanup，LIFO 保证关闭连接、DROP 之后才查询 `information_schema.schemata` 中本次精确库名；保留原 DROP 错误检查，输出 `cleanup verified: ... absent`。不扫描或删除其他库 |

新增 `TestResilienceMySQLRejectsCorruptEvidence`：同一个独立库中先完成四条任务，再分别在事务内替换任务、错配尝试归属、增加孤儿尝试、移走结果、伪造未来截止时间、抹去成功终态，确认校验器拒绝后回滚，最后再次确认基线正确。这六个负向用例包含新缺口和原有正确断言的防退化检查，不表示六种错误都曾在生产出现。

新增 `TestResilienceMySQLFailureCleanup`：子进程自建独立库后故意 `t.Fatal`，父进程必须同时观察到预期非零退出、指定断言消息、精确库名的缺库验证且无 race 告警。这证明普通断言失败的清理路径；不声称父进程被外部强杀、MySQL 不可用或权限撤销时也能完成清理。

### 本轮证据

- 补强后的十分钟联合 race **整条命令通过、退出码 0**：`go test -race ./engine -run '^TestSingleInstanceMySQL(Soak|CrashRecovery)$' -count=1 -v -timeout 15m`，环境 `PAPA_RESILIENCE_SOAK_DURATION=10m`。关闭 RecoverQueue 的持续负载实际 10m2.617s，12,672 条任务、12,672 次尝试、12,672 份结果全部核对通过并排空；持久积压采样峰值 64、驻留峰值 16、HeapAlloc 峰值 4,319,696 bytes、goroutine 峰值 19，未触发中途无进展断言，无 race 告警。三个强杀子项和四个自有库的缺库核验同样通过，日志 `.review-tmp/resilience-review-race.log`。运行期间只继续增加验收反例、独立校验器的总数/孤儿检查及失败清理用例，负载/调度路径未再修改；最终校验器另由下列联合短跑覆盖。
- 补强后的三个强杀切点完成三轮 race，日志 `.review-tmp/resilience-review-negative.log`；每组 64 条任务最终全部成功，强杀时 2 processing、16 future pending，记录 66 次尝试、64 份结果，并分别确认库已不存在。该批同时运行了五个负向用例；后补的第六个用例以最终联合测试为准。
- 最终联合 race：六个负向用例、故意断言失败后的清理、三个强杀切点及 30 秒负载全部通过，日志 `.review-tmp/resilience-review-final.log`。短负载 30.451s、704 条任务/结果、驻留峰值 16；所有自有库均有缺库检查通过证据。强杀 Stop 仍实测约 50ms 且未排空。
- 普通全仓 `GOWORK=off go test -p 1 -json ./...` 通过：77 个有测试包、5 个无测试包，2864 项通过、111 项跳过、零失败；日志 `.review-tmp/resilience-review-all-final.jsonl`。本轮允许 Go 缓存，真实 MySQL 的跳过项由前述独立测试补足，不混算为全仓命令通过了实库验收。
- 故意断言失败的清理场景另作最后一次 race 复跑，日志 `.review-tmp/resilience-review-cleanup-final.log` 记录具体自有库名及 `absent`，通过。`go vet ./...`、`go build ./...`、gofmt、`git diff --check` 及四份本轮修改文档的本地链接路径检查通过。

### 报告口径与保留边界

上轮记录的十分钟普通运行与 race 子项数字可在原始日志中复核；同一命令前置夹具校准失败导致整体退出码为 1，也已如实记录，不能改写为整条命令通过。本轮重跑使用补强后的切点、持续回灌和清理断言；最新结果单列，历史数据不覆盖、不相加。

当前证据仍仅针对 Windows / 本机共享 MySQL 8.1.0 / 单合成站点阶段 / Trace off 的 Engine 链路。峰值是采样值，不是全时刻 RSS、内存泄漏或磁盘上界证明；负载会阶段性消化完，不是保持十分钟持续饱和。24/72 小时生产长稳、MySQL 服务崩溃、断电、磁盘故障、外部非幂等副作用以及完整 App 和业务依赖仍不在本轮覆盖范围。

所有数据库读写仅限独占创建的测试库与精确库名元数据核验。保留工作区现有修改，不提交。

## 完整 App 组合根与多站点混合负载（2026-11-14）

新增 `internal/app/resilience_mysql_test.go`，通过真实 `NewApp → RegisterSites → Run → Shutdown/外部取消 → shutdownRun` 执行；不再用只构造 Engine 或直接调用 shutdownRun 的方式替代 App 集成验收。本轮未确认新的生产代码缺陷，未修改生产实现/API/锁序。

### 负载和组件范围

- alpha/beta 两站点分别注册同名 catalog/detail 阶段，初始 64 条任务含延迟任务；每个 catalog 再向本站 detail 投递 child，基础完整集合为 96 条。相同 URL 在不同站点/阶段保持隔离，结果按 TaskID 幂等写入。
- 全局/站点/阶段驻留配置为 8/4/2，每阶段 1 worker、队列容量 4。正常场景先暂停 alpha，必须观察 beta 的 48 条任务完成、alpha 的 32 条原始任务仍 pending，再恢复 alpha 并持续批量提交。采样检查全局驻留不超过 8；不是逐纳秒内存监控或严格饱和吞吐基准。
- 实际启用 HTTP 服务、运行策略初始化/采样、消息监听、空浏览器池、审计、事件流、每两秒报告及退出报告。没有真实账号 Provider、真实抓取、下载流量或 Chromium 实例；空池只验证所有权和关闭门控。
- 父进程独占 `papa_app_test_<随机值>`，丢弃原 DSN 数据库名；子进程验证精确命名格式和所有权 token。配置、日志、报告、浏览器临时根均归父进程 TempDir；强杀目标仅为本轮创建的子进程，Wait 完成后才启动恢复 App。每个库 DROP 后按精确名称确认不存在，不按前缀删除库。

### 四条生命周期路径

| 场景 | 断言 |
| --- | --- |
| 正常混合负载与 Shutdown | 所有任务完成后保留一个真实在途 HTTP 请求；调用 App.Shutdown 时 Run 不能提前返回，HTTP handler 和收尾 hook 仍可使用 DB；释放请求后关闭监听器、运行采样/消息循环，关闭 DB/浏览器池 |
| 合作取消 | 两站 catalog handler 已派生 child、写入幂等结果，再等待 context 取消；确认两者收到取消并退回 pending。App 因仍有持久积压返回 partial，DB/池保留；退出旧进程后从新 App 恢复 |
| 关停超时后强杀 | 两站 catalog 故意不响应取消，Engine stop_timeout 为 100ms；App 正常完成其余收尾且返回 partial，依赖仍可用；父进程核对两站各一条 processing catalog 及其已提交结果后强杀、Wait、启动新 App |
| 运行中强杀 | App 未走正常退出；同样核对两站 processing catalog 的已提交结果后强杀，确认旧进程退出才重启，验证启动恢复与持续回灌 |

所有有序退出还必须满足：run_started/run_stopped 各一条且 outcome 正确；收尾 hook 入队的 16 条合成审计记录全部落库；四份日报产物存在且非空，JSON 可解析；监听器关闭、组件确实已装配且自有循环退出。部分退出不误关共享资源，完整排空退出不遗留 DB/浏览器池准入。

父进程最终逐项核对原始与派生 URL 清单、四个站点/阶段分组、success 终态、结果的 site/stage/URL、延迟时间和逐任务重放次数。重启前冻结已持久化尝试与任务状态：已成功任务不应重跑，未完成任务再执行一次；新增派生任务执行一次，禁止孤儿结果/尝试。结果唯一来自合成业务的幂等写入，不是框架对任意外部副作用的 exactly-once 保证。

### 校准发现

第一轮 race 曾因 `cooperative cancellation left processing tasks` 失败。原夹具把“明确取消的 catalog 必须释放认领”扩大为“全库不得有 processing”，错误覆盖了并行 detail 业务已执行但结算未确认的路径。未把这些行强行改 pending：现在分别检查指定取消任务释放、未确认任务留库以及重启后的逐任务次数。后续日志实际观察到 alpha/beta detail 保留 processing，而取消的 catalog 均已退回 pending；恢复后全量结果一致。

这是测试语义修正，不是生产回归修复；原失败日志 `.review-tmp/app-mix-race.log` 保留，不将其整体命令写成通过。

### 验证记录

- 一分钟混合负载联合 race：`PAPA_APP_MIX_DURATION=1m go test -race ./internal/app -run '^TestAppMySQLMixedLifecycle$' -v -count=1 -timeout 8m` 整体通过，日志 `.review-tmp/app-mix-final-race.log`。正常场景 3,840 条任务/尝试/结果一致，alpha/beta 各 640 catalog + 1280 detail；该子项含启动、负载、排空及报告，共 77.39s。取消、超时、强杀各自恢复 96 条任务通过。
- 最终切点核对及取消语义三轮 race：`.review-tmp/app-mix-final-cuts.log`，9 组独立库通过；每组 96 条最终 success/结果，记录 98–100 次尝试，逐任务符合重放规则。日志记录重启前 unconfirmed 的站点/阶段；包括合作取消后并行 detail 未确认的实证。
- 最终短负载联合 race：`.review-tmp/app-mix-confirm.log` 全部通过，正常场景 288 条任务/结果；进一步确认监听器实际关闭、运行循环非 nil 且已退出。最后这项仅补强结束断言，不改动一分钟负载或生产代码。
- 全仓 `GOWORK=off go test -p 1 -json ./...` 通过，77 个有测试包、5 个无测试包；2865 项通过、116 项跳过、零失败，日志 `.review-tmp/app-mix-all-final.jsonl`。允许 Go 缓存，常规命令的实库跳过项由上述独立 MySQL 测试覆盖，不合并统计不同批次。`go vet ./...`、`go build ./...`、gofmt、`git diff --check` 及本次五份修改文档的本地链接路径检查通过。

### 剩余边界

本轮补齐 App 组合根和混合任务链路，不等于所有可选业务插件都已验收。未覆盖真实账号登录/身份诊断在途、真实浏览器实例或下载器在途、认证/权限完整验收、24/72 小时 App 长稳、持续饱和、MySQL 服务崩溃、断电/磁盘故障。App 仍没有统一硬退出总时限，报告、审计或外部回调不合作可延长关停；不能用此处受控回调的通过结果消除该边界。

未触碰业务数据，未提交；保留之前 Engine 验证及所有工作区修改。


## App 混合负载验证复审（2026-11-14）

真实强杀、旧进程 Wait、新 App 恢复及逐任务重放断言均存在。本轮发现的问题在验收夹具，未确认需要修改生产实现的问题；未改公开 API、任务结算或锁序。

### 已修正的验证漏洞

- 日报目录仅含普通文件时，原循环可以零次执行而通过。现在必须检查至少一个实际日报目录及四份产物；事件核对非空且一致的 run_id 与开始/结束顺序。这仍不等于报告统计内容全量正确性验证。
- 原先任意 HTTP 请求错误都视为监听器关闭；现在另用 TCP 连接探测排除请求超时/HTTP 处理错误导致的假通过，探测超时明确失败。空浏览器池精确核对开放/关闭两种错误。
- 原先只看 LastStartedAt，重放可能掩盖首次提前执行。尝试表新增微秒精度入口时间，每次尝试（包括强杀前）均与持久 NotBefore 比较。
- 超时路径原先只由控制端 Ping DB。现在两个在途 catalog handler 在 Run 返回 partial 后实际再次写入各自结果行，随后继续阻塞，父进程核验 processing 后强杀；同时核对 Retained 标志。
- 子进程清理原先无界等待且丢弃成功日志。现在回收限时 10 秒、保留所有子进程输出、关闭测试 HTTP 空闲连接，HTTP hold 入场增加超时。TempDir 自动清理后检查目录不存在；数据库仍 DROP 后精确查不存在，不按前缀扫描删除。

### 新证据与范围

- `.review-tmp/app-mix-review-race.log`：一分钟负载及四场景联合 race 整体退出码 0，97.908s；正常场景 3,888 条任务/尝试/结果，alpha/beta 各 648 catalog、1296 detail；四库清理确认。包含入口时间、事件顺序、TCP 和报告目录补强，尚未加入后续晚到 handler 写库及 TempDir 缺目录核验。
- `.review-tmp/app-mix-review-cuts.log`：取消/超时/强杀各三轮 race 通过，54.628s；每库 96 条最终任务/结果、98–100 次尝试。三次超时日志均确认两个 handler 在 Run 返回后写库成功，九库清理确认。

成功日志包含人工制造取消/100ms 超时引起的结算取消、未确认执行及熔断收尾超时，不描述为“无错误日志”；对应未确认任务由重启后的逐任务状态/次数/结果验证恢复。未验证真实熔断故障状态与本负载的组合场景。

本套件证明有限积压回灌、暂停站点隔离、采样全局驻留上限、合作取消、HTTP 在途排空及超时资源保留。正常 Shutdown 前任务已排空，不能声称覆盖正常关停时活跃 worker 的完整成功收尾；超时晚到写库后 handler 仍未退出，也不证明自动二次关闭保留资源。未独立断言各站点/阶段驻留峰值、持续负载公平性、内存泄漏或持续饱和。父进程外部强杀或 MySQL 不可用时清理仍不保证；App 专属“故意断言失败后清理”未另作故障注入，不借用 Engine 记录充当 App 实测。


### 最终确认

- `.review-tmp/app-mix-review-confirm.log`：最终文件四场景短负载 race 通过，25.968s；正常 288 条任务/尝试/结果，三个恢复场景各 96 条任务/结果、99 次尝试；晚到 handler 写库通过，四个测试库及四个父进程临时目录全部确认不存在。
- `.review-tmp/app-mix-review-all.jsonl`：全仓 `GOWORK=off go test -p 1 -json ./...` 退出码 0，77 个有测试包通过、5 个无测试包；2865 项通过、116 项跳过、零失败。允许缓存，未注入实库变量；真实 MySQL 由上述专门命令验证。该全仓运行之后仅追加 TempDir 缺目录断言，由最终四场景 race 覆盖。
- 最终 gofmt、go vet ./...、go build ./...、git diff --check 通过；同步开发手册、内部职责说明及框架画像。未触碰业务数据，保留所有原有修改，未提交。


## 活跃排空、持续饱和及 App 失败清理（2026-11-14）

### 发现与修复

新增 `TestAppMySQLActiveDrain` 在两站点四阶段各一个 handler 已 processing 时触发 Shutdown。四者收到取消后继续等待，测试确认 Run 未返回且 DB 仍可用，再放行业务写入并返回 nil。修复前 `.review-tmp/app-active-before.log` 实际失败：`successful late handlers did not settle`、`cleanup retained DB`；四个任务仍未确认，App 保留依赖。测试库和目录仍由父进程清除。

根因不是 App 提前关闭依赖，而是正常业务已经成功返回后，结束时间及 success 终态仍受已取消 context 阻断。修复归属 Engine：新增 `engine/internal/execution/settlement.go` 的 ConfirmSuccess，正常运行沿用原持久化重试，仅取消触发一次有界确认；用于 `attempt_execution.go` 的成功计时及 `stage.go` 的成功终态。执行版本保留，SQL 条件不变；ErrChanged/ErrRecordNotFound 不重试，写失败保留未确认任务，不重跑 handler，不改取消失败/panic 的路径和锁序。两个写入各自最多 2 秒，不改变 Stop 的截止时间或资源保留原则。

### 新测试职责与证据

`internal/app/lifecycle_load_mysql_test.go` 只负责组合根验收，不把调度状态或持久化规则移入 App。独占数据库、token 校验、合成 URL、父进程临时目录与精确清理沿用现有夹具。

- 活跃 worker：四个真实在途任务收到取消后完成业务与成功结算，再关闭依赖；父进程逐任务检查 success、起止时间、业务字段、一次执行、完整 URL 清单，非总数判定。
- 持续饱和：关闭启动 RecoverQueue，四组各预置 96 条任务，低于 64 补至 96；pending 采样少于 8 失败。全局/站点/阶段预算 4/3/2，单 worker，四组处理耗时分别 20/40/60/80ms。独立 20ms 采样器不受生产者提交阻塞影响，驻留超 4 或任一组 5 秒无进展即失败；最终停止补充并排空全部任务。
- App 断言失败：内层 App 四个 worker 活跃时故意 Fatal，cleanup 取消并放行收尾；确认 DB/池/HTTP/循环与事件报告后才输出成功清理标记。中层回收进程、目录和自有库，外层同时要求故障标记、非零退出、所有清理标记与无 race 告警。输出中的内层 FAIL 是预期注入，最外层 PASS 才是验收结论。

`.review-tmp/app-load-minute-race.log` 联合 race 整条命令通过（106.695s）：活跃排空、持续饱和、失败清理及原有四场景混合/恢复全部通过。一分钟饱和实际 1m0.469s，最终 2,366 条任务/结果/尝试一致，四组分别 662/630/569/505 条；303 次积压采样，组内 pending 最低 58；3,023 次独立采样、驻留峰值 4、最长采样无进展间隔 480.2226ms。结束前各组已完成 565/540/499/435 条，余量随后完整排空；不把最终排空计入一分钟内吞吐。

`.review-tmp/app-load-cuts-repeat.log`：活跃排空与故意失败清理各三轮 race 通过（12.979s）。`.review-tmp/app-drain-mysql-regression.log`：真实 MySQL 计时、调度、熔断串行/结算链回归通过（24.510s）。原有 mixed 的取消任务仍释放认领，未完成任务仍可重启恢复；本次修复使成功 handler 有机会正常确认，不以强制 pending 清零掩盖未确认执行。

### 证据边界

本轮覆盖的是单实例 Windows / MySQL 8.1.0 / 合成工作负载。持续饱和为“每组始终留有明显超出预算的采样积压”，不是 CPU、网络或 MySQL 满负荷。公平性是四组的有界进展，不是吞吐严格均分或任意负载的硬实时 SLA。未直接采样各组完整驻留状态，没有额外公开测试 API；真实 Provider、浏览器实例、下载流、24/72 小时长稳、断电、数据库服务崩溃仍未覆盖。失败清理是普通断言路径，资源所有者进程外部强杀/服务器不可用仍可能残留。晚到持久化若超过 Stop 时限仍返回 partial 并保留依赖，不承诺自动二次关闭。


### 最终回归及清理检查

- `.review-tmp/app-load-all.jsonl`：普通全仓测试退出码 0；77 个有测试包通过、5 个无测试包，2868 项通过、120 项跳过，零失败。允许缓存，普通命令未注入实库变量；不把跳过当作真实 MySQL 通过。
- `.review-tmp/app-drain-engine-race.log`：Engine 全包及三个内部包 race 通过，主包 285.746s。另开的 `.review-tmp/app-drain-engine-race-diagnostic.log` 因 90 秒诊断时限失败，栈位于既有 65 阶段 `TestBreakerReviewTimelineTruncationMustNotInvalidateHealthyStages` 的报告快照/脱敏；保留超时日志，不将该命令写为通过。全量 race 是独立完整通过的命令，没有靠排除该用例取得绿灯。
- 检查发现既有 `compensationMySQLWithOptions` 夹具未设置 TraceFiles.Dir，选中的熔断实库回归在 engine/traces 留下 21 份合成 Trace。已按 Engine 测试资源归属设置 t.TempDir，并将本轮已确认归属的原始文件移到 `.review-tmp/app-drain-traces-before` 留证；未删除业务 Trace。此测试夹具修正发生在全仓和全量 race 之后，由同一组真实 MySQL 定向回归复验。

所有修改保留在工作区，没有 Git 提交；新库均为独占测试库，不操作业务库。

最终 `.review-tmp/app-drain-mysql-final.log` 同组真实 MySQL race 退出码 0（21.125s），测试后 engine/traces 不存在；最终 gofmt、vet/build、diff 空白检查及本轮文档链接路径检查通过。

## obs 辅助报告隔离（2026-11-14）

按用户确认的定位先修订设计，再实施有界异步入口、信号聚合、独立报告 writer 和 App 共享 2 秒收尾预算。在线报告不再接收业务 DB、实时查询或 Section/Pattern 回调，不扫描历史；显式离线分析仍保留。移除 App.UseReportSection/UseReportPattern，不留兼容空壳；可靠失败/熔断证据迁入 pkg/incident，存储 JSON 契约不变，任务结算与熔断恢复没有改走可丢队列。

完整职责、回归、证据口径和限制见 [obs 实施记录](2026-11-14-obs-sidecar-implementation.md)，当前能力见 [最终画像](2026-11-13-framework-diagnosis/v4-final-portrait.md)。本次覆盖的是进程内故障隔离，不是物理零开销保证；报告仍可能缺失，四份文件不是整组事务。共享 CPU/磁盘、永久阻塞 I/O、离线不合作回调等边界均明确保留。

`obs-app-mysql.log` 中真实 App race 验证关闭观测、归档阻塞、归档 panic、报告不可写四种持续积压场景以及活跃排空、取消、超时和强杀恢复，联合命令通过（107.536s）。`obs-app-final-load.log` 补正常在线饱和及 App 故意断言失败清理（21.815s）；正常样本 523 条任务/结果/尝试一致，5.148s 补充阶段采样 pending 最低 58、驻留峰值 4、最长组内无进展间隔约 469ms，之后继续排空。内层故意 FAIL 是清理注入，外层要求非零退出和所有清理标记，最终 PASS。所有库和目录按独占夹具核验不存在，未动业务数据。

Engine 可靠协议真实 MySQL race 21.294s 通过；普通全仓 77 个有测试包通过，Engine 全量 race 主包 290.168s 通过，另有事件/在线与离线报告/App 定向 race 和 vet/build。新快照路径还补回本轮队列处理数，验证业务锁忙时跳过并标记截断，不阻塞主链。最终收尾结果以实施记录末节为准，不累计不同批次计数或把普通 MySQL 跳过当作实库证据。


## obs 隔离复审补正

复审补齐事件发生时间及并发序号、跨午夜/乱序信号统计、维度截断质量与故障夹具断言和清理；详见 [复审记录](2026-11-14-obs-sidecar-review.md)。不修改可靠结算或熔断存储协议，原有验证记录保留各自批次范围。


## obs 解耦后的整体收尾

补齐熔断启动恢复排空、事件限长与脱敏顺序及后台/CLI 提示，七模块审查和验证范围见 [整体收尾记录](2026-11-14-framework-final-review.md)。保留已有修改，不执行 Git 写，不操作业务数据。


## 后续最终代码复审

并发恢复 gate、事件身份总预算及报告二次限长另有补正，最终无缓存全仓验证见 [复审记录](2026-11-14-final-code-rereview.md)。此前验证批次保持历史范围，不当作这些补正后的完整验收。
