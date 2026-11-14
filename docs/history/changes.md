# 变更记录

<!-- version-scope -->
> **适用版本/阶段：跨阶段历史变更汇编，不能整体视为当前 v4 功能清单。**
> 各条保留当时的症状、修法和验证。SQL Trace、旧配置/API 兼容及手动追踪等条目已被独立 v4 契约替代，不据此操作当前版本。 当前使用方式以 [v4 手册导航](../README.md) 为准。
<!-- /version-scope -->


## 破坏性变更

- **2026-11-09：移除已无作用的模型密钥构造入口，任务流审计使用准确类型名。** `configstore.NewWithModelKeyFile(db, path)` 及只赋值不使用的 `modelKeyFile` 已删除；调用方改用 `configstore.New(db)`。`server.AgentActionEvent` 改为 `server.TaskFlowActionEvent`，删除未使用的 `OperationID` 字段；自定义 `OnTaskFlowAction` 回调同步修改参数类型。原因是模型配置模块已移除，保留这些接口会误导调用者以为密钥文件仍被使用。代价是直接引用旧符号的宿主需要改代码；无需数据库迁移，账号运行时的加密凭据存储不变。通过版本代码盘点发现，使用全包编译和后台测试验证调用链。

- **2026-11-12：普通恢复改为重试验证，强制解除使用明确入口（破坏性行为变更）。** `Engine.ResumeSite/ResumeCrawling` 和后台 `/api/breaker/resume` 不再直接绕过自动保护，只有纯人工暂停直接解除；需要绕过时显式调用 `ForceResumeSite` / `/api/breaker/force-resume`，并单独审计。此改动来自用户要求，避免把操作员点击当成健康证明。代价是旧脚本调用普通恢复后需等待全阶段验证；升级时检查这些调用。事件已有 JSON 列改存控制、失败窗口和报告的快照，不新增表列，无需额外迁移；旧格式可读但缺精确窗口的活动事件转人工处理，人工重试后重新验证。回归 `TestBreakerRetryAndForceAreSeparateEndpoints`、`TestBreakerReviewRetryKeepsProtectionAndCounts`。

- **2026-11-13：Trace 改为纯文件，默认仅保留异常尝试（破坏性变更）。** 用户在写多读少的成本分析后明确选择不再保留 SQL 存储，并要求主动追踪扩展口和默认七天清理。新增 internal/tracestore 拥有文件格式、原子发布、查询、容量及清理；Engine 保留异常判定、有界缓冲和下一执行选择，公开步骤/查询方法及 TraceStep 布局保留。正常成功不落盘，失败、panic、显式 Fail/Warn 和主动选中的执行留存；正常关停取消不单独制造异常。单实例目录容量满时拒绝新证据并告警，不改任务结果。

  **升级步骤与代价**：无需为 Trace 运行 migrate；常规模型清单和启动检查已排除旧表，不自动删除或读取旧 SQL 历史，也不自动导出。目录/容量可在 crawler.trace_files 配置；原 trace.enabled/retention 继续生效。默认七天保留不能覆盖进程硬杀前尚在内存的步骤；断电持久性、慢盘及容量风险见专项报告。后台按钮尚未增加，Engine.TraceNextExecution 预留下一次执行入口。

  回归包含 TestTraceFileRetentionPolicy、TestTraceCancellationAndDeadline、TestTraceManualSelectionOneShotRetryAndCapacity、TestTracePersistenceFailureDoesNotChangeOutcome、TestCleanupContinuesAfterCorruptRecord、TestLoweredQuotasAllowCleanup；通过 API 兼容、文件故障和本地合成 HTTP/浏览器检查发现并修正 JSON 编码语义与取消记录边界。全仓验证额外出现真实输出 `state page leaked`，并以 TestPageCreationCancellationClosesPartialTarget 确定性复现另一个创建中断清理缺陷：`created=true closed=false bounded=false`；浏览器组件现用独立三秒清理 context，保留原取消错误。原高负载失败与该缺陷是否同因未证实。各批验证及未验证项见 [Trace 文件实施](reviews/2026-11-13-framework-diagnosis/trace-files.md)，不以历史 MySQL 测试证明新文件实现。

## 修复

1. **2026-11-12：延迟投递和最近执行时间进入任务持久化契约。** 源码审查发现原 `Task.Delay/NotBefore` 仅进入内存堆，恢复时没有可重建的排期；用户要求将延迟及最近执行时间落库。增加 `delay_ms/not_before/last_started_at/last_finished_at`，首次相对排期以数据库 UTC 时钟生成，重复提交沿用既有期限；恢复、回灌、加急及数据库认领守卫共同防止提前执行。最近执行时间按每次 Handler 尝试记录，时间写入失败只重试持久化，不重跑已经执行的业务。代价是每次尝试增加时间记录写入，数据库持续故障时保留责任并背压；升级须停 worker 后显式执行 `papa migrate`，不改业务数据、不自动启动迁移。`TestTaskTimingRestoresDeadlineAndClaimSQL`、`TestAttemptFinishPersistenceFailureNeverReplaysHandler`、`TestAttemptStartPersistenceFailureDoesNotCallHandler`、`TestClaimRejectedByDurableDeadlineRetainsTask`、`TestSubmitExistingTaskCannotReplacePersistedDeadline` 与 `TestTaskTimingMySQLPersistenceRecoveryAndAttempts` 覆盖这些边界。此项通过审查和合成回归发现，无线上报错原文；完整规则见 `docs/runtime/TASK_TIMING.md`。

1. **2026-11-09：已删除的 Agent/规则体系仍在后台显示入口并引用不存在的资源。** 清除周期批次、规则发布、运行质量、Agent 候选、模型设置和 Agent 活动区的模板、菜单、切换/登录回调及样式，删除工作台的失效批次链接。保留动态任务流、三条治理队列、普通采集、账号、网络录制、字段校验和业务页面扩展。版本对比及 Node 行为测试复现：`ENOENT: no such file or directory, open '…/schedules.js'`（省略机器临时目录）。删除仅针对这些已移除功能的 `native_session_test.js`；通用导航和重新登录刷新改用仍在运行的页面验证。代价是旧功能不再有空壳入口，不恢复对应后端。`TestMonitorTemplateLocalAssetsExist` 检查本地资源存在性，`TestOperationsJavaScriptBehavior` 覆盖当前导航、工作台和会话切换。

2. **2026-11-09：框架表回归仍期待旧模型，现有任务流 JSON 脱敏漏掉 policy。** 旧测试输出 `catalog/model count = 6/6`、`unknown table`；测试迁移到当前六类模型，保留只读鉴权、投影、分页、稳定排序、缺表恢复和脱敏覆盖。迁移样本后发现 `policy` 只做字符串替换，嵌套 JSON 中的 `api_key` / Cookie 未被隐藏；现按 GORM schema 标识 JSON 列后统一递归脱敏，非法 JSON 不回显。代价是只读框架表中的敏感内容被遮盖，原始数据不修改。回归：`TestFrameworkCatalogCoversAllModelsReadOnly`、`TestFrameworkSQLProjectionPaginationAndStableOrder`、`TestFrameworkResponseSanitizesNestedJSONAndBinaryIDs`。

3. **2026-11-09：移除 Agent 后的任务流解码不再拒绝重复 JSON 键。** 后台定向测试复现 `wrong arguments`、`POST status 409 want 400`，重复 revision 请求错误地进入策略更新。任务流使用独立、有界、拒绝重复键的解码器，同时拒绝未知字段、多份 JSON 和过深嵌套。代价是含歧义或额外字段的请求返回 400，合法策略 API 不变。`TestTaskFlowHTTPExplicitCASAndAuthentication` 覆盖认证、版本冲突、顶层/嵌套重复键、未知字段、尾随 JSON 和超限请求；不恢复任何 Agent 依赖。

4. **2026-11-12：修复分类熔断复查中的阶段隔离、在途任务和观察误暂停。** 按用户确认的第 2—5 项施工。源码复查与合成回归复现：局部探针通过清掉全站窗口，报告每次写入都被新证据打断导致恢复期限失效，混合观察即使持续成功仍暂停。修复前真实输出：

   ```text
   unaffected list-stage failures erased/ignored: got 0, want >=2
   healthy unrelated traffic prevents enforcing expired recovery budget: state=cooldown
   observation always escalates to site pause: state=cooldown successes=51
   ```

   Detector 保留同一份终态记录，局部恢复只确认本阶段短窗口旧故障，硬总数与其他阶段结果不清零；恢复到期关闭新准入并等该范围在途任务结束，晚失败回冷却，未保存结案时的新硬失败继续原事件。管理和写入各自 select + timer，正常间隔 200ms、错误重试 1s、单批 context 2s，写入槽等待可取消；控制状态保存与证据更新区分，防止持续流量或慢库阻塞期限判断。混合观察无新增失败且每个受影响阶段至少两个不同任务成功时可退出，成功不抵消失败；系统时钟相同的结果用 Detector 到达序号区分前后。代价是恢复必须等在途结果，证据覆盖不足仍需探针，常规窗口旧失败仍能使新增失败命中硬线；不新增配置/表列，LastFailureAt 保存在已有 JSON 报告。回归：`TestDetectorResolveOnlyAcknowledgesScopedShortFaults`、`TestDetectorObservationOrdersEqualTimestamps`、`TestReviewStageProbeMustKeepOtherStageFailures`、`TestReviewRampDoesNotFinishWithUnsettledTrial`、`TestReviewBusyOtherStageMustNotStarveRecoveryBudget`、`TestReviewMixedObservationCanRejectAStaleIncident`、`TestSmartBreakerBlockedWriterCannotDelayDeadline`、`TestSmartBreakerWriterTimesNormalFlushAndFailureRetry`、`TestSmartBreakerUnsavedRecoveryCannotLoseNewHardFailure`、`TestSmartBreakerRestartDuringRampReturnsToCooldown`。人工暂停仍只在运行周期内有效；精确 Detector 窗口仍不跨重启重建，已持久化自动保护和预算照常恢复，未声称消除强杀未刷写窗口。

5. **2026-11-12：整站恢复复查五项修复，报告不再决定准入。** 用户要求复查后，隔离用例实际输出包括 `all 65 stages have two distinct successes; report Limited=true forced state=cooldown`、`valid stage key changed: input bytes=132 stored bytes=129 task successes=0`、`final incident still has unfinished stage after callback exited: state=probing finished=0001-01-01 00:00:00 +0000 UTC`、`most recently updated incident silently omitted because its first occurrence was oldest`。运行控制和原始标识独立于 Lifecycle，报告只投影；中止阶段补状态、时间与原因；obs 查活动及窗口相关持久事件，最近日志引用只作离线补充，上限显式提示。Detector 失败轮次单独保存，重启恢复原始时间、去重和容量保护，成功证明全部重验。为什么不重建报告计数：报告样本有限，不能恢复精确窗口。代价是事件 JSON 增加窗口快照、保存上限 16 MiB，强杀仍可能丢未刷写周期，事件前的窗口不单独持久化。回归 `TestBreakerReviewTimelineTruncationMustNotInvalidateHealthyStages`、`TestBreakerReviewReportCleaningMustPreserveValidStageIdentity`、`TestBreakerReviewDeadlineMustFinalizeRunningStageReport`、`TestBreakerReportLoadsSilentActiveIncidentWithoutLogEvents`、`TestCheckpointRestoresFailureIdentityAndOriginalExpiry`、`TestSmartBreakerMySQLRestartAndManualClosure`。空闲阶段仍保持严格门槛，完整只读验证方案单列在 BREAKER，尚未自动启用。

## 变更

1. **2026-11-12：任务去重统一为站点、阶段、身份类型和身份值（破坏性变更）。** 用户要求框架定义统一标准，避免自由拼接幂等键和 URL 两套唯一索引互相干扰。新增 `Task.ResourceID`，有稳定 ID 时优先，否则规范化完整 HTTP(S) URL；保留 origin、转义路径、原始查询顺序，去 fragment，规范 host/默认端口。无歧义版本化元组生成 SHA-256，内存、单条、批量、恢复与模型统一使用；新唯一索引为 `(site, stage, identity_hash)`。`Unique()` 不再返回旧拼接文本；旧 `IdempotencyKey` 暂保留为受站点阶段作用域约束的 ID 输入，业务新代码改用 `ResourceID`，不得将 Unique 当显示文案或业务远端 ID。显式迁移回填任务身份，冲突报错保留记录，不自动合并；完成后移除旧 URL/自定义键唯一索引。同 ID 重复提交沿用持久化 URL，不静默改执行地址。代价是升级需要停 worker、备份并执行 `papa migrate`，大表索引变更宜低峰；规范化后冲突需人工处理后重跑迁移，不涉及 crawler 历史业务关联。回归 `TestStandardTaskIdentity`、`TestTaskIdentityMySQLDistinctIDsSameURLAndRestart`、`TestTaskIdentityMySQLExplicitMigration`、`TestTaskIdentityMySQLMigrationRejectsCanonicalConflict`。首轮旧门面断言真实输出为 `unexpected unique key: 7b7c4c868f62f5f85566e9f72608a508b478713398cf29a3f578dca8ed4d72cd`，已按公开新契约调整断言。

2. **2026-11-09：增加令牌更新与通用浏览器诊断。** 用户反馈访问令牌行缺少更新入口，Rod 启动排查需要依赖 crawler 业务页面。新增行内更新按钮与受鉴权保护的 rotate 接口，保留原记录和启停状态、使旧凭据失效，新凭据只返回一次，审计不记明文；系统管理增加浏览器诊断，采用隔离临时目录和空白页面检查当前 Chromium/Rod 启动链路，并返回脱敏分步报告。代价是更新后其他客户端需要换凭据，诊断会短暂启动额外浏览器；不引入站点业务依赖。此项为用户提出的能力需求，无线上报错原文。回归：`TestRotateRevokesOldTokenAndKeepsIdentity`、`TestRotateAPIReturnsCurrentCredentialAndAuditsWithoutSecret`、`TestBrowserDiagnosticLaunchFailureAndCleanup`、`TestBrowserDiagnosticRedactsSecrets`。

1. **2026-11-09：同步当前后台能力与历史预检边界。** 更新 MONITOR、CORE_CONFIG、DEVELOPMENT、STRATEGY_MIGRATION，去掉 RuleFetcher 归档说明和失效批次导航。只读迁移预检保留历史表名，用于检查旧数据库；它们不重新注册到当前模型、后台或迁移清单，也不自动删除旧表。无需刷新业务代码模板，框架手册仍由 `papa new` / `papa upgrade` 同步。

验证（2026-11-09）：`GOWORK=off go test ./...`、`go vet ./...`、`go build ./...`、`git diff --check` 全部通过。真实 MySQL 集成测试所需环境变量未配置；未运行 race。未改数据库、未执行 Git 写操作。

2. **2026-11-12：分类熔断算法独立记录，暂不实施。** `docs/history/reviews/2026-11-12-breaker-analysis-design.md` 保存同指纹短时快速熔断、混合错误限时分析、窗口失败数硬保护、按 AutoThrottle 开关降频、Retry-After、有界试探与最终报告。用户明确要求先施工任务时间与身份，本批不改熔断算法和失败任务间隔。

3. **2026-11-12：实现分类熔断、受控恢复与持久事件报告。** 用户指出短时间快速失败会连续触发统一计数熔断，要求区分页面结构、迁移、网络、限流并保留最终报告。引入任务轮次去重与错误指纹，同类爆发快速保护、混合错误先受限观察，原窗口失败数仍作硬上限；429 复用已开启的 AutoThrottle，明确 Retry-After 独立遵守。新增 `crawler_breaker_incidents`，保存恢复预算和报告，语义探测通过后限量恢复，人工接管及旧轮次结果有所有权约束。代价是新增表和定期报告写入；升级要显式迁移，需要自动恢复的业务要注册只读语义探针，强杀仍可能丢失最后200ms未刷写状态。通过源码/并发审查与合成回归发现；首轮测试真实输出 `规则键 "crawler.breaker.policy"（规范化后 "crawler.breaker.policy"）在 Config 结构体里找不到`，已改为真实叶子路径上的完整策略校验。回归包括 `TestSmartBreakerFastScopeMixedObservationAndHardCeiling`、`TestSmartBreakerLateProbeCannotOverrideManualTakeover`、`TestSmartBreakerStopFlushesFinalState`、`TestSmartBreakerMySQLRestartAndManualClosure`、`TestBreakerStoreMySQLLifecycleRestartAndRevision`。全包测试/vet/build、针对性race及隔离MySQL通过；未运行真实站点故障演练。详情见 `docs/runtime/BREAKER.md` 与 `docs/history/reviews/2026-11-12-breaker-implementation.md`。此条接续前面的“暂不实施”设计记录。


4. **2026-11-12：删除已被替代的熔断计数/报告路径，人工暂停限定在运行周期内。** 用户复查后明确要求：人工暂停无需跨重启保存，新的计数和报告可替代旧实现时删除旧实现。移除 60 桶 Breaker、旧内存证据/历史、`BreakerIncidents` 引擎方法与后台回调，状态统计统一读取 Detector，报告统一使用 Lifecycle 与 breakerstore；obs 日报按 ID 查询持久事件，离线只引用不重建。此前定向复现原文为 `event points at different incident: event="1794472367457316000-2" durable="1794472367456242500-1"`；现在事件流和报告只生成一个 ID。人工完全接管自动保护时关闭自动事件，暂停自身只在内存有效，正常停机补写事件关闭记录；自动故障待人工处理仍保留。代价是旧公开报告回调调用方必须改用 BreakerReports，且离线日报不再自行推断完整熔断现场。回归 `TestBreakerReportsUseOneIDAndRetainHistory`、`TestManualTakeoverDoesNotRestoreAfterImmediateStop`、`TestNarrowManualPauseDoesNotOwnSiteProtection`、`TestSmartBreakerMySQLRestartAndManualClosure` 通过；全包测试/vet/build、定向race、隔离MySQL通过。复查第2—5项本轮仅解释，仍列待修；未提交或部署。

5. **2026-11-12：自动恢复统一为整站全阶段验证，运行过程进入同一 obs 报告（行为变更）。** 用户明确指出局部成功不能证明其他阶段健康，也不能让阶段单独恢复，要求全部阶段测试正常后才能清零，并让运营看到暂停、恢复与重试过程。先补回归，旧实现真实输出为 `local fault left the other stage running without site protection`、`idle registered stage absent from recovery: []`。现在触发自动保护即约束整站，注册阶段全部纳入验证；逐阶段语义试探全部通过后，才开始整站限量任务，各阶段本轮任务成功、观察期满足且全站在途结束后清零，报告保留清零前证据并等待保存再正常放行。任一阶段失败使本轮验证失效，下一轮全部重测；删除旧局部 Resolve 和阶段在途计数路径。Lifecycle 增加阶段结果和清零时间，统一时间线记录轮次、阶段、失败/成功统计与重试计划；新增 breaker_transition 引用让 obs 覆盖观察后恢复的事件，后台阶段表/时间线每 3s 刷新并保留历史选择。代价：`max_probes` 现在表示整站轮数，兜底探针必须按 req.Stage 验证，缺少探针或没有真实任务的阶段不能默认通过；人工强制解除不证明健康，不再清零。没有新增表/列，已有 JSON 报告自然兼容；原精确内存窗口不跨重启重建的限制仍保留。回归 `TestSmartBreakerLocalSuccessCannotReopenSite`、`TestSmartBreakerAllStagesDrainThenResetWithReport`、`TestSmartBreakerStageFailureInvalidatesWholeRound`、`TestSmartBreakerMissingStageProbeStopsWholeSite`、`TestSmartBreakerOldTaskSuccessCannotProveNewTrial`、`TestManualResumeDoesNotCertifySiteOrClearFailureWindow`，并更新并发/重启契约；MySQL 覆盖阶段进度/时间线/清零时间，真实 Chrome 覆盖阶段表、运行时间线、桌面/窄屏及注入防护。

   本项最终验证：全包 test/vet/build、定向 race 和隔离 MySQL、真实 Chrome 桌面/窄屏、Node 弹窗刷新行为通过。首次全包输出 `ReferenceError: setTimeout is not defined`，来源是旧 Node 沙箱未提供定时器；补充假定时器并增加历史选择保留、关闭/登出停止刷新回归后通过。未提交、未部署。

3. **2026-11-12：保存确认丢失后的快照重试保持可前进。** 本轮修复后复查发现：只读状态查询会清除自然过期窗口，若上次保存已提交却丢失确认，同修订重试可能带不同窗口，触发 `breaker event snapshot is stale or conflicting`。每次生成保存快照递增修订，避免额外引入缓存管理器；代价是失败重试也消耗修订号，修订仅用于单调排序，不代表操作次数。由源码交错推演及 `TestBreakerReviewLostCommitAckAndWindowExpiryCanRetry` 的丢确认夹具验证。

4. **2026-11-12：仅完整只读覆盖的阶段允许替代真实任务验证。** 用户复查空闲阶段方案后要求收紧：普通探针成功不能说明写入、下载、派发等实际操作正常，但完全只读的阶段没有任务时也需要受控的验证入口。StageSpec/StageConfig 新增 `BreakerReadOnlyCoverage`，默认空仍须真实任务；只有明确填写完整覆盖范围的阶段，才在既有逐阶段探针中要求间隔两次 `Healthy && Semantic && Complete` 且 Detail 非空。任一次不完整或失败均不能通过，重试及重启丢弃旧证明；第二次仍遵守新收到的 Retry-After。全部阶段满足各自条件后保留整站观察、在途排空和保存门槛，晚失败取消恢复，人工强制解除后的晚成功不能改成健康恢复。控制记录方式、范围与次数，报告单独派生验证结论；后台、文本、JSON、obs 不把普通探针 passed 写成阶段已验证。

   为什么：空队列和一次成功都不能证明完整阶段健康；默认不豁免其他阶段，同时避免新增测试任务队列和恢复管理器。代价：业务必须维护有效代表性样本并如实声明覆盖，框架无法静态证明回调完整；存在写入/下载/派发等未覆盖操作时不能声明替代。升级无需新表列或迁移，不增加 YAML 开关，现有业务默认行为不变。通过用户方案复核、状态机交错审查及合成夹具发现和验证；本轮首次编译真实输出为 `engine/engine.go:218:46: undefined: strings`，补齐 import 后通过。

   回归包含 `TestBreakerReadOnlyRequiresTwoSpacedChecksAndDurableFinish`、`TestBreakerReadOnlyMixedStagesKeepIdleTaskStageUnverified`、`TestBreakerReadOnlyRejectsIncompleteAndFailedSecondCheck`、`TestBreakerReadOnlySecondProbeDoesNotOverlapOrOverrideForceResume`、`TestBreakerReadOnlySecondCheckHonorsNewRetryAfter`、`TestBreakerReadOnlyRestartDiscardsProofAndUsesCurrentDeclaration`、`TestBreakerReadOnlyMySQLRestoresReportButInvalidatesControlProof`、`TestLifecycleVerificationDistinguishesProbeFromFullProof`。全量 go test、go vet、go build、定向 race、隔离 MySQL、Node 弹窗和真实 Chrome 桌面/窄屏通过；补充并发与 Retry-After 回归后再次定向 race 通过。没有执行真实业务探针验收，没有 Git 写操作或部署。

5. **2026-11-12：混合错误改为三次失败样本验证，长期保护每小时复查，obs 改读追加历史。** 用户要求三次检查优先覆盖不同失败任务，并确认每小时复查满足全部门槛后自动恢复；中断、重启和各轮范围必须保留到当日日报。此前复查夹具真实输出为 `report claims verified although control rejects repeated task: count=2 note="本轮真实任务验证通过"` 与 `restart erased earlier successful readonly coverage from report history`。现在每阶段固定最多三个不同失败样本，有三个时分别验证，只有一两个时轮换完成三个独立执行轮次；重复通知和未选任务不能充数。重跑复用原有任务版本、补偿和认领路径，探针通过 Sample/SampleID 检查指定样本并确认身份，完整只读替代仍须 Complete，其他阶段仍需真实任务。

   单轮预算耗尽后保存一小时后的复查计划；沿用现有定时循环重新给予一轮预算，全部阶段验证、观察、在途排空及保存完成后自动恢复。人工暂停不解除，未退出探针不叠加，错过多小时只做一轮；重启保留计划但丢弃旧证明。新增 `crawler_breaker_history` 追加运行里程碑、当时覆盖范围、样本、结果、停止和重启记录，与当前控制快照同事务保存，事件内序号保证重试幂等。obs 查询日初保护基线和当天记录，回查昨天不再读取今天的恢复结论；当前弹窗 128 条展示上限不删除持久历史。删除无人使用的旧 `ForWindow` 快照日报查询、`SuccessfulStagesSinceLastFailure` 以及仅用于该旧判定的到达序号，阶段证明由恢复控制记录维护，报告只投影结论。

   为什么：增加次数却不约束样本无法排除同一页面偶然成功；用可覆盖的当前快照也无法满足跨日审计。代价与升级：必须显式运行宿主迁移创建历史表；混合事件的旧探针要实际使用 req.Sample/req.SampleID 并在成功结果中回传 SampleID，未适配时保持保护。历史表随过程增长，单次日报最多展示 10000 条；内存未保存记录最多 4096 条，积压超限停止自动恢复并明确提示过程可能不完整，正常停机补写记录。非正常退出的精确时间不可得，下次启动记录发现中断，不补造过去的事件。没有独立调度服务、测试任务队列或新的恢复管理器。

   核心回归：`TestBreakerMixedThreeSelectedSamplesMatchReport`、`TestBreakerMixedFewSamplesRotateThreeFreshRounds`、`TestBreakerMixedProbeCannotIgnoreRequestedFailureSample`、`TestBreakerHourlyCycleCanAutoRecoverAndPreserveManualPause`、`TestBreakerHourlyRestartPreservesDueTimeAndRecordsOldScope`、`TestBreakerUnfinishedProbeBlocksHourlyRecheck`、`TestBreakerSelectedTaskRetryMySQLUsesExistingVersionedPath`、`TestBreakerHistoryMySQLAppendRetryAndHistoricalDay`、`TestBreakerHistoryMySQLFailureRollsBackControl`。并发复核补充历史压力与人工暂停的锁重入回归，积压时也不能清零或重开人工关闭的事件。首次 race 中 `TestBreakerHistoryBackpressureCannotResetFailures (84.42s)` 报 `history overflow reset failure evidence`，原因是大量构造展示记录耗尽了夹具的一分钟窗口；改成直接构造积压边界后定向 race 通过，未更改窗口自然过期规则。全量测试、vet/build、隔离 MySQL、Node/真实 Chrome 已验证；未对业务库迁移，未提交或部署，未做真实站点故障演练。


6. **2026-11-12：恢复验证改为同站点串行并限制样本等待/执行10分钟，修复升级门槛及日报截断误报。** 用户复查后要求测试样本串行，A 超时退出后才能继续 B，前次复查的其它问题同步修复。复查夹具真实输出：

   ```text
   already-verified sample unnecessarily requeued: reprocess=1 status=0
   old mixed snapshot recovered after one generic probe and one unrelated task; three failed samples untested
   ready failed samples never retried because first sample was pending
   truncated prefix reported as final day state: 本日报截止仍在保护：manual
   ```

   修法：原 Recovery 保存当前样本身份与截止时间；原准入锁统一仲裁正常调度和验证重投，SQL 期间占用选择位置，实际任务串行进入原 worker/认领路径。截止从选中开始，覆盖排队、账号/资源等待和执行；无法认领不能重新计时。超时 context 取消，未退出不能开始下一个或启动小时复查；A 退出后可记录 B/C 结果，但 A 保持未验证至新周期。迟到终态保留实际统计，不能补算健康证明。启动兼容入口补齐旧混合/可能由混合升级事件的三次门槛，不恢复旧成功成绩。obs 展示截断时明确无法确认截止状态；历史仍追加，不读取当天之后的恢复结果。报告新增选中、超时、退出、迟到终态及重启中断记录，同时纠正重启和小时跳过后的计划时间。

   为什么：只对验证循环串行不足以阻止正常调度抢跑，任务 CAS 也不能识别验证已经满足；而跳过超时样本不等于它健康。代价：恢复吞吐明确为每站一个样本，旧 ramp_concurrency 大于1也不增加验证并发；默认5分钟整轮预算仍优先，不因10分钟单样本上限而自动延长。业务须响应 context，忽略取消时保持保护；不新增调度队列或控制器。沿用原 JSON 控制快照，无新表列；已完成历史表迁移的环境无需新增迁移。历史追加改为只整理新条目并复用单阶段投影，避免持锁反复整理整份报告。

   测试：TestBreakerSerialAdmissionIgnoresHigherRampConcurrency、TestBreakerSerialRequeueExcludesNormalAdmissionMySQL、TestBreakerSerialDelayedClaimCannotRenewDeadlineMySQL、TestBreakerSerialDeadlineCancelsRealHandlerButWaitsForExitMySQL、TestBreakerSerialDeadlineIncludesResourceWaitMySQL、TestBreakerSerialUnfinishedTimeoutBlocksNextAndHourlyRetry、TestBreakerSerialRestartPreservesDeadlineAndRejectsExpiredSelection、TestBreakerSerialUpgradeOldMixedSnapshotKeepsThreeSamples、TestBreakerTruncatedDailyHistoryMustNotInventFinalState。来自源码交错复查、合成复现和隔离 MySQL 验证，无线上故障数据。全量测试、相关 race、vet/build 及 Node/Chrome 已验证；详见 2026-11-12-breaker-serial-resolved-issues.md。未提交、部署或迁移业务库。


7. **2026-11-12：样本超时后的实际业务错误不再被 ctx 取消分支吞掉。** 复查隔离 MySQL 夹具输出 `real late failure lost: task status=0 failures=1`：handler 返回明确解析错误，但已取消的 context 让任务直接退回 pending，漏掉失败数和 obs 错误历史。现在沿原 stageHandler 分流：取消错误链不计业务失败，独立或 joined 的业务错误使用限时收尾 context 保存终态，调用原失败统计和报告路径；串行位置到收尾结束才释放。完成时间持久化失败时保留未结算责任，避免把真实业务错误转为取消。只增加错误链判断函数，不新增分类器服务、队列或恢复门槛。代价是明确失败收尾最多等待原有2秒预算；存储不能确认时不宣称已记录成功。回归 `TestBreakerSerialLateErrorClassificationMySQL` 覆盖取消、包装取消、业务错误、取消与业务错误组合、落库期间禁止放行及 obs 错误记录；原串行、重启、迟到成功、小时复查和追加历史回归同步执行。不需要迁移，未部署或执行 Git 写操作。


8. **2026-11-12：串行样本未结算失败持久化并阻止后续验证。** 上轮复查合成故障注入的真实输出为 `next sample admitted despite unresolved failure`：终态写失败后只有内存中的 taskID/stage，worker 退出即释放串行位置，重启还会将 processing 重投。现在使用现有 Recovery.PendingFailure 保存原轮次、实际错误分类、补偿信息和截止时间，先保存结果，再条件写任务终态；确认之后原计数路径幂等记录，并把清除待确认结果和计数检查点一起保存。原报告循环在 handler 完全退出后每分钟重试结算，复用 sampleDispatching 排斥并发；三个治理队列和人工重投不能在此间替换执行轮次。obs 追加等待、重试、重启接续和确认记录，不以收尾完成证明健康。

   为什么：释放 worker 不等于责任结清；仅重试任务会丢失原错误并重复执行业务。代价：串行失败增加一次快照写入；库故障时继续阻止验证，不新增控制器、表列或调度循环。全部数据库写入都不可用时，未曾保存的原文无法承诺强杀后仍存在；正常停止会报告保存失败。新增 `TestBreakerSettlementRestartAndLostAcknowledgementMySQL` 与 `TestBreakerSettlementCheckpointOutageAndRoundConflictMySQL`，覆盖持续终态故障、丢回执、八路并发结算、两次重启、计数检查点故障、错误只追加一次、补偿条件保留、旧轮次冲突及追加历史。故障原文包含 `fixture terminal storage unavailable`、`fixture checkpoint unavailable`、`fixture count checkpoint unavailable`，来自隔离 MySQL 合成测试，无线上故障或业务库修改。

   复查补充 `TestBreakerSettlementExpiredResultStillReportsConfirmation`：旧失败超过统计窗口后确认，只追加历史，不重算当前失败，也不残留“待落库”文案。全量 go test、vet/build、相关隔离 MySQL race、Node 语法检查及 diff 检查通过。详细范围和持久化边界见 `2026-11-12-breaker-settlement-review.md`；未提交或部署。


9. **2026-11-13：按完整失败结算链路修复前置持久化与统计窗口遗漏。** 用户要求两个已确认问题与同链路问题一并修复。此前复现原文为 `known terminal failure not durable before finish-time retry; restart may rerun business`、`expired failures revived during live settlement: got=4 want=0`。第一处因 runAttempt 先重试 last_finished_at，已知终态仍未写入恢复快照；第二处因长时间结算等待时统计器时钟未推进，使用旧发生时间触发了旧窗口统计。

   现在明确的串行终态失败先保存现有 PendingFailure，再执行完成时间、账号反馈及归档/追踪辅助收尾；用一个任务私有布尔值传递“是否最后一次尝试”，不提前终结可重试错误。取消回退遇到同任务待确认失败则保留 processing；结算前按当前时间推进现有统计器。复查同时移除后台结算重复发送的 task_failed 执行事件，实际结果仍进入事务内追加历史，避免原执行已发事件、检查点尚未保存就中断时把一次失败呈现为两次执行。

   验证从真实 handler 返回开始：TestBreakerSettlementIntentBeforeTimingRetryMySQL、TestBreakerSettlementOldLiveWindow、TestBreakerSettlementCrashCutsFromHandlerMySQL、TestBreakerSettlementOnlyTerminalAttemptsJournalMySQL、TestBreakerSettlementIntentOutageDoesNotRunAuxiliaryWritesMySQL。覆盖完成时间重试时八路并发结算/取消回退/准入，结果保存失败、结果提交丢回执、终态提交丢回执、终态已提交未计数、计数提交前失败、计数提交丢回执及完成后的重启；原业务仅一次、错误追加一次、补偿版本一次、持久历史待确认/确认各一次。沿用原状态、锁、报告循环和终态 CAS，不增加控制器、表列或队列。代价是将已有结果快照写提前；未落盘时强杀的持久化边界未扩大承诺。

   完整复查还补齐“原业务失败已保存，辅助收尾再 panic”：worker 和重启结算共用原 PendingFailure 的终态字段，报告样本同样保留原错误，避免补偿策略或错误原文被收尾异常替换。`TestBreakerSettlementOnlyTerminalAttemptsJournalMySQL/auxiliary-panic` 已覆盖；只有小型字段共用函数，无新增状态层。

   最终全量测试、vet/build、diff 检查及隔离 MySQL 定向 race 全部通过。完整中断点矩阵、原始复现与限制见 `2026-11-13-settlement-chain-audit.md`。未部署、未迁移业务库、未执行 Git 写操作。

### 2026-11-13 Trace 三模式简化

用户取消主动追踪按钮与待选任务状态，统一使用 crawler.trace.mode=off/errors/all，默认 errors。配置包拥有默认值及旧 YAML enabled 映射，Engine 只按模式装配缓冲；取消边界与文件预算不变。代价：未配置的新应用现在默认记录异常，Go 显式关闭须使用 Mode: config.TraceOff；TraceConfig 增加字段，无字段名初始化需要迁移。TraceNextExecution 保留签名但返回不支持错误。定向验证发现旧断言真实报错 `disabled list should report configuration error`，原因是零配置现已默认启用错误模式，测试改为显式 off。回归覆盖 TestTraceModeLoad、TestTraceModeInvalid、TestTraceDefaultAndOff、TestTraceAllRetries、TestTraceActualDeadlineAndAllEmpty。

### 2026-11-13 删除手动 Trace 兼容空壳

用户明确要求删除兼容空壳，因此移除 Engine.TraceNextExecution 及其“不支持”测试断言，当前手册不再承诺保留签名。此前条目中的保留签名方案由本条取代。调用方须删除旧方法调用，统一配置 crawler.trace.mode；保留 TestTraceAllRetries 等实际模式行为回归。本次为主动 API 清理，无故障报错复现。

### 2026-11-13 v4 独立版本：清除旧兼容层

用户要求 v4 不再兼容旧版本。逐模块删除旧API、配置/数据转换、旧适配和无效配置，当前门面/协议保留；完整清单见 [v4清理记录](reviews/2026-11-13-v4-independent-cleanup.md)。代价：旧调用和配置直接编译/校验失败，旧库需独立保留，不能原地兼容启动。检查发现预热 SQL 残留旧列（`SQL missing "`idempotency_key`"`），已移除；旧表非空字段/索引和 NULL 身份类型会破坏新写入，已增加启动与迁移前只读拒绝。回归包括 TestTraceRejectsRemovedEnabled、TestCurrentTaskColumnsRejectBackfilledOldTable、TestTaskIdentityDataRejectsMissingValuesAndPropagatesFailure、TestDecodeRejectsUnsupportedSnapshotFormats、TestBreakerRejectsUnsupportedSnapshotBeforeRestore；取消/终态/恢复使用当前版本协议。浏览器合成端口偶发 `net::ERR_UNSAFE_PORT`，仅测试夹具显式允许自己监听的端口，不改变生产限制。
