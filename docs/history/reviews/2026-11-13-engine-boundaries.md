# Engine 同包组件职责收口

<!-- version-scope -->
> **适用版本/阶段：Papa v4 演进过程中的历史工作区快照（具体基线以正文为准；未记录精确发布版的条目不推定版本号）。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 当前使用方式以 [v4 手册导航](../../README.md) 为准。
<!-- /version-scope -->


## 目标与边界

按已批准的 ENGINE_STRUCTURE 审查结果执行，保留公开 API 和原有工作区改动，不提交。基线在 `.review-tmp/engine-boundaries/baseline`。单实例模型不变，没有新增多实例协调。

- 提交存储与投递以明确责任交接，账号池引用保护、部分成功和重复轮次语义保留。
- taskFlowService 独立拥有 GORM 事务、策略 CAS、父子关系和认领；Engine 门面校验/转换/投递。
- traceService/archiveService 使用功能配置值，不持有 Engine 或整份 Config；维护循环沿用 tickerWG，Stop 排空测试覆盖在途清理。
- breakerCoordinator 收回内部状态规则；Engine 保留既有锁序下的暂停、任务和探测协调。
- siteCatalog 使用原 breakerMu，复制声明的 map/slice，防止通过输入或返回快照越过状态边界；accountAlertGate 拥有限频窗口和容量状态。
- engine.go 的配置、依赖、站点、控制和生命周期按职责分文件。文件拆分之外，以组件接口和结构守卫约束依赖。

## 复核发现

独立复核发现批量提交先回读全批再统一投递会改变原逐条交接时序：后续慢查询会阻塞前面已持久化的任务。已恢复逐条回读/同步投递；`TestSubmitTasksDeliversBeforeNextRowReload` 阻塞第二条读取，验证第一条任务已进入队列。存储组件仅接受当前调用内的单一投递交接函数，不持有 Engine 或回调集合。这属于本轮重构回归，不能靠普通成功用例排除。

API AST 核对确认原 172 个导出声明/签名保持。独立复核确认 Stop 主流程次序未改、站点原锁未更换、账号限频的窗口/容量保持。

## 全量 race 揭示的既有测试假设

`TestPoolConcurrentFailureAndReverifyRejectsLateIdentity` 报 `duplicate failover history`，实际记录为 initial-selection a、expired a→空（task 42）、initial-selection 空→b。候选 b 的异步预验证尚未就绪时，该中间空状态合法；并发失败仍只有一个调用获胜。独立 20 次 race 已复现，不能通过单次重跑忽略。

测试应断言唯一的失败转换、最终选择及迟到反馈不改变历史，而不是固定总历史条数。本轮仅修正该测试的语义断言，不改账号池生产代码；重复与整包 race 结果见下方验证记录。

## 验证

- 全量普通测试：`GOWORK=off go test -p 2 ./... -count=1 -json`，75 个包、2650 个测试/子测试通过。日志 `full-test.jsonl`。
- 配置 MySQL、Chrome、FFmpeg 和 live Rod 的全仓 race：74 个包通过，唯一失败为上述账号池历史条数断言；原始失败日志保留在 `full-race.jsonl`，不改写为成功。账号池测试修正后目标 race 连跑 50 次通过（1.334s），账号池完整 race 重跑通过（41.979s）；结合原全仓其余 74 个包完成全部包的 race 验证。复核记录为 `account-race-recheck.txt`。
- 全仓 race 默认跳过的两个独立 DSN 用例 `TestAuthenticationMySQLEmptyCreateDisableDelete`、`TestFrameworkExistingMySQLReadOnly` 已另建隔离临时库补跑 race 并清理（`optional-mysql.log`）；`TestWriteKeepsFileMode` 因 Windows 不支持 Unix 文件权限语义跳过。
- engine 的真实 MySQL 测试、提交/任务流/熔断事务、浏览器回归在配置依赖的全仓 race 中通过。无数据竞争报告。
- 全仓 vet/build、Grab 兼容构建、Go 格式、目录文件覆盖、API AST 与差异格式检查通过。原 481 个 engine 测试声明均保留，公开 172 个声明/签名一致。

日志统一保存在 `.review-tmp/engine-boundaries/`。未执行部署或真实网站业务验收，未执行任何 Git 写操作；工作区原有修改保留。
