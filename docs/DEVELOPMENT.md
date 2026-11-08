# Papa 开发工具手册

> 面向：构建 / 测试 / 竞态检测等日常开发操作。
> 配套：各模块手册见 [README.md](./README.md) 的「阅读路径」。

---

## 0. 一句话

**长期架构定位：单实例大单体。** Papa 永远不规划多实例协同、分布式任务租约或横向扩容；这些不作为缺陷、路线图或生产验收门槛。能力建设聚焦单实例内的模块化、多站点/多阶段并发、资源预算、背压、持久化、进程崩溃恢复与纵向扩容。大单体不改变通用框架与独立业务模块的边界。

本模块现为 `github.com/ydtg1993/papa/v4`。主版本迁移必须同时更新源码 import、CLI 的模块识别/临时校验程序、脚手架及手册；第三方模块和历史验收记录不改。`cmd/papa/module_version_test.go` 校验 v4 模块与模板一致，`module_compat.go` 在 check/upgrade/migrate 前拒绝仍引用 v3 的项目，防止误执行框架迁移。验证须使用 GOWORK=off，另外真实生成合成业务并 tidy/build/check，不能让 go.work 掩盖依赖问题。

远端已有 v4.2.1 标签的模块声明错误，本地修复不等于远端可安装；待用户发布新的正确 v4 标签后，再验收不带 replace 的 go install/new/tidy。版本修复不需要数据库迁移。CLI 不能自动重写旧业务的 imports/require/replace。

用 Makefile 统一日常命令。**必须在 Git Bash（POSIX sh）里跑**——Windows 的 cmd / PowerShell 不认 Makefile 里的 sh 语法（`VAR=1 cmd`、`rm -rf`）。

## 1. 框架仓库（papa 自身）

根目录 `Makefile`：

| 目标 | 实际命令 | 作用 |
| --- | --- | --- |
| `build` | `go build ./...` | 编译校验全部包 |
| `cli` | `go install ./cmd/papa` | 安装 `papa` 脚手架 CLI 到 `$GOPATH/bin` |
| `test` | `go test ./...` | 单元测试 |
| `race` | `CGO_ENABLED=1 go test -race ./...` | 竞态检测 |
| `vet` | `go vet ./...` | 静态检查 |
| `clean` | `rm -rf bin/` | 清理 |

手写 SQL 有三条守卫（都在 `make test` 里跑）：

- **`engine/sqlshape_test.go`**：用 gorm 的 **DryRun** 把生产路径那两条语句拼出来，断言列名带引号 ——
  不连库（坑：要一起关 `SkipDefaultTransaction`，否则 gorm 的默认事务会先去连库）。
- **`engine/sqlreserved_test.go`**：扫全仓**非测试** `.go`，找"裸写的保留字列名"。保留字读
  `engine/testdata/mysql_reserved_words.txt`（导自 MySQL 的 `INFORMATION_SCHEMA.KEYWORDS`），
  列名从 `database.FrameworkModels` 的 gorm schema 反射 —— 两者相交就是要引号的列
  （今天是 `key` / `repeat` / `table` / `values`），加了新列自动覆盖。
  为什么必须有它：`repeat`（v3.1.0 起）与 `key`（v3.2.0 起）都被裸写过，MySQL 直接 Error 1064
  而且**静默**（调用处常常不判那条语句的错）—— 引擎测试跑的是假库、不解析 SQL，
  这类错只能靠扫源码或真库跑一次才现形。详细来历见框架仓库的 `docs/issues/changes.md`（那份**不随脚手架复制到项目里**，别在项目内找它）。
- **`admin/gormsource/sqlquote_test.go`**：后台表格的数据源（筛选 / 全局搜索 / 排序三处都直接往 SQL 里拼列名），
  钉的是"加引号只有 `quote()` 一个出口"，两层一起看 —— **行为层**用 gorm 的 DryRun 把一张表的**整个声明面**
  跑过一遍，断言每个列名只以反引号形态出现、绝不裸写（列名清单从表格声明里取，加一列自动跟上）；
  **结构层**扫本包非测试源码，反引号字面量只许出现在 `quote()` 里（排序那条原先自己拼了一对反引号，
  正是被它拦下才改成调 `quote()` 的）。它与上一条的分工：上一条问"这个名字是不是保留字"，这一条问
  "这个名字有没有加引号"—— 判据不重叠，所以两条都要在。来历见框架仓库的 `docs/issues/changes.md`（同上，不随脚手架复制）。

受控分析模块可单独验证：`go test ./pkg/crawlagent/... ./pkg/agentstore ./pkg/webaccess ./examples/siteagent`。模型测试使用本地模拟服务/回放；真实审计与 runner 集成需要 `PAPA_AGENT_MYSQL_TEST_DSN`，完整示例需要 `PAPA_AGENT_EXAMPLE_TEST_DSN` 和本机 Chromium，均使用专用测试库。缺少依赖的跳过不代表已验证该路径，入口与具体边界见 [SITE_AGENT.md](./SITE_AGENT.md) 和框架仓库的 `examples/siteagent/README.md`。

D0 受控浏览器回归可执行 `go test -race ./pkg/webaccess -run '^TestRealChromium' -count=1 -v`。`render_acceptance_test.go` 覆盖空正文 CDP 兼容、预算与取消后的资源释放；`pkg/crawlagent/runner_render_mysql_test.go` 使用专用 `papa_agent_test` 验证真实渲染失败/取消的持久审计与人工发布边界。Windows 保留系统默认临时目录作为验收基线；本机仅覆盖工作区 GOTMPDIR 也会启动失败，不能用禁用沙箱替代。真实 MySQL 用例缺少 DSN 时会跳过，不能计入通过。

D1 等待回归：`go test -race ./pkg/webaccess -run 'TestRenderWait|TestRealChromiumRenderWait' -count=1 -v`；`go test -race ./pkg/crawlagent -run 'TestRenderWaitActionContract|TestRunnerRenderWait' -count=1` 验证协议、转交、超时修正与审计脱敏。配置专用库后执行 `go test -race ./pkg/crawlagent -run '^TestRunnerMySQLChromiumWaitReopen$' -count=2 -v`，实际验证等待成功/超时、候选验收、重开读取与未自动发布。默认回放示例包含 800ms 延迟 DOM 和显式可见等待，工具调用次数/总预算保持不变。

D2 负样本回归：`go test -race ./pkg/crawlrule ./pkg/rulestore ./pkg/crawlagent -count=1`。真库分别设置 `PAPA_RULE_MYSQL_TEST_DSN`（库名包含 rule_test）与 `PAPA_AGENT_MYSQL_TEST_DSN`（papa_agent_test），验证混合验收、v2 纯正样本历史兼容、报告类型/状态与集合绑定、并发最新凭据和人工发布边界。`TestMySQLNegativeFixturesLegacyReleaseAndRecovery`、`TestRunnerMySQLNegativeFixturesReopenWithoutPublication` 为专项入口；无 DSN 的 skip 不算真库通过。前端沿用 `TestOperationsJavaScriptBehavior` 的 rules/agents 行为测试。

运行质量模块可单独执行 `go test ./pkg/rulequality`，引擎接线执行 `go test ./engine -run 'RuleQuality|RuleFetcher|RuleSnapshot'`。持久化和迁移集成分别使用 `PAPA_QUALITY_MYSQL_TEST_DSN`、`PAPA_QUALITY_MIGRATION_TEST_DSN`，库名必须包含 `quality_test`；没有专用库时跳过不代表真库验证通过。纯报告测试不调用模型，复分析回放也不验证真实供应商质量或账单；入口和边界见 [RULE_QUALITY.md](./RULE_QUALITY.md)。

周期批次执行 `go test ./engine -run 'RuleSchedule|RuleBatch|RunVersion'`。真实 MySQL 用 `PAPA_RULE_SCHEDULE_TEST_DSN`，库名严格为 `papa_rule_test_schedule`；完整两批示例用 `PAPA_RULE_SCHEDULE_EXAMPLE_TEST_DSN`，应指向另一个专用库（推荐 `papa_rule_test_schedule_example`）。DSN 开启 `parseTime=True`，不要让迁移测试与引擎/示例共用数据库。示例走真实 repeat_queue，至少运行 10 秒；同样运行 race。没有 DSN 的跳过结果不能代表重启、行锁、JSON 查询和提交不明路径已验证。

D3 固定评测：`go test -race ./pkg/agenteval ./pkg/crawlrule ./pkg/crawlagent ./examples/siteagent -count=1`。`TestFixedEvaluationMySQLAndChromium` 使用 `PAPA_AGENT_EXAMPLE_TEST_DSN` 的专用 agent_test 库与沙箱 Chromium，实际运行三类各三次、再执行一次完整基线比较，检查审计重开、预算不兼容时零模型调用和无自动发布；无 DSN 跳过不算验收。普通测试验证固定 18 样本、隔离、字段分母、未知用量、比较身份与完整重复次数。正式保存基线使用 `go run ./examples/siteagent -evaluate ...`，见示例 README；不是 Go benchmark，也不默认付费。

D4 回归沿用上述命令。`TestRunnerHistory*` 验证默认不披露、宿主范围授权、最小投影、限额/取消/审计失败、隐藏 URL 和快照冻结；`TestHistoryComparisonAllowsOnlyExplicitTreatment` 防止放宽普通比较。专用库下 `go test -race ./examples/siteagent -run '^TestHistoryEvaluationMySQLAndChromium$' -count=1 -v` 实际跑无/有历史各 9 次，检查审计重开和宿主合成发布指针不变。`TestRecordedHistoryComparisonContract` 校验双组存档；Replay 不能衡量真实模型收益。Windows 若 go run 使用工作区 GOCACHE 中缓存的 exe 导致 Chrome 启动失败，可将相同源码构建到系统默认 TEMP 后运行作对照，不关闭沙箱。

D4 review 补测：`TestHistoryDisclosureChangesReviewedPlan` 与后台 agents 脚本验证披露状态可见且进入审阅快照；`TestRunnerHistoryRejectsEquivalentHiddenURL` 验证等价 URL 隔离；`TestRunnerHistoryUsesOriginalInputAndCostBudgets` 用刚好可容纳无历史输入的额度验证历史确实消耗原预算；`TestRunnerConcurrentHistorySnapshots` 验证同 Agent 的 8 个并发运行隔离。`PAPA_AGENT_MYSQL_TEST_DSN` 配置专用 papa_agent_test 后，`TestRunnerMySQLHistoryAuditRecoveryWithoutReplay` 实际验证 history 写入失败→重开 pending→明确旧执行器停止后 Interrupt 为 unknown→同 RunID 不可重放，Current 不变。

## 2. 业务项目（`papa new` 生成）

生成的 `Makefile` 在框架基础上多几个目标：

| 目标 | 作用 |
| --- | --- |
| `build` | 编译业务二进制到 `bin/<项目名>(.exe)`（自动按 OS 补 `.exe` 后缀；项目名就是 `papa new` 时给的那个） |
| `run` | `go run .` 启动爬虫 |
| `test` / `race` / `vet` | 同框架 |
| `clean` | 清 `bin/`、`logs/*.log`、`downloads/*` |
| `docker-up` / `docker-down` | docker compose 起停 |

## 3. Windows 怎么跑

Makefile 是 POSIX 语法，需要 Git Bash 的 sh。`make` 在 MinGW 下叫 **`mingw32-make`**：

| 环境 | 命令 |
| --- | --- |
| Git Bash（推荐） | `mingw32-make race`（`make` 在 PATH 时可直接 `make race`） |
| PowerShell | 不跑 make，直接 `$env:CGO_ENABLED="1"; go test -race ./...` |
| cmd.exe | `set CGO_ENABLED=1 && go test -race ./...` |

## 4. 竞态检测（race）的前置条件

`race` 依赖 cgo + C 编译器：

- **Windows**：装 MinGW gcc（例如 `C:\ProgramData\mingw64`，确认 `gcc` 在 PATH）。Makefile 的 `race` 已内联 `CGO_ENABLED=1`，不用手动设。
- **Linux / macOS**：装 gcc / clang，直接 `make race`。

## 5. 两个 Windows 坑（已规避）

1. `go build -o bin/xxx` 在 Windows 上**不会自动加 `.exe`**，产物 `bin/xxx` 无扩展名、跑不了。Makefile 用 `$(EXE)` 变量按 OS 补齐。
2. 装 CLI 用 `go install ./cmd/papa`（自动处理 `.exe`），不要用 `go build -o bin/papa`。

## 6. 包结构（改代码前先看这个）

```
papa.go · task.go · errors.go · rulefetcher.go   门面：全是 type/var 别名，没有实现
core/                           零依赖叶子包：跨包流动的纯值 + 不依赖任何内部包的纯函数
                                （告警 / 错误分类 / 监控快照 DTO、ctx 携带的请求头与代理、URL 解析与同站判定、
                                页面身份与任务幂等键 identity.go、站点地址回钉 siteurl.go）
config/                          配置结构与校验（纯数据，不依赖任何内部包）
engine/                          引擎：Task · Trace · Fetcher · Engine
  engine.go    结构体 + 生命周期 + 依赖注入 + 结果读写
  task.go      Task 结构与状态更新（Insert / UpdateStatus / IncrRetry / Unique）
               + 两个 fetcher 接口：Fetcher（GetStage / FetchHandler）与 EntrySubmitter（可选的入口任务）
  stage.go     阶段装配与 worker 循环（ApplyRegisterStage / runAttempt）
  persistence.go 认领/终态写入重试、轮次条件守卫与关停未确认状态统计
  pools.go     活池登记表（懒建 / 空闲回收 / 关停都从这里取池）
  submit.go    提交路径（去重 → 落库 → 入队 → 认领）
  spill.go     高水位溢出与回灌
  pause.go     暂停信号表（唯一真源：scope / reason / source）+ 取任务点的闸门
               （入队点的收口在 submit.go 的 submitToStage；熔断只往这张表里写）
  batch.go     各治理队列共用的分页扫描（keyset 游标）
  dedup.go     内存去重表 + 活跃任务装载
  sitestat.go  站点表与站点维度快照（启动播种、轮询统计回写、熔断状态）
  stats.go     监控快照
  fetch.go     浏览器池 / 静态 HTML 客户端 / 渲染等待与 FetchRenderedPageWith
  record.go    有界网络录制门面（仅摘要 trace）
  http.go      通用 HTTP/API、增量与浏览器响应捕获门面（只记录摘要 trace）
  account.go   显式账号管理器接线、认证 HTTP/HTML/浏览器和无凭据状态快照
  rulefetcher_types.go          规则流程配置、固定版本引用和保存结果契约
  rulefetcher.go                通用 Fetcher、校验后保存、详情与分页、受限子任务规划
  rulefetcher_tasks.go          固定版本/账号、请求身份、持久化分页检查点与来源范围
  rulefetcher_source.go         HTTP/账号/Rod 来源适配、失败分类与安全错误
  rulefetcher_catalog.go        持久 Catalog 版本解析、完整绑定检查与固定发布身份
  ruleschedule.go              完整周期批次控制 Fetcher、构造与状态 API
  ruleschedule_payload.go      静态控制身份、固定批次入口、版本化载荷与摘要校验
  ruleschedule_store.go        MySQL 控制行锁/CAS、先持久后投递、真实成员完成判定
  ruleschedule*_test.go        周期状态、并发、重启与未知提交；rulefetcher_batch_test.go 验证跨阶段传播
  repeat_submission_test.go   公共提交入口独立对象并发，同一 pending 轮次保持计数
  repeat_submission_mysql_test.go 专用 MySQL 库：并发、首次初始化、恢复、限额、提交结果不确定
  stage_runversion_test.go     普通 worker 认领版本传递、重试固定和旧轮次终态拒绝
  rulequality.go               可选执行质量旁路、原始提取摘要与终止环节分类；rulequality_test.go 覆盖故障不重放业务
  rule_snapshot_types.go        规则页面观察清单：规则/尝试、双摘要、正文引用与省略原因
  rule_snapshot.go              尝试登记、显式脱敏、预算、EvidenceID 与最小 Trace 引用
  rule_snapshot_store.go        脱敏正文按内容共用、原子发布与按存活清单引用回收
  rule_snapshot_store_windows_test.go  固定重命名句柄窗口，验证 Windows 共享模式及完整/损坏/恢复读取
  capture.go   浏览器网络请求捕获的引擎层包装（trace 步骤 + 归档登记）
  downloads.go 下载器解析（阶段 > 站点 > 基座）
  archive.go   页面归档：失败那一刻的原始页面 + trace 步骤
  restricted.go 受限页判定（站点词表从 task.Site 取）
  deps.go      阶段依赖声明（NeedsBrowser 的启动期校验）
  queuestats.go 三条治理队列的运行快照与积压（全局各一条）
  dynticker.go 间隔可动态调整的定时器（错误/轮询队列的节拍）
  alias.go     给 core 里的值类型留的别名（见文末）
  recoverqueue.go · errorqueue.go · repeatpoll.go · delayqueue.go · taskadmin.go · trace.go
  taskadmin_rule.go            持久任务角色与批次手工操作保护；失败成员 UPDATE 状态守卫
  ruleschedule_admin.go        控制任务游标列表、当前批次一致快照、固定规则与成员分页
  ruleschedule_admin_mysql_test.go  真实 MySQL 生命周期/分页/读写并发与并发重试
  taskadmin_rule_test.go       角色边界、SQL 原子条件与误开轮询的修复路径
admin/                          ★后台模块：**对外公开**，业务项目可以直接 import 复用
  server/       监控后台（HTML / API / 静态资源）
    ruleschedule.go / ruleschedule_test.go  注入式批次只读 API、认证/参数/错误映射
    quality.go / quality_test.go  质量查询、范围/精度/认证/错误映射
    quality_write.go / agents.go / agents_test.go  具名固化/运行控制、严格 DTO 与操作审计
    ui_behavior_test.go        Node 执行生产 JS 与依赖真实 renderer 的行为回归入口
    static/requests.js         原生 API 错误、轮询代次/超时/退避/可见性管理
    static/table-links.js      oao v1.3.4 整值 URL 兼容适配（table-loaded 扩展点）
    static/schedules.js / schedules.css  周期批次页与窄屏导航样式
    static/quality.js / quality-write.js / quality.css / testdata/quality_test.js  查询、固化与证据
    static/agents.js / agents.css / testdata/agents_test.js  分析预览、运行、候选、会话恢复与竞态
    testdata/monitor_requests_test.js / table_links_test.js / schedules_test.js
  auth/         访问令牌校验与操作人身份
  tokenadmin/   令牌 CRUD 接口
  oplog/        操作日志（异步写入器 + 只读表格页）
  tasksource/   内置「任务」表，动作接线的参考实现
    rule_state.go / rule_state_test.go  数据源派生角色，不下发 Meta；动作展示与脱敏回归
  gormsource/   配置驱动的 oao.Source，业务表格页直接复用
  frameworkdata/ 按显式选表生成只读查询；后台不默认挂载通用目录，不扫描业务表
    tables.go / source.go  模型列、受限 SQL 投影、联合主键稳定分页、查询超时
    sanitize.go / status.go  有界脱敏、鉴权状态探测（未迁移/不可用/可查询）
    tables_test.go / mysql_test.go  HTTP/SQL/并发回归与显式只读 MySQL 验证
  configstore/  运行期配置存储（crawler_config 表）+ 读缓存层（内存 / TTL 装饰器）
  sysinfo/      机器指标 / 目录占用采集
internal/                       真正的内部件：不对外承诺 API，**模块外也 import 不到**
  selectorbudget/css.go         Agent 规则与受控渲染等待共用的保守 CSS 校验；旧规则契约保持不变
  workerpool/ database/ msgqueue/ track/ breaker/
  app/                          装配层：把上面这些接成 App（门面 papa.App 指向它）
    data_tables.go / data_tables_test.go  只接入可操作任务，Trace 走行内弹窗；保留 task API 和动作保护
    quality_admin.go            quality.enabled 接通查询与显式固化，不安装 Collector
    agent_admin.go              App.UseAgentCenter 接线、只读默认值与停机等待
pkg/                             可独立复用的件
  events/                        结构化运行事件：契约（event.go）+ 异步 emitter + JSONL sink
  browser/                       池与页面生命周期（pool.go / rod.go）、网络请求捕获（capture.go）、
                                 等待条件（wait.go）、按页资源拦截（block.go）
  browser/record.go              多交换网络录制、预算与显式脱敏导出
  browser/state.go               同源隔离页面登录态快照、AES-GCM 加密读写与恢复
  browser/state_navigation.go    首次导航前 Cookie/LocalStorage 恢复与 origin 检查
  browser/isolation.go           回调式独立 Chromium 上下文、取消与异常清理
  browser/response.go            按响应匹配并等待正文；session.go 提供浏览器 Cookie 到 HTTP 会话的显式桥接
  htmlfetch/                     client.go HTML；request.go 通用请求；json.go 严格 JSON/内嵌 JSON；session.go 独立 CookieJar；session_guard.go 每账号检查/刷新并发协调；sample.go 显式脱敏样本与离线响应
  discovery/                    links.go URL/HTML；xml.go 订阅源；robots.go 规则；pagination.go 分页停止
  extract/validate.go            JSON 字段与批次唯一性校验
  htmlfetch/incremental.go       条件 GET、304 与内容指纹
  htmlfetch/session_state.go     完整 Cookie 属性追踪、版本化快照与受限恢复
  account/                      types.go 契约；manager.go 注册/认证/请求/版本；browser.go 账号浏览器与桥接；store.go 加密存储
  crawlrule/                    rule.go 规则编译；evaluate.go HTML/JSON/DOM 与受限证据；transform.go 确定性转换；fixture.go 样本/根级游标验收；registry.go 激活、Verified 与回滚；catalog.go 持久发布读取契约
  crawlrule/fixture_contract.go / fixture_negative_test.go 显式正/负预期、严格分类、整组正样本门槛与非法输入/资源边界回归
  crawlrule/field_match.go / field_match_test.go 诊断字段精确匹配计数；missing/null、JSON Pointer 和大整数边界
  agenteval/                    score.go 固定分母评分；report.go 审计指标、未知用量、完整性与严格基线比较；score_test.go 失败/部分匹配/不可比报告回归
  agenteval/history.go / history_test.go 仅显式历史处理差异的双组比较、合法摘要与处理记录校验
  crawlagent/history.go / runner_history_test.go 已发布引用的宿主披露批准、投影/限额、冻结与安全审计
  rulestore/negative_contract_test.go / negative_mysql_test.go 旧 JSON/报告兼容、负样本报告完整性、并发验收、真库重开与人工发布/回滚
  crawlagent/runner_negative_test.go / runner_negative_mysql_test.go 混合样本训练/隐藏集隔离、模型前置校验、持久审计与不自动发布
  rulestore/                    types.go 数据契约；repository.go 不可变规则与固定版本解析；repository_samples.go 样本/集合/验收；repository_release.go 完整站点发布、CAS、幂等与回滚
  rulestore/blobs.go             长期正文按内容保存、原子发布、完整性校验；sample.go 显式脱敏；snapshot.go 选定快照导入；compare.go 结构对比与离线影子评估
  rulestore/review_*_test.go     constructor 外层事务；release 并发/提交故障；rollback 历史凭据；verification 样本/验收一致性真库回归
  crawlagent/                   types.go/config.go 宿主契约；run.go/action.go/tools.go 有限工具循环；evidence.go/url.go/selector_budget.go 证据隔离与预算；audit.go 审计引用
  crawlagent/*_test.go           回放修正、权限/预算/隐藏样本隔离、审计失败与真实 MySQL runner 重开回归
  crawlagent/runner_render_mysql_test.go 真实 Chromium 超限/取消、MySQL 审计收尾和发布不变
  crawlagent/action_wait_test.go / runner_wait_test.go 等待严格参数、转交、默认兼容、超时修正、权限与审计脱敏
  crawlagent/runner_wait_mysql_test.go 真实 Chromium 延迟 DOM/超时、MySQL 重开、候选验收和人工发布边界
  webaccess/render_acceptance_test.go 空正文响应、实际渲染预算、排队/在途取消与并发名额回收
  webaccess/render_wait.go / render_wait_test.go 有界 CSS 等待、隔离执行、页面关闭、取消/期限/网络预算、排队回收与快照主动取消在途资源
  crawlagent/run_identity_test.go 宿主 RunWithID 身份、额外证据与隐藏样本隔离回归
  crawlagent/model/             model.go Provider 契约；compatible.go 有界兼容 HTTP 客户端；replay.go 确定性回放；对应测试不调用真实模型
  agentadmin/                   types.go/service.go/execution.go/review.go：宿主配置、幂等执行、具名控制、固定候选审阅；review_test.go
  crawlagent/plan.go             生效预算/披露种子的不可变预览与操作者绑定
  agentstore/admin.go / admin_memory.go / admin_test.go  有界运行/步骤快照分页、SQL 与真库重开
  rulequality/report_admin.go / admin_memory.go  报告固化、操作查询和请求分页
  rulequality/reanalysis_preview.go / agent_admin_test.go  显式预览与停止核对、跨中心 CAS/取消/恢复回归
  agentstore/                   types.go/state.go 运行/步骤与 revision；memory.go 内存实现；mysql.go 事务持久化；store_test.go/mysql_test.go 审计一致性与重开验证
  rulequality/                  types.go/validation.go 摘要、窗口、策略与状态契约；summary.go 无值字段统计；collector.go 有界旁路与失败计数；aggregate.go 固定窗口报告
  rulequality/{aggregate,collector,summary}_test.go 字段分母、重试折叠、基线兼容、缺失覆盖及旁路超时/panic/race 回归
  rulequality/memory.go          内存 Store；mysql.go 不可变观测/报告、请求 CAS 与显式有界清理；对应 *_test.go 验证重开、并发与未知提交
  rulequality/admin.go / admin_test.go  后台候选分页、范围汇总、报告关联与真库完整性回归
  rulequality/reanalysis.go      宿主显式复分析、选定快照导入与固定 Agent RunID；reanalysis_test.go 验证幂等、证据和授权边界
  webaccess/                    policy.go/session.go 范围与预算；http.go 受控 GET/robots/重定向；render.go 独立 Chromium 出口；*_test.go 拒绝边界回归
  loggers/ notify/ middleware/{proxy,m3u8,filedown}
  redact/                       诊断副本 URL/Text/JSON 已知凭据遮盖（不改请求、任务身份与恢复数据）
  middleware/output_locks.go    下载目标/断点资源的整组路径锁（目录与子路径互斥、可取消、With 共享）
obs/                             运行报告引擎（通用、只读）：obs.go 契约 / load.go 三层装载 /
                                 eventlog/ 事件读取 / logidx/ 日志证据 / analyze/ 通用分析节 /
                                 pattern/ 内置模式库 / report/ 4 产物渲染
models/                          框架自带的表结构
models/rule_publication.go       可选规则/样本/集合/验收/发布/站点指针 6 表，按 rules.enabled 纳入显式迁移
models/rule_schedule.go          可选完整周期批次状态，第 7 张 rules.enabled 表；不改变任务列
internal/database/rule_schedule_models_test.go 周期表主键/类型/注释与可选注册；rules_migrate_mysql_test.go 覆盖旧规则库补表
models/agent.go                  可选运行/步骤审计 2 表，按 agent.enabled 纳入显式迁移
models/rule_quality.go           可选观测/报告/复分析请求 3 表，独立按 quality.enabled 纳入显式迁移
internal/database/quality_migrate_mysql_test.go 质量开关独立性、普通启动不建表与重复显式迁移的真库回归
internal/database/quality_models_test.go 三张可选模型与任务表字段边界守卫；config/quality_test.go 独立开关校验
engine/review_catalog_test.go     动态规则与宿主兼容、持久任务恢复、永久/临时错误分类回归
examples/rulecrawl/              main.go 本地 HTML / JSON POST 游标完整流程；main_test.go 示例验收；README.md 生产接线
examples/rulepublish/            main.go 专用 MySQL＋本机 HTTP 的验收/发布/恢复/影子/回滚示例；main_test.go 入口约束验证
examples/siteagent/              main.go 本地静态/DOM/GET JSON 分析与审计重开；main_test.go MySQL＋Chromium 回放验收；README.md 真实模型显式接线
examples/siteagent/evaluation_{fixtures,run}.go 固定 18 合成样本、9 次受控评测、显式付费预算与报告比较；evaluation_test.go 样本协议/隔离、真库浏览器和无发布验证
examples/siteagent/evaluation_history.go / evaluation_history_test.go 宿主合成已发布种子、显式双组对照、真库/Chromium 和存档契约
examples/rulequality/            本地网站质量场景、固定报告与显式复分析回放；入口见 RULE_QUALITY.md
cmd/monitor-demo/quality_demo.go / quality_demo_test.go 模拟观测、真实聚合演示及 Trace
cmd/monitor-demo/agent_demo.go    不接模型或站点的运行审阅 UI 模拟
examples/ruleperiodic/           真实 Engine/MySQL 两批列表、详情、分页及幂等业务更新
cmd/papa/                        CLI（new / check / upgrade / migrate / token / html / rod / diff / select / report）
  scaffold_safe.go               new 独占写入与链接路径保护；--missing-only 只补缺失文件
  scaffold_safe_test.go          已有项目保护、module 冲突、链接路径与缺失文件回归
  scaffold_review_test.go        Windows junction 越界、并发独占写、带空格 replace 与缺失参数回归
```

> **`admin/` 与 `internal/` 的区别就一条**：Go 规定 `internal/` 下的包只能被 `internal/` 的
> 父目录（也就是本模块）内的代码 import。所以业务项目**能** import `admin/gormsource`，
> **不能** import `internal/workerpool` —— 后者是框架自己的实现细节，改了不算破坏性变更。

**框架与业务边界**：真实网站的目标 URL、选择器/预期、脱敏策略、数据库/模型接线及外站验收均放在独立业务模块，通过 Papa 公开 API 接入。框架只保留通用库、通用修复/回归和本地合成 fixture 示例；业务模块不得导入 Papa 的 internal 包，也不得成为 Papa 的反向依赖。

**依赖方向**（箭头 = import 方向，不允许反向）：

```
papa 门面 ──► internal/app ──► engine ──► internal/{workerpool,database,track,breaker}
                 │               │              pkg/{browser,htmlfetch,loggers,middleware}
                 │               └──► core ◄── config
                 └──► admin/{server,auth,tokenadmin,oplog,tasksource,sysinfo,...} ──► core
                         └──► tasksource/oplog 还依赖 admin/gormsource
pkg/notify ──► core
pkg/rulestore ──► pkg/crawlrule + pkg/htmlfetch + models + GORM（不依赖 engine；通过 Catalog 接入）
pkg/crawlagent ──► pkg/{crawlagent/model,webaccess,agentstore,crawlrule,htmlfetch,rulestore}（不依赖 engine；无生产发布/业务保存接口）
pkg/agentadmin ──► pkg/{rulequality,crawlagent,agentstore,rulestore}（不持有发布能力）
pkg/agentstore ──► models + GORM（构造不迁移，不自动接管运行）
pkg/rulequality ──► pkg/{crawlrule,crawlagent,agentstore,rulestore,htmlfetch} + models + GORM（不依赖 engine；采集旁路不调用模型）
obs ──► pkg/events + models（只读；不依赖 engine / admin / app，业务项目可直接 import 扩展）
```

三条约束值得记住：

1. **`core` 只能 import 标准库。** 它被 `admin/server`、`pkg/notify` 这些叶子用；一旦它
   import 了 gorm / models，这两处就会被连带着拖上 —— 那正是抽它出来要避免的。
2. **值类型要跨包就走 `core`，引擎自己的类型留在 `engine`。** `Task` / `Trace` / `Fetcher`
   与引擎强耦合（`Fetcher` 的签名里就带 `*Engine`），搬过去零收益，还得多拖 gorm 和 logrus。
3. **放进 `admin/` 就等于承诺 API。** 只想自己用的实现放 `internal/`；哪天决定给业务用，
   再挪过去 —— 挪的那次是**新增**（原来外部也 import 不到），不是破坏性变更。

`engine/alias.go` 给 core 里的值类型留了别名（`engine.StageStats` ≡ `core.StageStats`，
是**同一个类型**而不是可互转的两个）。它的来历是"抽 core 时不破坏 `crawler.StageStats`"——
而 `crawler` → `engine` 这次改名本身已经是破坏性的，所以对**模块外**来说这个垫子基本失去意义；
留着是因为模块内（`internal/app` 等）还在用，新代码请直接写 `core`。

---

## 7. 站点维度：哪些已按站点切、哪些还是全局

多站点项目里最常问的一句是"这个配置/这个状态是按站点的，还是全局的？"。下面按**层**列清楚 ——
改东西之前对着它看一眼，省得把一个本就是全局的东西硬塞进 `SiteSpec`（或者反过来以为某处已经分站了）。

**两条硬规则**（阶段名按站点作用域那一版定下的）：**站点声明的 `Key` 必填**（阶段键是 `site:stage`，
没有 Key 就分不开，启动即报错）；**任务的 `Site` 必填**（提交时就得带对，否则那个阶段查不到）。
**没有「默认 scope」那一档** —— 两处都拒空。旧库里 `site` 为空的行成了孤儿：它们拼出的键
（`:catalog`）查不到任何已声明的阶段，三条治理队列会跳过后打 WARN，要救就自己把 `site` 补上。

### 已按站点切

可选 `pkg/account.Manager` 进一步按站点＋账号管理登录态、请求预算和暂停。它通过显式 Engine 账号入口接入，不改变站点队列暂停规则，也不向框架任务表增加凭据列。详见 [ACCOUNT_RUNTIME.md](./ACCOUNT_RUNTIME.md)。

| 层 | 是什么 | 在哪儿 |
| --- | --- | --- |
| 声明 | `Key` / `BaseURL` / `Headers` / `RestrictedKeywords` / `StartPaused` / `Breaker` / `M3U8` / `FileDown` / `Stages` | `internal/app/SiteSpec` |
| 执行 | **每站每阶段一个 workerpool**（阶段键是 `core.StageKey(site, stage)`：阶段名只在本站内唯一，两个站的 `catalog` 是两个池子） | `engine.stage.go` 建池那一段 |
| 执行 | **每站一个熔断计数器**（没单配阈值的站也用自己那份，阈值取 `crawler.breaker` 那份默认）；它只判据，不再持有暂停状态 | `internal/app/spec.go` 的 `defaultBreakerConfig`、`engine.breakerFor` |
| 执行 | **暂停是一处信号**（`engine/pause.go`，按 scope：整站 / 某站某阶段）：写它的人有三个 —— 后台/业务手动、熔断触发、声明期的 `SiteSpec.StartPaused`（`applyStartPaused`，启动那一刻下一道整站闸）；两个消费者 —— 取任务点的闸门（冻住已排队的）+ 入队点的寄放（不接新活，寄放到溢出表） | `engine/pause.go`、`engine/stage.go` 的 `ApplyRegisterStage`、`engine/submit.go` 的 `submitToStage`、`engine/spill.go` 的 `drainSpilled` |
| 执行 | 告警事件带 `site` | `core.TaskError.Site`（`core.AlertEvent` 内嵌它） |
| 统计 | 阶段快照的键是 `site:stage`，条目里另带裸阶段名 `Stage` 与 `Site`（后台按 `site` 分组、标题用 `stage`）；熔断状态带 `Site` | `core.StageStats` / `core.BreakerStatus` |
| 统计 | 站点快照（**内存是源**：启动读 `crawler_config` 的 `sites` 键（没有就按声明生成）、按声明对齐；`StartPaused` 也在这**一次**写里落成暂停态，手动暂停/恢复整块回写） | `engine/sitestat.go`、`admin/configstore/sites.go` |
| 后台 | 顶部站点 Tab、站点概要；任务表有 `site` 列与筛选 | `admin/server/static/mo.js`、`admin/tasksource/table.go` |
| 数据 | 任务的 `site` 列（提交时**必填**，重投路径照抄回来）；唯一索引 `(url_hash, stage, site)` | `models/crawler_task.go`、`engine.submit.go` |
| 落盘 | 原始 HTML 归档按 `{dir}/{site}/{stage}/` 分两层；RuleFetcher 观察清单记录 `site/stage`，同一归档根下的脱敏正文按内容共用 | `engine/archive.go` 的 `relPath`；`engine/rule_snapshot*.go` |
| 发布 | 一个站点的完整 stage→规则版本表在事务中切换，generation 防止并发覆盖；任务固定整组绑定及发布身份 | `pkg/rulestore/repository_release.go`、`engine/rulefetcher_catalog.go` |
| 周期批次 | 每个控制任务独立持久化批次、规则快照和完成门槛，批次传播到该站点所有派生规则任务 | `engine/ruleschedule*.go`、`models/rule_schedule.go` |
| 质量 | 报告固定站点、阶段、规则版本与 release 的观测窗口；只覆盖已记录数据，重试不放大逻辑页面数 | `pkg/rulequality/aggregate.go`、`engine/rulequality.go` |

### 全局，且**有意**如此（别想着分站）

- **进程级**：`crawler.stop_timeout`（停机怎么等）、`crawler.drain_interval`（溢出回灌跳多久一次）、
  `server.*`（HTTP 服务/白名单/采样间隔）、`db.*`、`log.*` —— 一个进程只有一份。
- **客户端级**：`html.*` / `browser.*`（超时、体积上限、空闲回收、全局 headers）。站点要自己的头就用
  `SiteSpec.Headers`（同键覆盖全局那层）；单次请求还能 `papa.WithHeaders`。
- **代理出口**：框架只提供 `engine.NextProxy()` 这个"随用随取"，**不替业务决定**用哪个 ——
  按站点分流出口是业务自己的事（`papa.WithProxyURL`）。
- **操作日志/访问令牌**：与站点无关（前者记的是后台动作，后者是后台的凭据）。
- **规则仓库与样本目录**：`rules.enabled/sample_dir` 是应用级配置，规则 ID/版本与样本标识在仓库内唯一；正文按内容共用且不自动 GC。发布指针按站点切分，短期 archive 仍使用独立目录和保留期。
- **受控分析**：`agent.enabled` 是应用级审计表开关；Goal 的 Site/Stages、允许 origin/路径和预算由宿主逐次提供，使用独立访问客户端和浏览器。分析 run 不参与引擎三队列，也不自动发布或提交业务任务；完整契约见 [SITE_AGENT.md](./SITE_AGENT.md)。
- **质量反馈**：`quality.enabled` 是独立的应用级三表开关，不要求开启 rules/agent。宿主显式构造 Collector、生成固定 Report，随后才可选择创建并执行复分析；旁路失败不改变任务终态。Collector 失败计数仅覆盖本实例生命周期，不能证明进程重启前或其它入口的完整覆盖，见 [RULE_QUALITY.md](./RULE_QUALITY.md)。

### 按站点切的进度：三队列已**并回全局**；`crawler` 那四项**有意**保持全局

- **`error_queue` / `recover_queue` / `repeat_queue` 现在是全局各一条**（不再按站点拆）：
  各一段写在 `config.yaml` 里，后台各占一行。当初按站点拆没有收益（三队列本质都是对
  `crawler_tasks` 表的一次复查），反而让"一个阶段占一个协程空等"的多站开销被放大 —— 已并回全局。
- **`crawler` 的四项有意留在 `config.yaml`（定过的，不是待办）**：`queue_watermark`（`engine/stage.go`
  建池那行）、`archive.*`（原始 HTML 按 `{site}/{stage}/` 分层，规则快照在 `.rule-snapshots/` 下按观察和正文组织）、`trace.*`
  （`engine/trace.go`，步骤记录本来就带站点）、`dedup_cache_size`（`engine.NewEngine`）。
  它们是**进程级资源**那类数值（水位、去重预算、保留期、归档总闸），调的是"这个进程扛不扛得住"，
  而不是"这个站要怎么抓"；目前没有逐站调的真实需求，按站点拆等于把一句话摊成 N 份。
- RuleFetcher 的 `SnapshotPolicy` 是业务侧的响应脱敏策略，不增加站点或 YAML 配置。`RulePage.EvidenceID` 只是 Save 可自行保存的关联值，不增加框架数据库列。快照清单和 Trace 共同引用正文文件，原始摘要与脱敏摘要分开记录；存储发布和引用回收只在同进程协调，多个进程不能共用一个规则快照目录。契约与预算见 [RULE_FETCHER.md](./RULE_FETCHER.md)。
- **`dedupCache` 是"按 key 分、预算共享"**：key 是 `site:stage:url`（`Task.Unique()`；缺 site 时以空段出现），但只有**一个**
  LRU、一份 `dedup_cache_size`。某站刷爆缓存只会让别的站多几次 SELECT（DB 唯一索引兜底），
  不影响正确性 —— 想每站一份预算就是"去重缓存按站点"那一项。
- ~~**任务表的预置筛选**~~：oao 已经加上了（`render`/`open` 的 `opts.filter`，见 oao README 的
  「宿主预置筛选」），papa 侧也已接上（切站点 tab 打开任务表会带 `site=<该站>`）——
  **前提是 oao ≥ v1.3.0**（papa 的 go.mod 已经升上去了），老版本会静默忽略第三个参数。

### 新增东西时的判断规则

一句话：**跟"某个站点的抓取"有关的按站点，跟"进程/服务本身"有关的不按站点。**
并发、间隔、重试、请求头、出口这些按站点（阶段参数 + 站点声明上那几项）；
**队列水位 / 归档 / trace / 去重预算目前有意留在全局**，停机怎么等、HTTP 服务开不开、日志写哪儿也一样
—— 一个进程只有一份答案。要动这条边界，先问一句：这是"**这一站怎么抓**"，还是"**这个进程扛不扛得住**"。


## 业务脚手架目录（新项目）

`papa new` 生成薄根入口 `main.go` 与 `internal/app/run.go`，采集实现、站点声明、模型和后台分别在 `internal/{fetcher,sites,models,monitor}`。Docker 文件放 `deploy/docker`，应用文档与 `docs/framework` 框架副本分开；完整生成树见 [CLI.md](CLI.md)。

CLI 实现中 `cmd/papa/layout.go` 统一识别站点和文档布局，供 check、upgrade 与随项目技能共用。旧项目仍使用原目录，不会被升级命令自动迁移。模板 `app_run.go.tmpl` 负责启动组装，`main.go.tmpl` 只转交参数。

脚手架区分完整 module 与本地项目名：module 用于 import，本地项目名取最后一段，用于站点文件和构建/部署名称。`cmd/papa/module_path_test.go` 的 `TestRunNewNamespacedModuleBuilds` 覆盖多级路径、连字符和 `/v2` 后缀，实际生成临时项目、tidy/build、构建入口二进制并运行纯站点校验。

轮询回灌计数回归在 `engine/spill_repeat_test.go` 的 `TestSpillRetriesPreservePendingRepeat`：覆盖连续满池、首次写库失败、容量释放后投递和下一轮计数；SQL 方言与保留字仍由 `engine/sqlshape_test.go` 的 MySQL DryRun 验证。

真实计数回归：配置 `PAPA_REPEAT_TEST_DSN` 后执行 `go test -race ./engine -run '^TestRepeatSubmissionMySQL' -count=1`。测试强制数据库名为 `papa_repeat_test`，需要预先建库并授予迁移表权限；自动迁移仅该库的任务表、按本次随机站点清理测试行，不接受业务库 DSN。覆盖 MySQL changed-rows/found-rows、首次并发、已有轮次重投、pending/processing 重启恢复、下一轮/上限，以及 UPDATE 已提交后注入错误的恢复。未配置时明确跳过。普通测试另用 MySQL DryRun 钉住初始化 CASE 与轮询 reset 的 SQL，fake 不代替真实数据库语义。

批内重复提交回归在 `engine/submit_batch_dedup_test.go`：覆盖同 URL、相同幂等键、轮询任务、重复指针、混合批次首次参数保留、已有状态不重置、满池寄放及延迟投递。外部唯一冲突回查沿用 `TestSubmitTasksSkipsConflictingRows`。

批量部分成功回归在 `engine/submit_partial_test.go`：覆盖首/中/尾行失败、多行与全批失败、成功项不重复投递、失败项可重试、延迟项保持等待，以及落库后读取失败仍保留投递责任。

后台报告切换回归在 `admin/server/obsreport_race_test.go` 的 `TestObsReportLatestRequestWins`：有 Node.js 时执行 `testdata/obs_report_race_test.js`，以受控响应顺序运行真实的报告加载函数，覆盖响应头/正文晚到、旧请求失败、文件切换、A→B→A 和空报告。没有 Node.js 的环境跳过该行为测试；可直接运行 `node admin/server/testdata/obs_report_race_test.js admin/server/static/mo.js`。

报告运行筛选回归在 `obs/load_run_scope_test.go` 的 `TestLoadSourcesGapCountFollowsRunAndWindow`：从临时 JSONL 与 gzip 轮转文件加载交错、多运行、重复及乱序事件，验证单次运行的缺口和 `event_gaps` 检查、全量汇总及时间窗口边界；文件读取诊断仍保留原范围。

验证包含 `TestScaffoldBuildsAndUpgradePreservesAppDocs`（生成真实项目并 tidy/build）、`TestCheckEndToEnd`（新站点目录校验及非法声明拒绝）、`TestLayoutDetectionAndLegacyUpgrade`（旧布局与优先级）及 `TestUpgradeWriteOnlyTouchesFrameworkCopies`（升级只写框架副本）。

浏览器临时目录：`pkg/browser/temp_dir.go` 统一解析、写入验证和独立子目录创建；`pool.go` 保存实际根目录。配置入口为 `config.BrowserConfig.TempDir`，引擎与调试 CLI 均传入池。


OA 行为回归 `TestOperationsJavaScriptBehavior` 调用 Node 行为脚本，直接执行嵌入资源与当前 oao 依赖的 renderer/request；没有 Node 时显式跳过。覆盖监控响应/正文乱序、401/超时/隐藏页、退避、队列重复点击、整值 URL 协议与参数编码，以及批次页状态切换和动作确认。复核用例还包含原生正文停滞、oao 旧登录 401 与验证合并且不重放写入、Trace/菜单乱序、登录恢复、弹窗替换取消与键盘焦点、成员误开轮询和大 ID 展示。`TestRuleScheduleDetailJSONPreservesIntegers` 验证 JSON 精度；`TestRuleBatchAdminMySQLRetryChecksLatestState` 在真实 MySQL 中将竞争更新插入读与 CAS 之间。`cmd/monitor-demo/schedule_demo.go` 提供显式模拟批次、停/开轮询与成员重试数据，独立于数据库和生产任务。

运营首页位于 `admin/server/static/workbench.js` / `workbench.css`；`buildWorkbench` 只派生当前监控快照，`renderWorkbench` 与 `setWorkbenchConnection` 分别接到现有渲染及连接状态。`workbench_test.js` 覆盖统计口径、站点并集、缺失数据、采样时间、XSS、导航作用域、同名按钮焦点、折叠状态和 `__all__` 站点名；`TestWorkbenchTemplateKeepsOperationsAndDetails` 验证模板及嵌入资源接线；`TestDemoTaskSiteFilter` 让演示数据能真实验证本站任务跳转。此路径不增加 HTTP 请求或数据库读取。

规则运维文件分工：`pkg/rulestore/admin.go` 负责元数据分页/一致快照/预览，`repository_release.go` 的 `PublishReviewed` 在原事务锁内核对审阅依据；`admin/server/rules.go` 负责可选接口、鉴权、严格正文、期限和操作审计；`internal/app/rule_admin.go` 根据既有 rules.enabled 接线，不打开样本文件。`static/rules.js/css` 与模板提供审阅、完整清单、发布/回滚及会话存储中的原操作 ID 恢复。`cmd/monitor-demo/rule_demo.go` 为完全模拟的浏览器验证入口。

质量中心文件分工：`pkg/rulequality/admin.go` 复用持久化完整性检查、精确 Scope 聚合及只读分页；`admin/server/quality.go` 提供可选接口、5 秒期限、严格参数与大整数字符串输出；`internal/app/quality_admin.go` 按既有 quality.enabled 接线。`static/quality.js/css` 处理筛选失效、详情、临时汇总、报告引用与只读 Trace 入口。模拟数据在 `cmd/monitor-demo/quality_demo.go`，使用真实 Aggregate 生成演示报告，不连接外部服务。

新增回归：`TestQualityAdminSQLKeysetsAreBounded` 验证生产查询参数与 LIMIT；`TestQualityAdminMySQLCutoffFilteringRecoveryAndEvidence`、`TestQualityAdminMySQLReportsAndCorruption` 验证真实 MySQL 翻页/重开/引用/清理/损坏；`TestQualityHTTPGuardsPrecisionAndErrors` 验证认证、方法、范围、期限和脱敏；`TestOperationsJavaScriptBehavior/quality_test.js` 与 `monitor_requests_test.js` 验证乱序、迟到正文、身份/筛选切换、精度、转义和 Trace 取消。`PAPA_QUALITY_MYSQL_TEST_DSN` 必须指向专用 `papa_quality_test`，缺失则显式跳过，不能称真库通过。

新增 `TestAdminPlanAndSQLBounds`、`TestAdminMySQLReviewCASRecoveryAndPagination`、`TestAdminMySQLSnapshotAndUnknownCommit`、`TestAdminMySQLFailedReverifyRemainsVisible` 验证 SQL 形状、分页、旧审阅拒绝、并发唯一提交、重开/未知提交和验收记录；`TestRuleAdminHTTPGuardsAuditAndPrecision` 验证 HTTP 边界；`rules_test.js` 执行生产前端，覆盖旧请求/确认、存储失败、重复点击、凭据切换、恢复与安全随机 ID 的兼容路径。`TestDemoRuleOperationsAndIsolation` 验证模拟仓库的幂等与返回值隔离。真实仓库测试依然显式要求专用 `PAPA_RULE_MYSQL_TEST_DSN`；不能用 demo 的内存状态代替数据库一致性证据。


OA Agent 中心定向验证：`go test ./pkg/agentadmin ./pkg/agentstore ./pkg/rulequality ./admin/server ./internal/app ./cmd/monitor-demo ./examples/rulequality`，并发路径加 `-race`。报告固化与分页真库沿用 PAPA_QUALITY_MYSQL_TEST_DSN / PAPA_AGENT_MYSQL_TEST_DSN；完整中心候选回放沿用 PAPA_QUALITY_EXAMPLE_TEST_DSN。前端生产 JS 的 Node 回归由 ui_behavior_test.go 自动运行（脚本清单以测试入口为准）。App 退出先排空后台 Agent 再关闭 DB；无法排空时保留依赖，不把超时视为 worker 已停止。


后台通用扩展：`admin/server/static/extensions.js` 管理 Papa.page/settings、离页保护与销毁；`testdata/extensions_test.js` 执行真实脚本验证生命周期、令牌失效强制清理和旧 renderer 兼容。`internal/app/shutdown.go` 提供 UseShutdown，在关库前等待业务后台任务；测试覆盖逆序、失败、panic、超时。模型显式凭据 API 及思考模式位于 `pkg/crawlagent/model/compatible.go`，不引入业务设置表或站点配置。

### 可视化与采样文件分工（2026-11-07）

- `admin/server/visual_assets.go` / `visual_assets_test.go`：检测当前链接 oao 的成对静态资源；HTML 不引用旧依赖不存在的文件。`TestMonitorOptionalVisualReferencesMatchLinkedDependency` 在本地新版与发布旧版两配置下复验。
- `admin/server/monitor_metadata_test.go`：Monitor 实例标识、响应时间、系统未采样元数据及旧字段兼容。
- `admin/sysinfo/sysinfo.go` / `system_sampling_test.go`：CPU、内存和磁盘锁外采样、短锁发布；目录快照不丢失，可用性、成功时间、合法零值、采样失败与读取不阻塞回归。
- `admin/server/static/visual-state.js`：纯内存、受限观测模型，按真实时间去重/断线/重置；`visuals.js/css`：首页图表接线和响应式样式；`appearance.js`：注册既有设置槽的浏览器本地动效偏好。
- `testdata/visual_state_test.js` / `visuals_test.js`：真实生产 JS 的时间、身份、阶段集合、采样周期、同时间戳重连、旧响应、缺失值、前11项+其他总计、降级和销毁回归，由 `TestOperationsJavaScriptBehavior` 自动执行。`monitor_requests_test.js` 同时验证迟到/超时响应不进入观测历史。
- 通用图表、motion 与表格生命周期代码在独立 oao 仓库；Papa 不复制组件实现，不增加 SQL、真实站点或付费模型调用。

当前默认以 `GOWORK=off` 验证 go.mod 中已发布的 oao v1.4.0。修改尚未发布的 oao 时再用临时 go.work 联调；旧版兼容用独立临时 modfile 指向真实发布版本，不修改正式依赖或模块缓存。目标包 `go test ./admin/... ./cmd/monitor-demo`，并发 `go test -race ./admin/server ./admin/sysinfo ./cmd/monitor-demo`，全包静态检查与构建 `go vet ./...` / `go build ./...`。Node 不存在时行为测试显式跳过，不能称前端行为验证通过。

提交前二次复核新增 `monitor_identity_test.js`（外部凭据更换、晚到401、轮询恢复、新旧表格导航隔离），`workbench_test.js` 覆盖站点全部消失及当前模块清空筛选。`TestMonitorReducedMotionDisablesFeedbackTransforms` 守卫减弱动效下的反馈位移；`visuals_test.js` 核对系统资源范围。`TestCollectorSnapshotDirsAreIndependent` / `TestCollectorOwnsDirectoryConfig` 验证 map 所有权；`TestHarvestStageTotalsRetiresCurrentSnapshotAtomically` / `TestHarvestStageTotalsPreservesReplacementSnapshot` 以真实 worker pool 验证回收累计与重建身份。回收不在引擎锁内等待 worker 停止。

oao 独立运行 8 个 Node 行为脚本，新增 `oao_lifecycle_race_test.js` 覆盖迟到 POST、401重试、同步/异步 render 失败、清理重入与弹窗重入；这些脚本不由 oao 的 `go test` 自动执行。浏览器补验 CSS 跨宿主隔离、数据表滚动焦点、减弱动效与窄屏。完整审查证据和真实环境限制见 `docs/issues/2026-11-07-oao-monitor-visuals-review.md`。


### 四类导航与概览简化

`admin/server/static/navigation.js` 集中维护 module/table/page 的四类归属与二级菜单；`mo.js` 仍负责切页生命周期、鉴权、请求代次和日志弹窗。框架 task/operation_log 使用固定分类，普通表格默认 data；`Page.Section` 通过 `/api/pages` 输出，空/未知值回退 data。`Group` 仅用于类内分组，业务项目在自己的 UsePage 声明分类。`cmd/papa/templates/monitor_view.go.tmpl` 展示可选字段；已有项目不会被 upgrade 改写业务页面。

`template.html` 保留旧模块根及操作入口，新增 `mod-logs`、`section-nav` 与空分类区域；`layout.css` 在原样式后、业务 custom.css 前加载，用于轻量布局，不覆盖 oao 通用组件源码。`visuals.js` 按 details 展开状态创建/销毁图表，toggle/恢复可见仅重绘已接受快照；`workbench.js` 限制关注项三条并保留队列/暂停的完整详情入口。

`TestMonitorLayoutHasFourCategoriesAndSeparateLogs`、`TestMonitorOverviewHasOneDefaultChart` 守卫模板结构；`navigation_test.js`、`monitor_requests_test.js`、`monitor_identity_test.js` 执行真实生产 JS，覆盖四类映射、兼容回退、文本安全、未保存阻止导航、晚到清单/日志和凭据切换。`visuals_test.js` 覆盖 1→3→6 实例、折叠销毁、离页/后台不复活和旧依赖回退。`internal/app/page_sections_test.go` 通过真实 HTTP handler 验证 Section 契约，`cmd/papa/scaffold_page_sections_test.go` 检查新建模板。


旧 oao 的兼容回归 `testdata/legacy_oao_test.js` 通过 `ui_behavior_test.go` 读取当前链接依赖的真实 `oao.js`，使用仓内最小事件 DOM helper；不依赖相邻 oao 仓或复制动作实现。旧版验证查询/筛选/分页、确认 Promise 取消、强制旧动作的传输拒绝与过期401/正文；新版执行真实 render、筛选、正常 POST、销毁确认与迟到401/正文隔离，并断言不安装兼容包装。`mo.js` 的适配只接管同源独立虚拟前缀，其余 fetch 原样透传，不改服务端权限；旧版表格只读限制见 MONITOR 的版本与部署说明。

`TestMonitorLinkedVisualsBehavior` 从当前链接 oao 的 StaticFS 读取真实图表与动效脚本，运行 `testdata/linked_visuals_test.js`，覆盖观测边界、图表折叠/离页、pagehide 迟到回调、KPI 动画和滚动焦点保留。`native_session_test.js` 纳入 `TestOperationsJavaScriptBehavior`，验证批次/规则/质量/Agent 身份清理、旧确认与恢复响应隔离；`monitor_identity_test.js` 联合真实 mo/requests 验证冻结页面漏收 storage 事件后的凭据核对。DOM、动画调度及请求仍为受控替身，浏览器排版与实际 BFCache 命中须另验。

### 外观主题文件分工

- `static/appearance.js`：浏览器外观偏好、设置控件、跨标签页/页面恢复同步与订阅清理；设置只更新根属性，不重新挂载业务。
- `template.html`：CSS加载前恢复合法外观/明暗值；`static/mo.js` 保留明暗快捷入口和安全的凭据读取。显示选择以DOM当前状态为准，不能每次读取旧存储覆盖写入失败后的临时选择。
- `static/layout.css`：共享导航/响应式规则与按外观限定的覆盖；原 `mo.css` 是经典基础。简约根条件使用低权重 `:where`，body中的oao浮层同样继承；`visuals.css` 保留减弱动效优先。
- `appearance_layout_test.go`：主题根变量隔离、导航/浮层覆盖及动效结构守卫。`testdata/appearance_test.js` 执行首帧、实际主题/设置代码，覆盖7组状态行为；`storage_startup_test.js` 完整执行后台启动，4项验证存储读取失败仍保持未登录、无请求与无旧凭据复用。两者均由 `ui_behavior_test.go` 自动执行。

主题切换不改Go API、数据库或oao版本。业务适配在业务模块内，不能把站点类名写入Papa。浏览器需另验经典/简约×浅色/深色、390窄屏、表格浮层、原生弹窗、草稿与焦点；截图不能代替迟到请求/存储异常的确定性回归。

## 动态阶段调度实现

engine/taskflow_policy.go 为有界树的纯放行判据，engine/taskflow.go 管理 MySQL 运行锁/CAS/同步派生；claimTaskContext 统一检查新投递、加急、重试与恢复。终态通知仅唤醒回灌，不作为持久真相。测试包括独立 MySQL 并发与恢复、策略纯函数、后台请求与生产页面组件。见 [动态阶段调度](TASK_FLOW.md)。


### 后台默认鉴权回归（2026-11-07）

`admin/auth` 的 Verifier/Confirmer 逐请求查库，不允许用令牌数量缓存授权；Required 恒为 true。`admin/server` 未注入 VerifyToken 时拒绝受保护接口，AuthRequired 仅影响登录框。首次令牌通过本机 CLI 创建，业务扩展继续默认套 Monitor.Auth。

新增 `TestDefaultAuthenticationNeverAuthorizesEmptyStore`、`TestAuthenticationIgnoresStaleZeroCounts`、`TestDefaultAuthProtectsManagementAndExtensionRoutes`、`TestFirstTokenImmediatelyAuthenticatesAndRevocationImmediatelyDenies`。无关 handler 测试显式注入测试身份校验器，不借用生产匿名放行；HTTP 扩展测试用桩处理器验证鉴权前无副作用，并不替代各业务 API 自身的回归。


### 网络出口实现与验证（2026-11-07）

新增 `pkg/egress`：`policy.go` 规范化 URL、编译私网 origin 授权并解析地址，`ip.go` 统一公网分类，`http.go` 固定已验证 IP 建连，`proxy.go` 提供浏览器网关与上游隧道。策略在构造时复制，无全局可变授权。浏览器的 `egress.go` 固定安全参数，导航入口拒绝本地文件等 scheme；池拥有网关生命周期，隔离上下文共享父进程的网关。

`egress_test.go` 覆盖默认拒绝、显式授权、混合 DNS 答案、固定 IP、重定向、上游 CONNECT、关闭隧道；`integration_test.go` 从实际 HTML/下载器/通知 API 验证目标无请求及 M3U8 四类依赖。`pkg/browser/egress_test.go` 覆盖入口/参数与真实 Chrome 子资源、跳转、弹窗；真实用例需 `PAPA_EGRESS_BROWSER_BIN` 指定本机可执行文件，未配置显式跳过。其余浏览器回归可用 Rod 的 `-rod=bin=<绝对路径>` 指定本机 Chrome。测试仅用合成站点，外网模型与真实业务不参与普通回归。

调用方提供的代理和 Go 回调属于可信宿主配置，无法通过框架验证任意自定义网络代码；不要把这层等同于容器/系统防火墙。HTTPS 上游代理与明文 WebSocket 暂不支持的边界、代理连接复用代价写在 CORE_CONFIG 中。

`TestDownloaderConfigCopiesCannotExpandPrivateGrant` 验证外部配置和 Config 副本不能改变 With/SetClient 授权；`TestHTMLHotReloadCannotGrantPrivateOrigin`、`TestInvalidDownloaderProxyNeverFallsBackToDirect` 验证热更与代理错误不会扩大权限。`pkg/middleware/m3u8/egress_ffmpeg_test.go` 的真实合并测试需 `PAPA_EGRESS_FFMPEG_BIN`，同时验证本地片段成功与嵌套网络清单被拒绝。

本机普通浏览器与 Agent 的严格沙箱渲染器是不同验证路径；后者若报 `webaccess: controlled renderer unavailable`，不能用普通浏览器通过代替。不要为测试成功而取消 Agent 沙箱。Rod 的 `-rod` 参数仅适用于链接 Rod 的测试包，不能直接附加给所有 Go 包。

### 模型配置与受控 HTTP 扩展文件

- `admin/configstore/model.go` / `model_test.go`：模型设置、硬上限、快照转换、修订 CAS；复用 crawler_config，不新增迁移。
- `admin/configstore/model_secret.go` / `model_secret_test.go`：只写 API Key、本机 AES-GCM 主密钥、冻结凭据与受额度约束的 Provider 构造。
- `admin/configstore/model_quota.go` / `model_quota_test.go`：北京时间日用量、事务预留/结算、未知占用和跨日归属；真实 SQL/并发/重连验收由独立业务通过公开 API 执行。
- `admin/server/model_settings.go` / `model_settings_test.go`：具名设置接口、严格 JSON、错误映射。
- `admin/server/static/model-settings.js` / `testdata/model_settings_test.js`：设置表单与晚到响应、未保存输入、重复保存、身份清理回归，接入现有 `TestOperationsJavaScriptBehavior`。
- `engine/rulefetcher_http_source_test.go`：受控回调与原来源校验/账号/浏览器边界；实现继续在 rulefetcher_source.go/types.go。
- `pkg/webaccess/http.go` / `http_test.go`：GetNoRedirect 与 robots/预算/跳转回归。

MySQL CAS、重开连接读取与真实 Engine 多阶段/证据/清洗接线的集成验收放独立业务模块，以显式 opt-in 专用测试库运行，普通框架测试不访问真实站点或模型。

`pkg/webaccess/robots_optional_test.go` 覆盖默认不请求 robots、显式遵守沿用旧测试、私网/请求/字节/限速保留，以及真实 Chromium 页面→脚本→API 录制。Policy.RespectRobots 为宿主冻结配置，默认 false；旧强制 robots 行为的调用者须显式设 true。

### Agent 运行概览数据与图表

`admin/server/agent_activity.go` 复用 `AgentAdministration.Runs/Run` 生成 `/api/monitor.agent_activity`，总超时 2 秒，最多一页运行与一页步骤，失败仅返回固定错误码；不携带原始错误、提示词、正文或密钥，不引入新表或业务站点。`agent_activity_test.go` 覆盖时间范围/超时、非时间序 ID 的排序、详情新快照对齐、读取失败隔离与空页。

`static/agent-activity.js` 只消费主监控请求已经接受的快照，使用 `visuals.js` 的统一图表生命周期；无额外轮询或异步响应通道。`testdata/agent_activity_test.js` 纳入 `TestOperationsJavaScriptBehavior`，验证 Token 与队列口径分离、运行中/终态步骤耗时、人工发布提示、站点过滤、离页销毁、身份清空、旧图表降级与读取异常。真实业务的审计与页面效果在独立模块验证。
