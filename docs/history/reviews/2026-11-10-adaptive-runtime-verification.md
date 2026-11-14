# 站点账号池与运行治理验收记录

<!-- version-scope -->
> **适用版本/阶段：Papa v4 演进过程中的历史工作区快照（具体基线以正文为准；未记录精确发布版的条目不推定版本号）。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 当前使用方式以 [v4 手册导航](../../README.md) 为准。
<!-- /version-scope -->


日期：2026-11-10。对应 [已确认设计](../designs/2026-11-10-adaptive-runtime-design.md) 与 [使用手册](../../runtime/RUNTIME_CONTROL.md)。修改保留在工作区，未执行 Git 写操作，未启动真实站点采集。

## 交付范围

- 站点内命名账号池：固定当前账号、失效标记、独立替补调度、版本隔离、持久化与重启复验。失效任务直接失败，之后由原有补偿入口处理；任务去重不增加账号维度。
- 运行策略：Go 声明和 YAML 显式值作为基础，OA 保存版本化数据库覆盖；并发写冲突、写入回执不明确、重启读取异常均有明确处理。
- 站点请求：跨阶段与账号共享并发/速率，AutoThrottle、429/503 冷却与 Retry-After；HTTP、账号、文件及 M3U8 使用共享预算。受管浏览器页面支持范围内的导航、XHR、子资源和重定向，以及已有页面上的热启停。
- 单实例资源：CPU/内存采样、连续窗口与恢复阈值、任务/浏览器借用放行、站点服务权重，调节只作用于新工作。
- 代理健康：故障反馈、冷却、受限探测、版本保护；固定出口不自动换代理，要求代理的请求不退回直连。
- OA：“数据与资源 → 账号池”；“运行监控与报告 → 采集控制”的站点请求、资源控制、代理健康三个页签。支持草稿保留、显式冲突核对、公共浏览器池实际实例统计与窄屏键盘操作。

## 验证结果

Windows / PowerShell，使用 `GOWORK=off`。真实浏览器用本机 Chrome，所有目标为本地合成服务。

| 检查 | 结果与证据 |
| --- | --- |
| 全仓测试 | `go test ./...` 通过 |
| 全仓并发检查 | `CGO_ENABLED=1 go test -race ./...` 通过，无 race 报告；设置 `PAPA_EGRESS_BROWSER_BIN` 启用真实浏览器用例 |
| 静态检查与构建 | `go vet ./...`、`go build ./...` 通过 |
| 共享请求预算 | `TestRuntimeSharedRequestBudgetAcrossHTTPAccountFileAndM3U8`：六条并行路径在 `max_inflight=1` 下服务器峰值为 1，包含重定向、固定代理和重新包装的客户端 |
| 账号与任务解耦 | `TestRuntimeAccountFailureEndsTaskWithoutWaitingForReplacement`、`TestAccountPoolTaskRoundTripAndDedupIdentity`、账号池并发引用保护及迟到反馈回归通过 |
| 执行许可 | `TestRuntimeCooldownPendingDoesNotReserveGlobalTaskCapacity`、`TestRuntimeStageReleasesAdmissionOnEveryOutcome`、`TestRuntimePauseDuringAdmissionDoesNotClaimTask` 通过，覆盖成功、失败、panic、取消与暂停 |
| 浏览器网络与生命周期 | `TestGovernanceRealBrowserNavigationXHRSubresourceRedirectAndBlock`、`TestGovernanceRealBrowserExistingPageHotEnableDisable` 通过；`TestPoolStatsCloseDuringCreationDoesNotPublishOrLeakBrowser` 覆盖创建期间关闭池 |
| OA 操作 | `TestRuntimeUIRealBrowserDraftSaveConflictAndNavigation` 通过：保存、刷新不覆盖草稿、409 后只合并修改字段、保留无关并发修改、窄屏、Tab/Shift+Tab/Escape 和页面销毁 |
| 账号池管理 API | `TestRuntimeAccountPoolAPITombstoneAndReadOnly` 通过：GET 不认证/写入、旧版本拒绝、策略引用阻止删除、墓碑版本显式重建 |
| 写入回执与重启 | `TestRuntimeUncertainGETReconcilesDurableRevision`、`TestRuntimePolicyRestartUsesSavedOverrideAndRejectsCorruption` 通过 |
| 真实 MySQL | `TestRuntimePolicyMySQLConcurrentCAS` 通过：独立随机测试库迁移四个模型；首次插入和已有版本更新各 12 个并发写者均仅一个成功；账号池 CAS、读取及任务归属列检查通过；测试库清理成功 |
| 脚手架 | `go test ./cmd/papa ./docs ./config` 通过，包含真实生成项目、tidy、build；配置模板默认关闭新增自动控制 |
| 工作区检查 | Go 格式化及 `git diff --check` 通过 |

MySQL 只借用本地连接配置建立 `papa_runtime_test_<随机值>` 测试库，没有迁移或修改业务库。业务应用仍需在升级后单独执行迁移。

## 验证中修正的问题

- 旧框架表数断言固定为 6，新增两张账号池管理表后失败，已改为 8 并保留列级回归。
- 联合预算测试最初超时：本地安全代理只授权了 HTTP 目标，测试 CONNECT 所需的 HTTPS 同源授权缺失。修正合成 fixture 后通过，没有增加等待时限或放宽生产出口校验。
- 实际浏览器窄屏断言报 `narrow drawer overflow`：`100vw` 包含滚动条宽度，固定右侧抽屉越界。改为相对包含块的 `100%`，390 像素视口和键盘回归通过。
- 运行策略未覆盖任务间隔时保留 Go 随机区间；阶段显式空池名覆盖站点默认；账号等待之后复查选择版本和站点暂停；浏览器实例统计不再把启动预约槽当存活实例。

## 覆盖边界与启用要求

1. 先运行业务项目的显式迁移入口（脚手架为 `go run . -migrate`），补齐 `crawler_config.revision`、`crawler_tasks.account_pool` 和两张账号管理表。普通启动不自动迁移。当前没有替业务应用执行这一动作。
2. 请求必须保留治理 context。原始 Rod 页面、新 popup、独立 Worker、业务替换 CDP Fetch 处理器及自建非受管客户端不自动受控；第三方 origin 需显式加入范围。下载认证仍由业务提供。
3. 认领后的任务采用非抢占许可，后续请求等待不会让出任务许可；公平性是就绪工作在资源可用时获得服务机会，不是突破远端冷却或内存保护的保证。
4. OA 公共浏览器池实例数不包含账号专用池或 Chromium 子进程。请求读数按站点汇总，没有每阶段请求量。代理页面展示最近反馈和管理动作审计，不提供完整代理请求历史。
5. 当前账号与切换记录持久化，池内保留最近 100 次切换。每次任务的实际账号/版本依赖已开启的任务 Trace；没有新增永久流程绑定表。
6. Windows 实际采样与可注入采样回归已验证；没有在真实 Linux 容器中做部署压力验收。真实网站的登录协议、跨账号补偿是否可行、长期负载下阈值是否合适，仍需在独立业务模块验收。

框架没有恢复 Agent，也没有加入真实站点目标、业务模型或多实例协调。
