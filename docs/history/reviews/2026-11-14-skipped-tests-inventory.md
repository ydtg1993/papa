# 124 项跳过测试逐项清单

<!-- version-scope -->
> **适用版本/阶段：独立 Papa v4 的 2026-11-14 工作区验证快照。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 当前使用方式以 [v4 手册导航](../../README.md) 为准。
<!-- /version-scope -->


日期：2026-11-14。概况、执行方法、修复与证明边界见 [补验报告](2026-11-14-skipped-tests-verification.md)。

计数以 `final-recheck-all-verified.jsonl` 的 `Action=skip` 且含 `Test` 的记录为准，包含子测试，保留原始 124 个 ID；不把本轮新增子测试混入分母。普通全仓与 race 的原始 skip ID 集合另行核对。

“上一轮”仅对照 `final-recheck-mysql-final.log` 与 `final-recheck-settlement-verified.log` 中精确测试名的实际结果；“无直接记录”不等于历史从未测过，但不能借用同类功能绿灯推定通过。日志位于 `.review-tmp/`。

## MySQL 独占合成库（94 项）

| 功能 / 包 | 原始跳过 ID | 上一轮直接证据 | 本轮结果 | 本轮日志 |
| --- | --- | --- | --- | --- |
| 鉴权令牌实时生效<br>`admin/auth` | `TestAuthenticationMySQLEmptyCreateDisableDelete` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| 运行策略并发 CAS 与历史事务<br>`admin/configstore` | `TestRuntimePolicyMySQLConcurrentCAS` | 无直接记录 | 通过（race） | `skip-mysql-supplement.jsonl` |
| 后台框架表只读查询<br>`admin/frameworkdata` | `TestFrameworkExistingMySQLReadOnly` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| 熔断恢复、串行探测与结算<br>`engine` | `TestBreakerSelectedTaskRetryMySQLUsesExistingVersionedPath` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| 熔断恢复、串行探测与结算<br>`engine` | `TestBreakerReadOnlyMySQLRestoresReportButInvalidatesControlProof` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| 熔断恢复、串行探测与结算<br>`engine` | `TestSmartBreakerMySQLRestartAndManualClosure` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| 熔断恢复、串行探测与结算<br>`engine` | `TestBreakerSerialDeadlineCancelsRealHandlerButWaitsForExitMySQL` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 熔断恢复、串行探测与结算<br>`engine` | `TestBreakerSerialDeadlineIncludesResourceWaitMySQL` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 熔断恢复、串行探测与结算<br>`engine` | `TestBreakerSerialRequeueExcludesNormalAdmissionMySQL` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 熔断恢复、串行探测与结算<br>`engine` | `TestBreakerSerialDelayedClaimCannotRenewDeadlineMySQL` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 熔断恢复、串行探测与结算<br>`engine` | `TestBreakerSerialLateErrorClassificationMySQL/cancel` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 熔断恢复、串行探测与结算<br>`engine` | `TestBreakerSerialLateErrorClassificationMySQL/wrapped-deadline` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 熔断恢复、串行探测与结算<br>`engine` | `TestBreakerSerialLateErrorClassificationMySQL/business` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 熔断恢复、串行探测与结算<br>`engine` | `TestBreakerSerialLateErrorClassificationMySQL/joined` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 熔断恢复、串行探测与结算<br>`engine` | `TestBreakerSettlementIntentBeforeTimingRetryMySQL` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 熔断恢复、串行探测与结算<br>`engine` | `TestBreakerSettlementCrashCutsFromHandlerMySQL/intent-ack-lost` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 熔断恢复、串行探测与结算<br>`engine` | `TestBreakerSettlementCrashCutsFromHandlerMySQL/terminal-ack-lost` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 熔断恢复、串行探测与结算<br>`engine` | `TestBreakerSettlementCrashCutsFromHandlerMySQL/terminal-before-count` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 熔断恢复、串行探测与结算<br>`engine` | `TestBreakerSettlementCrashCutsFromHandlerMySQL/count-before-commit` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 熔断恢复、串行探测与结算<br>`engine` | `TestBreakerSettlementCrashCutsFromHandlerMySQL/count-ack-lost` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 熔断恢复、串行探测与结算<br>`engine` | `TestBreakerSettlementCrashCutsFromHandlerMySQL/complete` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 熔断恢复、串行探测与结算<br>`engine` | `TestBreakerSettlementOnlyTerminalAttemptsJournalMySQL/retry-success` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 熔断恢复、串行探测与结算<br>`engine` | `TestBreakerSettlementOnlyTerminalAttemptsJournalMySQL/exhausted` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 熔断恢复、串行探测与结算<br>`engine` | `TestBreakerSettlementOnlyTerminalAttemptsJournalMySQL/panic` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 熔断恢复、串行探测与结算<br>`engine` | `TestBreakerSettlementOnlyTerminalAttemptsJournalMySQL/cancel-only` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 熔断恢复、串行探测与结算<br>`engine` | `TestBreakerSettlementOnlyTerminalAttemptsJournalMySQL/auxiliary-panic` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 熔断恢复、串行探测与结算<br>`engine` | `TestBreakerSettlementIntentOutageDoesNotRunAuxiliaryWritesMySQL` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 熔断恢复、串行探测与结算<br>`engine` | `TestBreakerSettlementRestartAndLostAcknowledgementMySQL/write-failure` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 熔断恢复、串行探测与结算<br>`engine` | `TestBreakerSettlementRestartAndLostAcknowledgementMySQL/lost-ack` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 熔断恢复、串行探测与结算<br>`engine` | `TestBreakerSettlementRestartAndLostAcknowledgementMySQL/timing-failure` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 熔断恢复、串行探测与结算<br>`engine` | `TestBreakerSettlementCheckpointOutageAndRoundConflictMySQL/round-conflict-false` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 熔断恢复、串行探测与结算<br>`engine` | `TestBreakerSettlementCheckpointOutageAndRoundConflictMySQL/round-conflict-true` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 任务身份与重启<br>`engine` | `TestCompensationMySQLPersistenceRecoveryCASAndIdentity` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| 重复提交与轮次 CAS<br>`engine` | `TestRepeatSubmissionMySQLConcurrent/SubmitTask/changed-rows` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| 重复提交与轮次 CAS<br>`engine` | `TestRepeatSubmissionMySQLConcurrent/SubmitTasks/changed-rows` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| 重复提交与轮次 CAS<br>`engine` | `TestRepeatSubmissionMySQLConcurrent/mixed/changed-rows` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| 重复提交与轮次 CAS<br>`engine` | `TestRepeatSubmissionMySQLConcurrent/SubmitTask/found-rows` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| 重复提交与轮次 CAS<br>`engine` | `TestRepeatSubmissionMySQLConcurrent/SubmitTasks/found-rows` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| 重复提交与轮次 CAS<br>`engine` | `TestRepeatSubmissionMySQLConcurrent/mixed/found-rows` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| 重复提交与轮次 CAS<br>`engine` | `TestRepeatSubmissionMySQLRoundsAndRestart` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| 重复提交与轮次 CAS<br>`engine` | `TestRepeatSubmissionMySQLFirstRoundConcurrent` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| 重复提交与轮次 CAS<br>`engine` | `TestRepeatSubmissionMySQLLostAcknowledgement/first-round` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| 重复提交与轮次 CAS<br>`engine` | `TestRepeatSubmissionMySQLLostAcknowledgement/next-round-reset` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| 积压、短时持续负载、崩溃与失败清理<br>`engine` | `TestResilienceMySQLRejectsCorruptEvidence` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| 积压、短时持续负载、崩溃与失败清理<br>`engine` | `TestResilienceMySQLFailureCleanup` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| 积压、短时持续负载、崩溃与失败清理<br>`engine` | `TestSingleInstanceMySQLCrashRecovery/before-effect` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| 积压、短时持续负载、崩溃与失败清理<br>`engine` | `TestSingleInstanceMySQLCrashRecovery/after-effect` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| 积压、短时持续负载、崩溃与失败清理<br>`engine` | `TestSingleInstanceMySQLCrashRecovery/stop-timeout` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| 积压、短时持续负载、崩溃与失败清理<br>`engine` | `TestSingleInstanceMySQLSoak` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| 调度预算、持久化补充与背压<br>`engine` | `TestSchedulerMySQLRecursiveDurableRefillWithOneToken` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 调度预算、持久化补充与背压<br>`engine` | `TestSchedulerMySQLStaleRoundAndDelayedBatchPointer` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 调度预算、持久化补充与背压<br>`engine` | `TestSchedulerMySQLPausedSpillReleasesCapacityForOtherSite` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 调度预算、持久化补充与背压<br>`engine` | `TestSchedulerMySQLWaitingFlowDoesNotPinLastToken` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 调度预算、持久化补充与背压<br>`engine` | `TestSchedulerMySQLPauseOffloadsExistingPoolAndDelay/delayed=false` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 调度预算、持久化补充与背压<br>`engine` | `TestSchedulerMySQLPauseOffloadsExistingPoolAndDelay/delayed=true` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 调度预算、持久化补充与背压<br>`engine` | `TestSchedulerMySQLStopCountsDurableBeforeApply` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 调度预算、持久化补充与背压<br>`engine` | `TestSchedulerMySQLDelayRefillInitializesFirstRound` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 完整 App 生命周期、混合负载与排空<br>`internal/app` | `TestAppMySQLActiveDrain` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 完整 App 生命周期、混合负载与排空<br>`internal/app` | `TestAppMySQLSaturatedFairness` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| 完整 App 生命周期、混合负载与排空<br>`internal/app` | `TestAppMySQLFailureCleanup` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| 任务身份与重启<br>`engine` | `TestTaskIdentityMySQLDistinctIDsSameURLAndRestart` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| 任务计时与尝试持久化<br>`engine` | `TestTaskTimingMySQLPersistenceRecoveryAndAttempts` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 任务计时与尝试持久化<br>`engine` | `TestTaskTimingMySQLBatchExplicitDeadlineAndNegativeDelay` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 任务流与策略事务<br>`engine` | `TestTaskFlowMySQLSwitchRestartConcurrencyAndBoundaries` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| 任务流与策略事务<br>`engine` | `TestTaskFlowServiceMySQLLineageAndPolicyCAS` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| 任务流与策略事务<br>`engine` | `TestTaskFlowServiceMySQLRegistrationCancellationRollsBack` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| obs 故障隔离<br>`internal/app` | `TestAppMySQLObservationIsolation/off` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| obs 故障隔离<br>`internal/app` | `TestAppMySQLObservationIsolation/archive-blocked` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| obs 故障隔离<br>`internal/app` | `TestAppMySQLObservationIsolation/archive-panic` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| obs 故障隔离<br>`internal/app` | `TestAppMySQLObservationIsolation/report-unwritable` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| 完整 App 生命周期、混合负载与排空<br>`internal/app` | `TestAppMySQLMixedLifecycle/graceful` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 完整 App 生命周期、混合负载与排空<br>`internal/app` | `TestAppMySQLMixedLifecycle/cancel` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 完整 App 生命周期、混合负载与排空<br>`internal/app` | `TestAppMySQLMixedLifecycle/timeout` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 完整 App 生命周期、混合负载与排空<br>`internal/app` | `TestAppMySQLMixedLifecycle/crash` | 已实库通过 | 通过（race） | `skip-mysql-race.jsonl` |
| 熔断控制 / 历史原子持久化<br>`internal/breakerstore` | `TestBreakerHistoryMySQLAppendRetryAndHistoricalDay` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| 熔断控制 / 历史原子持久化<br>`internal/breakerstore` | `TestBreakerHistoryMySQLFailureRollsBackControl` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| 熔断控制 / 历史原子持久化<br>`internal/breakerstore` | `TestBreakerStoreMySQLLifecycleRestartAndRevision` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| v4 迁移、旧表拒绝与百万行校验<br>`internal/database` | `TestV4MySQLFreshRepeatMigrationAndTaskRound` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| v4 迁移、旧表拒绝与百万行校验<br>`internal/database` | `TestV4MySQLRejectsOldTablesWithoutWrites/missing_identity` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| v4 迁移、旧表拒绝与百万行校验<br>`internal/database` | `TestV4MySQLRejectsOldTablesWithoutWrites/old_column` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| v4 迁移、旧表拒绝与百万行校验<br>`internal/database` | `TestV4MySQLRejectsOldTablesWithoutWrites/old_key` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| v4 迁移、旧表拒绝与百万行校验<br>`internal/database` | `TestV4MySQLRejectsOldTablesWithoutWrites/old_hash` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| v4 迁移、旧表拒绝与百万行校验<br>`internal/database` | `TestV4MySQLRejectsOldTablesWithoutWrites/old_index` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| v4 迁移、旧表拒绝与百万行校验<br>`internal/database` | `TestV4MySQLRejectsOldTablesWithoutWrites/old_url_index` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| v4 迁移、旧表拒绝与百万行校验<br>`internal/database` | `TestV4MySQLRejectsOldTablesWithoutWrites/null_type` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| v4 迁移、旧表拒绝与百万行校验<br>`internal/database` | `TestV4MySQLRejectsOldTablesWithoutWrites/invalid_type` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| v4 迁移、旧表拒绝与百万行校验<br>`internal/database` | `TestV4MySQLRejectsOldTablesWithoutWrites/empty_hash` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| v4 迁移、旧表拒绝与百万行校验<br>`internal/database` | `TestV4MySQLRejectsOldTablesWithoutWrites/null_hash` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| v4 迁移、旧表拒绝与百万行校验<br>`internal/database` | `TestV4MySQLRejectsOldTablesWithoutWrites/empty_value` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| v4 迁移、旧表拒绝与百万行校验<br>`internal/database` | `TestV4MySQLRejectsOldTablesWithoutWrites/null_value` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| v4 迁移、旧表拒绝与百万行校验<br>`internal/database` | `TestV4MySQLIdentityStartupScale` | 无直接记录 | 通过（race） | `skip-mysql-supplement.jsonl` |
| 账号池迁移、CAS 与重启<br>`pkg/account/gormstore` | `TestMySQLPoolAdapterExplicitMigrationCASAndRestart` | 无直接记录 | 通过（race） | `skip-mysql-supplement.jsonl` |
| 账号接入配置与浏览器远程接管<br>`pkg/account/onboarding` | `TestProfileMySQLExplicitMigrationConcurrentCASAndRestart` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |
| 浏览器身份迁移与 CAS<br>`pkg/browser/identity/runtime` | `TestIdentityMySQLMigrationCAS` | 无直接记录 | 通过（race） | `skip-mysql-race.jsonl` |

## 本地 Chrome + 合成 HTTP（23 项）

| 功能 / 包 | 原始跳过 ID | 上一轮直接证据 | 本轮结果 | 本轮日志 |
| --- | --- | --- | --- | --- |
| 身份诊断 UI<br>`admin/diagnostics` | `TestIdentityUIRealBrowserDraftConflictBindingAndNavigation` | 无直接记录 | 通过（race） | `skip-browser-admin-diagnostics.jsonl` |
| 后台页面组合与熔断分析<br>`admin/server` | `TestBreakerAnalysisRealBrowser` | 无直接记录 | 通过（race） | `skip-browser-admin-server.jsonl` |
| 后台页面组合与熔断分析<br>`admin/server` | `TestMonitorFeatureCompositionRealBrowser` | 无直接记录 | 通过（race） | `skip-browser-admin-server.jsonl` |
| 后台账号 / 运行策略页面<br>`internal/app` | `TestAccountOperationsUIRealBrowserPolicyAndPartialCompensation` | 无直接记录 | 通过（race） | `skip-browser-internal-app.jsonl` |
| 后台账号 / 运行策略页面<br>`internal/app` | `TestAccountsUIRealBrowserDraftLoginIdentityAndSecretFields` | 无直接记录 | 通过（race） | `skip-browser-internal-app.jsonl` |
| 后台账号 / 运行策略页面<br>`internal/app` | `TestAccountsUIRealBrowserNullFieldsAndLateResponseOwnership` | 无直接记录 | 通过（race） | `skip-browser-internal-app.jsonl` |
| 浏览器身份诊断<br>`internal/browserdiagnostics` | `TestIdentityDiagnosticRealBrowserFirstDocumentAndCleanup` | 无直接记录 | 通过（race） | `skip-browser-internal-browserdiagnostics.jsonl` |
| 后台账号 / 运行策略页面<br>`internal/app` | `TestRuntimeUIRealBrowserDraftSaveConflictAndNavigation` | 无直接记录 | 通过（race） | `skip-browser-internal-app.jsonl` |
| 账号接入配置与浏览器远程接管<br>`pkg/account/onboarding` | `TestRemoteRealBrowserViewportInputPopupIdentityAndOwnership` | 无直接记录 | 通过（race） | `skip-browser-pkg-account-onboarding.jsonl` |
| 账号接入配置与浏览器远程接管<br>`pkg/account/onboarding` | `TestRemoteRealBrowserDisconnectCancellationAndScope` | 无直接记录 | 通过（race） | `skip-browser-pkg-account-onboarding.jsonl` |
| 账号与浏览器身份桥接<br>`pkg/account` | `TestIdentityAccountBrowserBridgeRestartAndFailedSwitch` | 无直接记录 | 通过（race） | `skip-browser-pkg-account.jsonl` |
| 账号与浏览器身份桥接<br>`pkg/account` | `TestIdentityAccountDurableCandidateSurvivesUncertainPublish` | 无直接记录 | 通过（race） | `skip-browser-pkg-account.jsonl` |
| 浏览器身份、网络治理与临时目录<br>`pkg/browser` | `TestIdentityDiagnosticStartupRealChromeUsesManagedFactory` | 无直接记录 | 通过（race） | `skip-browser-pkg-browser.jsonl` |
| 浏览器身份、网络治理与临时目录<br>`pkg/browser` | `TestEgressRealBrowserResourcesRedirectAndPopup` | 无直接记录 | 通过（race） | `skip-browser-pkg-browser.jsonl` |
| 浏览器身份、网络治理与临时目录<br>`pkg/browser` | `TestGovernanceRealBrowserNavigationXHRSubresourceRedirectAndBlock` | 无直接记录 | 通过（race） | `skip-browser-pkg-browser.jsonl` |
| 浏览器身份、网络治理与临时目录<br>`pkg/browser` | `TestGovernanceRealBrowserCooldownAndCancelledPendingRequests` | 无直接记录 | 通过（race） | `skip-browser-pkg-browser.jsonl` |
| 浏览器身份、网络治理与临时目录<br>`pkg/browser` | `TestGovernanceRealBrowserThirdPartyExcludedFromTargetLatency` | 无直接记录 | 通过（race） | `skip-browser-pkg-browser.jsonl` |
| 浏览器身份、网络治理与临时目录<br>`pkg/browser` | `TestGovernanceRealBrowserExistingPageHotEnableDisable` | 无直接记录 | 通过（race） | `skip-browser-pkg-browser.jsonl` |
| 浏览器身份、网络治理与临时目录<br>`pkg/browser` | `TestIdentityFirstScriptHeadersStateAndProtection` | 无直接记录 | 通过（race） | `skip-browser-pkg-browser.jsonl` |
| 浏览器身份、网络治理与临时目录<br>`pkg/browser` | `TestIdentityRuntimeMismatchAndInitialConflict` | 无直接记录 | 通过（race） | `skip-browser-pkg-browser.jsonl` |
| 浏览器身份、网络治理与临时目录<br>`pkg/browser` | `TestIdentityTwoAccountsSameBrowserRemainIndependent` | 无直接记录 | 通过（race） | `skip-browser-pkg-browser.jsonl` |
| 浏览器身份、网络治理与临时目录<br>`pkg/browser` | `TestLivePoolTempDirCleanup` | 无直接记录 | 通过（race） | `skip-browser-pkg-browser.jsonl` |
| 浏览器身份、网络治理与临时目录<br>`pkg/browser` | `TestLivePoolClosePreservesBorrowedAndSharedProfiles` | 无直接记录 | 通过（race） | `skip-browser-pkg-browser.jsonl` |

## 本地 FFmpeg + 合成媒体（2 项）

| 功能 / 包 | 原始跳过 ID | 上一轮直接证据 | 本轮结果 | 本轮日志 |
| --- | --- | --- | --- | --- |
| 媒体合并、路径与输出预算<br>`pkg/middleware/m3u8` | `TestEgressFFmpegMergeOnlyReadsLocalFiles` | 无直接记录 | 通过（race） | `skip-media-m3u8-race.jsonl` |
| 媒体合并、路径与输出预算<br>`pkg/middleware/m3u8` | `TestFFmpegConcatHandlesApostrophePaths` | 无直接记录 | 通过（race） | `skip-media-m3u8-race.jsonl` |

## WSL Linux 原生临时目录（2 项）

| 功能 / 包 | 原始跳过 ID | 上一轮直接证据 | 本轮结果 | 本轮日志 |
| --- | --- | --- | --- | --- |
| 报告文件权限<br>`obs/report` | `TestWriteKeepsFileMode` | 无直接记录 | 通过（Linux 实际执行，无 Linux race） | `skip-media-linux-report.jsonl` |
| 下载产物权限<br>`pkg/middleware/filedown` | `TestMergedArtifactPreservesPermissions` | 无直接记录 | 通过（Linux 实际执行，无 Linux race） | `skip-media-linux-filedown.jsonl` |

## 父测试专用入口（3 项）

| 功能 / 包 | 原始跳过 ID | 上一轮直接证据 | 本轮结果 | 本轮日志 |
| --- | --- | --- | --- | --- |
| 进程生命周期 / 父测试入口<br>`engine` | `TestResilienceMySQLChild` | 无直接记录 | 父测试已驱动（含预期强杀/失败） | `skip-mysql-race.jsonl` |
| 进程生命周期 / 父测试入口<br>`internal/app` | `TestAppMySQLLoadChild` | 已由父测试驱动 | 父测试已驱动（含预期强杀/失败） | `skip-mysql-race.jsonl` |
| 进程生命周期 / 父测试入口<br>`internal/app` | `TestAppMySQLMixedChild` | 已由父测试驱动 | 父测试已驱动（含预期强杀/失败） | `skip-mysql-race.jsonl` |
