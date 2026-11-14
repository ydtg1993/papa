# 验证明细（2026-11-13）

<!-- version-scope -->
> **适用版本/阶段：Papa v4 演进过程中的历史工作区快照（具体基线以正文为准；未记录精确发布版的条目不推定版本号）。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 当前使用方式以 [v4 手册导航](../../../README.md) 为准。
<!-- /version-scope -->


> 本文保留修复前诊断证据与当次验证。D1–D9 后续修复和最终验证见 [修复记录](remediation.md)。
返回 [诊断与画像](README.md)。这里只记录本次实际执行，不复用历史测试通过结论。

## 执行结果

| 命令/批次 | 结果 |
| --- | --- |
| GOWORK=off go vet ./... | 退出 0 |
| GOWORK=off go build ./... | 退出 0 |
| go test ./... -count=1 -json | 退出 1；2742 个测试/子测试通过，82 跳过，1 失败；75 包通过、1 包失败、5 包无测试 |
| CGO_ENABLED=1 go test -race ./... -count=1 -json | 退出 1；同样 2742 通过、82 跳过、1 失败；未出现 DATA RACE 报告 |
| 配置本机 Chrome 后补跑 7 个相关包 | 退出 1；694 个测试/子测试通过、2 跳过、1 失败；6 包通过、pkg/browser 失败 |
| 单独导航用例 -count=3 | 退出 0，3 次均通过 |
| 单独导航用例 -race -count=3 | 退出 0，3 次均通过，无竞态报告 |

Chrome 批次覆盖 admin/server、admin/diagnostics、internal/app、internal/browserdiagnostics、pkg/browser、pkg/account、pkg/account/onboarding。通过的测试数包含子测试，不能把三批计数相加作为唯一测试数量。普通/race 两批的 82 跳过包含 MySQL、显式浏览器/ffmpeg 门控及平台差异；本机 Chrome 批次仍跳过 onboarding MySQL 的 2 个用例。

## 唯一既有失败

`pkg/browser/capture_test.go:170` 的 `TestCaptureRequestNavigationError` 要求整个 WithBrowser 调用小于 3 秒。三批观测分别为 3.2338318s、3.4997706s、7.1066817s，错误断言为“导航失败应立刻返回，实耗 …”。其它错误返回/结果对象断言未失败。

本次全仓、race 和显式浏览器批次曾并行运行，增加了本机浏览器竞争，因此不能仅凭该现象认定生产导航功能回归。单独连续复跑 3 次通过。用例计时包含借浏览器/创建页面等开销，后续应把冷启动开销与导航失败后是否多等 CaptureTimeout 分开验证；不能简单调大阈值后宣称根因解决。

Engine 普通测试 58.452s、race 330.031s，最终均通过。大阶段熔断报告用例涉及反复复制/脱敏；运行期间 CPU 持续增长，最终完成，没有死锁证据。这是测试成本观察，不等于生产吞吐基准。

## 新发现的复现证据

- filedown/m3u8 临时定向测试按“应使用派生配置超时”断言，两包都失败：base=7s、derived config=1s、actual client=7s、shared=true。
- filedown 临时定向测试按“合并失败保留旧产物”断言失败：22 字节旧文件变为部分覆盖的 20 字节内容。
- 上述测试专为复现缺陷，失败是问题证据，不与既有全仓唯一失败混淆。临时测试已删除，未放宽任何既有断言。
- 报告下载 junction、历史生成后自删、run_id=.. 父目录写入均用合成 fixture 复现；实际 CLI 两次返回 0。详见后台分报告。
- D1 内存膨胀及 D7/D8/D9 来自源码确认，没有执行 OOM、真库或长期轮询复现。

## 没有完成的验收

未配置任何真实 MySQL 测试 DSN，未读取或迁移业务数据库；未运行 ffmpeg 门控用例；未运行真实站点、真实登录 Provider、生产部署、长稳压力或强杀恢复。这里的测试结果不应被解释为这些场景已经通过。

原始记录位于 Papa 的 `.review-tmp/diagnosis/`：test.jsonl、race.jsonl、browser.jsonl、results.json、navigation-recheck.txt、navigation-race-recheck.txt、vet.txt、build.txt。下载定向复现输出位于 Papa `.review-tmp/diagnosis-capabilities-reproduction.txt`。报告 CLI 合成材料位于父工作区 `.review-tmp/admin-report-probe/`，其 junction 仅指向同一合成目录中的 outside。

## 跳过用例清单（普通全仓批次，包含子测试）

- `admin/auth/TestAuthenticationMySQLEmptyCreateDisableDelete`
- `admin/frameworkdata/TestFrameworkExistingMySQLReadOnly`
- `admin/diagnostics/TestIdentityUIRealBrowserDraftConflictBindingAndNavigation`
- `admin/configstore/TestRuntimePolicyMySQLConcurrentCAS`
- `admin/server/TestBreakerAnalysisRealBrowser`
- `internal/breakerstore/TestBreakerHistoryMySQLAppendRetryAndHistoricalDay`
- `internal/breakerstore/TestBreakerHistoryMySQLFailureRollsBackControl`
- `internal/breakerstore/TestBreakerStoreMySQLLifecycleRestartAndRevision`
- `internal/browserdiagnostics/TestIdentityDiagnosticRealBrowserFirstDocumentAndCleanup`
- `internal/app/TestAccountOperationsUIRealBrowserPolicyAndPartialCompensation`
- `internal/app/TestAccountsUIRealBrowserDraftLoginIdentityAndSecretFields`
- `internal/app/TestAccountsUIRealBrowserNullFieldsAndLateResponseOwnership`
- `admin/server/TestMonitorFeatureCompositionRealBrowser`
- `obs/report/TestWriteKeepsFileMode`
- `pkg/account/gormstore/TestMySQLPoolAdapterExplicitMigrationCASAndRestart`
- `pkg/account/onboarding/TestRemoteRealBrowserViewportInputPopupIdentityAndOwnership`
- `pkg/account/onboarding/TestRemoteRealBrowserDisconnectCancellationAndScope`
- `pkg/browser/identity/runtime/TestIdentityMySQLMigrationCAS`
- `engine/TestBreakerSelectedTaskRetryMySQLUsesExistingVersionedPath`
- `engine/TestBreakerReadOnlyMySQLRestoresReportButInvalidatesControlProof`
- `internal/app/TestRuntimeUIRealBrowserDraftSaveConflictAndNavigation`
- `pkg/account/onboarding/TestProfileMySQLExplicitMigrationConcurrentCASAndRestart`
- `pkg/middleware/m3u8/TestEgressFFmpegMergeOnlyReadsLocalFiles`
- `pkg/middleware/m3u8/TestFFmpegConcatHandlesApostrophePaths`
- `engine/TestSmartBreakerMySQLRestartAndManualClosure`
- `engine/TestBreakerSerialDeadlineCancelsRealHandlerButWaitsForExitMySQL`
- `engine/TestBreakerSerialDeadlineIncludesResourceWaitMySQL`
- `engine/TestBreakerSerialRequeueExcludesNormalAdmissionMySQL`
- `engine/TestBreakerSerialDelayedClaimCannotRenewDeadlineMySQL`
- `engine/TestBreakerSerialLateErrorClassificationMySQL/cancel`
- `engine/TestBreakerSerialLateErrorClassificationMySQL/wrapped-deadline`
- `engine/TestBreakerSerialLateErrorClassificationMySQL/business`
- `engine/TestBreakerSerialLateErrorClassificationMySQL/joined`
- `engine/TestBreakerSettlementIntentBeforeTimingRetryMySQL`
- `engine/TestBreakerSettlementCrashCutsFromHandlerMySQL/intent-ack-lost`
- `engine/TestBreakerSettlementCrashCutsFromHandlerMySQL/terminal-ack-lost`
- `engine/TestBreakerSettlementCrashCutsFromHandlerMySQL/terminal-before-count`
- `engine/TestBreakerSettlementCrashCutsFromHandlerMySQL/count-before-commit`
- `engine/TestBreakerSettlementCrashCutsFromHandlerMySQL/count-ack-lost`
- `engine/TestBreakerSettlementCrashCutsFromHandlerMySQL/complete`
- `engine/TestBreakerSettlementOnlyTerminalAttemptsJournalMySQL/retry-success`
- `engine/TestBreakerSettlementOnlyTerminalAttemptsJournalMySQL/exhausted`
- `engine/TestBreakerSettlementOnlyTerminalAttemptsJournalMySQL/panic`
- `engine/TestBreakerSettlementOnlyTerminalAttemptsJournalMySQL/cancel-only`
- `engine/TestBreakerSettlementOnlyTerminalAttemptsJournalMySQL/auxiliary-panic`
- `engine/TestBreakerSettlementIntentOutageDoesNotRunAuxiliaryWritesMySQL`
- `engine/TestBreakerSettlementRestartAndLostAcknowledgementMySQL/write-failure`
- `engine/TestBreakerSettlementRestartAndLostAcknowledgementMySQL/lost-ack`
- `engine/TestBreakerSettlementRestartAndLostAcknowledgementMySQL/timing-failure`
- `engine/TestBreakerSettlementCheckpointOutageAndRoundConflictMySQL/round-conflict-false`
- `engine/TestBreakerSettlementCheckpointOutageAndRoundConflictMySQL/round-conflict-true`
- `engine/TestCompensationMySQLPersistenceRecoveryCASAndIdentity`
- `engine/TestRepeatSubmissionMySQLConcurrent/SubmitTask/changed-rows`
- `engine/TestRepeatSubmissionMySQLConcurrent/SubmitTasks/changed-rows`
- `engine/TestRepeatSubmissionMySQLConcurrent/mixed/changed-rows`
- `engine/TestRepeatSubmissionMySQLConcurrent/SubmitTask/found-rows`
- `engine/TestRepeatSubmissionMySQLConcurrent/SubmitTasks/found-rows`
- `engine/TestRepeatSubmissionMySQLConcurrent/mixed/found-rows`
- `engine/TestRepeatSubmissionMySQLRoundsAndRestart`
- `engine/TestRepeatSubmissionMySQLFirstRoundConcurrent`
- `engine/TestRepeatSubmissionMySQLLostAcknowledgement/first-round`
- `engine/TestRepeatSubmissionMySQLLostAcknowledgement/next-round-reset`
- `engine/TestTaskIdentityMySQLDistinctIDsSameURLAndRestart`
- `engine/TestTaskIdentityMySQLExplicitMigration`
- `engine/TestTaskIdentityMySQLMigrationRejectsCanonicalConflict`
- `engine/TestTaskTimingMySQLPersistenceRecoveryAndAttempts`
- `engine/TestTaskTimingMySQLBatchExplicitDeadlineAndNegativeDelay`
- `engine/TestTaskFlowMySQLSwitchRestartConcurrencyAndBoundaries`
- `engine/TestTaskFlowServiceMySQLLineageAndPolicyCAS`
- `engine/TestTaskFlowServiceMySQLRegistrationCancellationRollsBack`
- `pkg/account/TestIdentityAccountBrowserBridgeRestartAndFailedSwitch`
- `pkg/account/TestIdentityAccountDurableCandidateSurvivesUncertainPublish`
- `pkg/browser/TestIdentityDiagnosticStartupRealChromeUsesManagedFactory`
- `pkg/browser/TestEgressRealBrowserResourcesRedirectAndPopup`
- `pkg/browser/TestGovernanceRealBrowserNavigationXHRSubresourceRedirectAndBlock`
- `pkg/browser/TestGovernanceRealBrowserCooldownAndCancelledPendingRequests`
- `pkg/browser/TestGovernanceRealBrowserThirdPartyExcludedFromTargetLatency`
- `pkg/browser/TestGovernanceRealBrowserExistingPageHotEnableDisable`
- `pkg/browser/TestIdentityFirstScriptHeadersStateAndProtection`
- `pkg/browser/TestIdentityRuntimeMismatchAndInitialConflict`
- `pkg/browser/TestIdentityTwoAccountsSameBrowserRemainIndependent`
- `pkg/browser/TestLivePoolTempDirCleanup`

当前收尾状态见 [闭环清单](closure.md)；本文的修复前发现和当次未验证项作为历史证据保留，不能代替最新验证结论。
