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

浏览器身份分层：`pkg/browser/identity` 负责声明/原生 CDP/观察（不依赖 GORM），其 `runtime` 子包负责 Store 与单实例绑定作用域；`pkg/account` 负责候选认证状态，`engine` 负责认领前固定，`internal/app` 提供 OA 和本地诊断。见 [BROWSER_IDENTITY.md](BROWSER_IDENTITY.md)。本地 Chromium 测试使用 `PAPA_EGRESS_BROWSER_BIN` 和 `PAPA_TEST_CHROME`；隔离 MySQL 测试可用 `PAPA_RUNTIME_TEST_CONFIG` 读取连接配置并创建自有临时库，不对业务库迁移。

账号生命周期仍复用 `pkg/account.Pools` 协调器：`NeedsVerify`、仅检查/恢复模式、持久政策和使用计量在该包内。`pkg/account/onboarding` 提供业务 Provider、候选身份确认、加密存储引用、登录过程及受控远程浏览器；业务真实站点实现留在独立模块。`core/compensation.go` 定义错误补偿契约，`engine/compensation.go` 统一原任务的预览、条件判断和 CAS；`admin/configstore` 将运行策略与最近历史放在同一事务中，`internal/app` 负责 OA 接线。

`internal/gormfake.EnableTransactionStatements` 只让 SQL/API 分支测试通过显式事务边界，不模拟回滚或隔离。真实事务保证由 `TestRuntimePolicyMySQLConcurrentCAS`（并发唯一赢家、历史失败回滚）和 `TestCompensationMySQLPersistenceRecoveryCASAndIdentity` 等隔离 MySQL 测试验证；不要将假库通过当成数据库验收。新增 OA 页面与模式使用真实 Chromium 合成 fixture 验证，不访问真实业务站点。

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
  这类错只能靠扫源码或真库跑一次才现形。
- **`admin/gormsource/sqlquote_test.go`**：后台表格的数据源（筛选 / 全局搜索 / 排序三处都直接往 SQL 里拼列名），
  钉的是"加引号只有 `quote()` 一个出口"，两层一起看 —— **行为层**用 gorm 的 DryRun 把一张表的**整个声明面**
  跑过一遍，断言每个列名只以反引号形态出现、绝不裸写（列名清单从表格声明里取，加一列自动跟上）；
  **结构层**扫本包非测试源码，反引号字面量只许出现在 `quote()` 里（排序那条原先自己拼了一对反引号，
  正是被它拦下才改成调 `quote()` 的）。它与上一条的分工：上一条问"这个名字是不是保留字"，这一条问
  "这个名字有没有加引号"—— 判据不重叠，所以两条都要在。

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
core/                           零依赖叶子包：跨包流动的纯值 + 不依赖任何内部包的纯函数
                                （告警 / 错误分类 / 监控快照 DTO、ctx 携带的请求头与代理、URL 解析与同站判定、
                                页面身份与任务幂等键 identity.go、站点地址回钉 siteurl.go）
config/                          配置结构与校验；runtime.go 引用公开治理/代理策略并保留显式 patch 语义
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
  breaker_runtime.go  分类状态机、受限准入/排空、独立报告定时写入与持久化屏障
  breaker_recovery.go  全阶段验证进度、整站试探、真实任务验证、最终清零与统一时间线
  breaker_site_recovery_test.go / breaker_runtime_test.go / breaker_concurrency_test.go / breaker_runtime_mysql_test.go
  breaker_readonly_test.go  显式完整只读覆盖、两次间隔检查、混合阶段、并发晚结果及重启回归（含隔离 MySQL）
               阶段隔离、在途终态、观察退出、阻塞写入/重试节奏、重启及 MySQL 回归
  breaker_snapshot.go  运行控制到只读报告投影；裁剪不回写控制
  breaker_samples.go / breaker_hourly_test.go  失败样本的版本化重试、三次验证与小时复查回归
  breaker_review_fixes_test.go  截断、标识、中断、重试/强制解除与重启回归
  breaker_reports_test.go  唯一事件 ID、Detector 证据快照、人工暂停生命周期和并发回归
  batch.go     各治理队列共用的分页扫描（keyset 游标）
  dedup.go     内存去重表 + 活跃任务装载
  sitestat.go  站点表与站点维度快照（启动播种、轮询统计回写、熔断状态）
  stats.go     监控快照
  fetch.go     浏览器池 / 静态 HTML 客户端 / 渲染等待与 FetchRenderedPageWith
  record.go    有界网络录制门面（仅摘要 trace）
  http.go      通用 HTTP/API、增量与浏览器响应捕获门面（只记录摘要 trace）
  account.go   显式账号管理器接线、认证 HTTP/HTML/浏览器和无凭据状态快照
  account_pool.go  池任务归属引用保护、认领前账号准备和显式池请求门面
  governance.go  Go/YAML/OA 优先级、已注册站点/物理容量校验、任务 context 与执行放行
  repeat_submission_test.go   公共提交入口独立对象并发，同一 pending 轮次保持计数
  repeat_submission_mysql_test.go 专用 MySQL 库：并发、首次初始化、恢复、限额、提交结果不确定
  stage_runversion_test.go     普通 worker 认领版本传递、重试固定和旧轮次终态拒绝
  capture.go   浏览器网络请求捕获的引擎层包装（trace 步骤 + 归档登记）
  downloads.go 下载器解析（阶段 > 站点 > 基座）
  archive.go   页面归档：失败那一刻的原始页面 + trace 步骤
  restricted.go 受限页判定（站点词表从 task.Site 取）
  deps.go      阶段依赖声明（NeedsBrowser 的启动期校验）
  queuestats.go 三条治理队列的运行快照与积压（全局各一条）
  dynticker.go 间隔可动态调整的定时器（错误/轮询队列的节拍）
  alias.go     给 core 里的值类型留的别名（见文末）
  recoverqueue.go · errorqueue.go · repeatpoll.go · delayqueue.go · taskadmin.go · trace.go
admin/                          ★后台模块：**对外公开**，业务项目可以直接 import 复用
  server/       监控后台（HTML / API / 静态资源）
    breaker_control_test.go / testdata/breaker_control_test.js  重试与强制解除入口、确认和审计
    breaker_analysis.go / breaker_analysis_test.go  鉴权只读熔断分析、指定现场导出、空态和过期响应
    breaker_analysis_ui_test.go / breaker_analysis_browser_test.go  请求竞态、纯文本证据、历史切换和窄屏浏览器回归
    ui_behavior_test.go        Node 执行生产 JS 与依赖真实 renderer 的行为回归入口
    static/requests.js         原生 API 错误、轮询代次/超时/退避/可见性管理
    static/table-links.js      oao v1.3.4 整值 URL 兼容适配（table-loaded 扩展点）
  auth/         访问令牌校验与操作人身份
  tokenadmin/   令牌列表、新增、启停、删除与更新接口
  oplog/        操作日志（异步写入器 + 只读表格页）
  tasksource/   内置「任务」表，动作接线的参考实现
  gormsource/   配置驱动的 oao.Source，业务表格页直接复用
  frameworkdata/ 按显式选表生成只读查询；后台不默认挂载通用目录，不扫描业务表
    tables.go / source.go  模型列、受限 SQL 投影、联合主键稳定分页、查询超时
    sanitize.go / status.go  有界脱敏、鉴权状态探测（未迁移/不可用/可查询）
    tables_test.go / mysql_test.go  HTTP/SQL/并发回归与显式只读 MySQL 验证
  configstore/  运行期配置存储（crawler_config 表）+ 读缓存层（内存 / TTL 装饰器）
    versioned.go  runtime_policy 的 LoadVersioned/SaveCAS；旧 Save 拒绝写受保护键
  sysinfo/      机器指标 / 目录占用采集
internal/                       真正的内部件：不对外承诺 API，**模块外也 import 不到**
  workerpool/ database/ msgqueue/ track/ breaker/
    breaker/recovery.go / recovery_test.go  原始恢复控制数据与失败窗口快照恢复
    breakerstore/snapshot.go / store.go / history.go  控制快照与追加历史同事务；按日报时间读取历史
    models/breaker_history.go  追加运行记录表，随显式框架迁移创建
    breaker/detector.go / policy.go / detector_test.go  有界终态轮次、短窗口同类/混合爆发、常规硬保护和恢复预算
  app/                          装配层：把上面这些接成 App（门面 papa.App 指向它）
    rod_diagnostics.go / rod_diagnostics_browser.go  鉴权诊断 handler、环境检查及隔离 Chromium/Rod 启动链路
    rod_diagnostics_view.go / rod_diagnostics.js  系统管理诊断页与嵌入脚本
    rod_diagnostics_test.go    启动失败、目录清理与脱敏回归
    data_tables.go / data_tables_test.go  只接入可操作任务，Trace 走行内弹窗；保留 task API 和动作保护
    runtime_policy.go / runtime_control.js  持久策略加载发布、远端等待确认和采集控制页
    account_pools_api.go / account_pools.js  池/成员 CAS 动作、账号池页面与显式复验
    proxy_health_api.go  代理管理 CAS、注册站点的有界 HEAD 探测与脱敏审计
pkg/                             可独立复用的件
  events/                        结构化运行事件：契约（event.go）+ 异步 emitter + JSONL sink
  browser/                       池与页面生命周期（pool.go / rod.go）、网络请求捕获（capture.go）、
                                 等待条件（wait.go）、按页资源拦截（block.go）
  browser/record.go              多交换网络录制、预算与显式脱敏导出
  browser/state.go               同源隔离页面登录态快照、AES-GCM 加密读写与恢复
  browser/state_navigation.go    首次导航前 Cookie/LocalStorage 恢复与 origin 检查
  browser/isolation.go           回调式独立 Chromium 上下文、取消与异常清理
  browser/response.go            按响应匹配并等待正文；session.go 提供浏览器 Cookie 到 HTTP 会话的显式桥接
  browser/governance.go          借用资源许可、当前 page target 范围的请求拦截与延迟反馈
  browser/stats.go               真实实例生命周期统计，区分存活/借用/预约/空闲；不把未创建槽计入存活
  htmlfetch/                     client.go HTML；request.go 通用请求；json.go 严格 JSON/内嵌 JSON；session.go 独立 CookieJar；session_guard.go 每账号检查/刷新并发协调；sample.go 显式脱敏样本与离线响应
  discovery/                    links.go URL/HTML；xml.go 订阅源；robots.go 规则；pagination.go 分页停止
  extract/validate.go            JSON 字段与批次唯一性校验
  htmlfetch/incremental.go       条件 GET、304 与内容指纹
  htmlfetch/session_state.go     完整 Cookie 属性追踪、版本化快照与受限恢复
  account/                      types.go 契约；manager.go 注册/认证/请求/版本；browser.go 账号浏览器与桥接；store.go 加密存储
  account/pool_types.go / pools.go  站点内黏性账号池、独立调度、有界验证与迟到结果隔离
  account/pool_store.go           管理状态/池状态 GORM CAS 存储；不自动迁移、不保存凭据
  account/pool_request.go / pool_capture.go  尝试身份固定的 HTTP/HTML/浏览器/捕获入口
  governance/                    config.go / patch.go 策略；controller.go 请求与许可；resources.go / sampler.go 采样与预算；transport.go HTTP计量
  middleware/proxy/health.go / transport.go  引用反馈、冷却/探测/固定出口、CAS和每交换健康包装
  egress/proxy_failure.go         代理本身拨号/认证失败的安全分类，目标错误不混入
  loggers/ notify/ middleware/{proxy,m3u8,filedown}
  redact/                       诊断副本 URL/Text/JSON 已知凭据遮盖（不改请求、任务身份与恢复数据）
  middleware/output_locks.go    下载目标/断点资源的整组路径锁（目录与子路径互斥、可取消、With 共享）
obs/                             运行报告引擎（通用、只读）：obs.go 契约 / load.go 三层装载 /
                                 eventlog/ 事件读取 / logidx/ 日志证据 / analyze/ 通用分析节 /
                                 pattern/ 内置模式库 / report/ 4 产物渲染
  incident/incident.go / classify.go / failure.go  实时与历史报告共用的错误分组、脱敏证据、结构化分类及文本导出
  incident/failure_test.go           稳定错误码、数字保留、HTTP/网络/认证/等待条件分类
  incident/incident_test.go          状态码保留、窗口/数量上限、脱敏和原因置信边界
  analyze/breaker_reports.go / breaker_reports_test.go  直接查窗口相关/活动持久报告；离线按最近事件 ID 引用，不重建
models/                          框架自带的表结构
cmd/papa/                        CLI（new / check / upgrade / migrate / token / html / rod / diff / select / report / strategy-migrate inspect）
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
pkg/strategymigrate ──► database/sql + MySQL错误分类（只读旧表/目标表盘点；业务清单由宿主提供）
pkg/temporary ──► 标准库 + UUID（Files安全目录、Cleaner清理顺序；业务实现CleanupStore）
pkg/testdb ──► 标准库（独占测试库创建/清理；不包含业务库白名单）
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

`pkg/account.Manager` 按站点＋账号管理登录态及账号许可；`account.Pools` 按站点＋池名保存当前账号与补偿状态。`pkg/governance` 的请求预算按站点跨阶段/账号共享，资源许可按进程全局分配。任务表仅增加池归属，不保存凭据；已有任务的去重身份不含账号/池。详见 [ACCOUNT_RUNTIME.md](./ACCOUNT_RUNTIME.md)、[RUNTIME_CONTROL.md](RUNTIME_CONTROL.md)。

| 层 | 是什么 | 在哪儿 |
| --- | --- | --- |
| 声明 | `Key` / `BaseURL` / `Headers` / `RestrictedKeywords` / `StartPaused` / `Breaker` / `M3U8` / `FileDown` / `Stages` | `internal/app/SiteSpec` |
| 执行 | **每站每阶段一个 workerpool**（阶段键是 `core.StageKey(site, stage)`：阶段名只在本站内唯一，两个站的 `catalog` 是两个池子） | `engine.stage.go` 建池那一段 |
| 执行 | **每站一个终态 Detector**；阶段分类用于定位，自动保护统一整站；全阶段验证与在途排空完成后才清零，人工解除不清零 | `internal/app/spec.go` 的 `defaultBreakerConfig`、`engine/breaker_runtime.go`、`engine/breaker_recovery.go` |
| 执行 | **暂停是一处信号**（`engine/pause.go`，按 scope：整站 / 某站某阶段）：写它的人有三个 —— 后台/业务手动、熔断触发、声明期的 `SiteSpec.StartPaused`（`applyStartPaused`，启动那一刻下一道整站闸）；两个消费者 —— 取任务点的闸门（冻住已排队的）+ 入队点的寄放（不接新活，寄放到溢出表） | `engine/pause.go`、`engine/stage.go` 的 `ApplyRegisterStage`、`engine/submit.go` 的 `submitToStage`、`engine/spill.go` 的 `drainSpilled` |
| 执行 | 告警事件带 `site` | `core.TaskError.Site`（`core.AlertEvent` 内嵌它） |
| 统计 | 阶段快照的键是 `site:stage`，条目里另带裸阶段名 `Stage` 与 `Site`（后台按 `site` 分组、标题用 `stage`）；熔断状态带 `Site` | `core.StageStats` / `core.BreakerStatus` |
| 统计 | 站点快照（**内存是源**：启动读 `crawler_config` 的 `sites` 键（没有就按声明生成）、按声明对齐；`StartPaused` 也在这**一次**写里落成暂停态，手动暂停/恢复整块回写） | `engine/sitestat.go`、`admin/configstore/sites.go` |
| 后台 | 顶部站点 Tab、站点概要；任务表有 `site` 列与筛选 | `admin/server/static/mo.js`、`admin/tasksource/table.go` |
| 数据 | 任务的 `site` 列（提交时**必填**，重投路径照抄回来）；唯一索引 `(url_hash, stage, site)` | `models/crawler_task.go`、`engine.submit.go` |
| 治理 | 同站请求并发/速率/AutoThrottle、阶段有效执行并发、站点服务权重 | `pkg/governance`、`engine/governance.go` |
| 身份 | 站点内命名池保持当前账号；账号管理状态由同站多池共享；task.account_pool 保留补偿归属 | `pkg/account/pools.go`、`engine/account_pool.go` |

### 全局，且**有意**如此（别想着分站）

- **进程级**：`crawler.stop_timeout`（停机怎么等）、`crawler.drain_interval`（溢出回灌跳多久一次）、
  `server.*`（HTTP 服务/白名单/采样间隔）、`db.*`、`log.*` —— 一个进程只有一份。
- **客户端级**：`html.*` / `browser.*`（超时、体积上限、空闲回收、全局 headers）。站点要自己的头就用
  `SiteSpec.Headers`（同键覆盖全局那层）；单次请求还能 `papa.WithHeaders`。
- **代理出口**：管理器全局持有 provider 列表与有界健康记录；业务仍决定显式出口/固定账号绑定，健康管理不跨账号换代理。新入口使用 Acquire/AcquireURL 与实际结果反馈，开启后耗尽报错。按站点分流仍由业务配置。
- **资源预算**：CPU/内存采样和任务/浏览器借用总许可属于单进程；站点权重分配就绪工作的服务机会，不增加物理容量。
- **操作日志/访问令牌**：与站点无关（前者记的是后台动作，后者是后台的凭据）。

### 按站点切的进度：三队列已**并回全局**；`crawler` 那四项**有意**保持全局

- **`error_queue` / `recover_queue` / `repeat_queue` 现在是全局各一条**（不再按站点拆）：
  各一段写在 `config.yaml` 里，后台各占一行。当初按站点拆没有收益（三队列本质都是对
  `crawler_tasks` 表的一次复查），反而让"一个阶段占一个协程空等"的多站开销被放大 —— 已并回全局。
- **`crawler` 的四项有意留在 `config.yaml`（定过的，不是待办）**：`queue_watermark`（`engine/stage.go`
  建池那行）、`archive.*`（原始 HTML 按 `{site}/{stage}/` 分层）、`trace.*`
  （`engine/trace.go`，步骤记录本来就带站点）、`dedup_cache_size`（`engine.NewEngine`）。
  它们是**进程级资源**那类数值（水位、去重预算、保留期、归档总闸），调的是"这个进程扛不扛得住"，
  而不是"这个站要怎么抓"；目前没有逐站调的真实需求，按站点拆等于把一句话摊成 N 份。
- **`dedupCache` 是"按 key 分、预算共享"**：key 是版本化的站点、阶段、身份类型和值的 SHA-256（`Task.Unique()`），但只有**一个**
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



运营首页位于 `admin/server/static/workbench.js` / `workbench.css`；`buildWorkbench` 只派生当前监控快照，`renderWorkbench` 与 `setWorkbenchConnection` 分别接到现有渲染及连接状态。`workbench_test.js` 覆盖统计口径、站点并集、缺失数据、采样时间、XSS、导航作用域、同名按钮焦点、折叠状态和 `__all__` 站点名；`TestWorkbenchTemplateKeepsOperationsAndDetails` 验证模板及嵌入资源接线；`TestDemoTaskSiteFilter` 让演示数据能真实验证本站任务跳转。此路径不增加 HTTP 请求或数据库读取。









### 可视化与采样文件分工（2026-11-07）

- `admin/server/visual_assets.go` / `visual_assets_test.go`：检测当前链接 oao 的成对静态资源；HTML 不引用旧依赖不存在的文件。`TestMonitorOptionalVisualReferencesMatchLinkedDependency` 在本地新版与发布旧版两配置下复验。
- `admin/server/monitor_metadata_test.go`：Monitor 实例标识、响应时间、系统未采样元数据及旧字段兼容。
- `admin/sysinfo/sysinfo.go` / `system_sampling_test.go`：CPU、内存和磁盘锁外采样、短锁发布；目录快照不丢失，可用性、成功时间、合法零值、采样失败与读取不阻塞回归。
- `admin/server/static/visual-state.js`：纯内存、受限观测模型，按真实时间去重/断线/重置；`visuals.js/css`：首页图表接线和响应式样式；`appearance.js`：注册既有设置槽的浏览器本地动效偏好。
- `testdata/visual_state_test.js` / `visuals_test.js`：真实生产 JS 的时间、身份、阶段集合、采样周期、同时间戳重连、旧响应、缺失值、前11项+其他总计、降级和销毁回归，由 `TestOperationsJavaScriptBehavior` 自动执行。`monitor_requests_test.js` 同时验证迟到/超时响应不进入观测历史。
- 通用图表、motion 与表格生命周期代码在独立 oao 仓库；Papa 不复制组件实现，不引入真实站点业务依赖。

当前默认以 `GOWORK=off` 验证 go.mod 中已发布的 oao v1.4.0。修改尚未发布的 oao 时再用临时 go.work 联调；旧版兼容用独立临时 modfile 指向真实发布版本，不修改正式依赖或模块缓存。目标包 `go test ./admin/... ./cmd/monitor-demo`，并发 `go test -race ./admin/server ./admin/sysinfo ./cmd/monitor-demo`，全包静态检查与构建 `go vet ./...` / `go build ./...`。Node 不存在时行为测试显式跳过，不能称前端行为验证通过。

提交前二次复核新增 `monitor_identity_test.js`（外部凭据更换、晚到401、轮询恢复、新旧表格导航隔离），`workbench_test.js` 覆盖站点全部消失及当前模块清空筛选。`TestMonitorReducedMotionDisablesFeedbackTransforms` 守卫减弱动效下的反馈位移；`visuals_test.js` 核对系统资源范围。`TestCollectorSnapshotDirsAreIndependent` / `TestCollectorOwnsDirectoryConfig` 验证 map 所有权；`TestHarvestStageTotalsRetiresCurrentSnapshotAtomically` / `TestHarvestStageTotalsPreservesReplacementSnapshot` 以真实 worker pool 验证回收累计与重建身份。回收不在引擎锁内等待 worker 停止。

oao 独立运行 8 个 Node 行为脚本，新增 `oao_lifecycle_race_test.js` 覆盖迟到 POST、401重试、同步/异步 render 失败、清理重入与弹窗重入；这些脚本不由 oao 的 `go test` 自动执行。浏览器补验 CSS 跨宿主隔离、数据表滚动焦点、减弱动效与窄屏。


### 四类导航与概览简化

`admin/server/static/navigation.js` 集中维护 module/table/page 的四类归属与二级菜单；`mo.js` 仍负责切页生命周期、鉴权、请求代次和日志弹窗。框架 task/operation_log 使用固定分类，普通表格默认 data；`Page.Section` 通过 `/api/pages` 输出，空/未知值回退 data。`Group` 仅用于类内分组，业务项目在自己的 UsePage 声明分类。`cmd/papa/templates/monitor_view.go.tmpl` 展示可选字段；已有项目不会被 upgrade 改写业务页面。

`template.html` 提供当前监控模块根及操作入口，新增 `mod-logs`、`section-nav` 与空分类区域；`layout.css` 在原样式后、业务 custom.css 前加载，用于轻量布局，不覆盖 oao 通用组件源码。`visuals.js` 按 details 展开状态创建/销毁图表，toggle/恢复可见仅重绘已接受快照；`workbench.js` 限制关注项三条并保留队列/暂停的完整详情入口。

`TestMonitorLayoutHasFourCategoriesAndSeparateLogs`、`TestMonitorOverviewHasOneDefaultChart` 守卫模板结构；`navigation_test.js`、`monitor_requests_test.js`、`monitor_identity_test.js` 执行真实生产 JS，覆盖四类映射、兼容回退、文本安全、未保存阻止导航、晚到清单/日志和凭据切换。`visuals_test.js` 覆盖 1→3→6 实例、折叠销毁、离页/后台不复活和旧依赖回退。`internal/app/page_sections_test.go` 通过真实 HTTP handler 验证 Section 契约，`cmd/papa/scaffold_page_sections_test.go` 检查新建模板。


旧 oao 的兼容回归 `testdata/legacy_oao_test.js` 通过 `ui_behavior_test.go` 读取当前链接依赖的真实 `oao.js`，使用仓内最小事件 DOM helper；不依赖相邻 oao 仓或复制动作实现。旧版验证查询/筛选/分页、确认 Promise 取消、强制旧动作的传输拒绝与过期401/正文；新版执行真实 render、筛选、正常 POST、销毁确认与迟到401/正文隔离，并断言不安装兼容包装。`mo.js` 的适配只接管同源独立虚拟前缀，其余 fetch 原样透传，不改服务端权限；旧版表格只读限制见 MONITOR 的版本与部署说明。

`TestMonitorLinkedVisualsBehavior` 从当前链接 oao 的 StaticFS 读取真实图表与动效脚本，运行 `testdata/linked_visuals_test.js`，覆盖观测边界、图表折叠/离页、pagehide 迟到回调、KPI 动画和滚动焦点保留。`monitor_identity_test.js` 联合真实 mo/requests 验证冻结页面漏收 storage 事件后的凭据核对。DOM、动画调度及请求仍为受控替身，浏览器排版与实际 BFCache 命中须另验。

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


导航归属：`internal/app/page_ownership.go` 校验 Page.Owns 的引用与无环性，`admin/server/static/navigation.js` 负责菜单归属和直接访问高亮；不改变后端授权。

临时资源基础能力：`pkg/temporary/files.go` 负责宿主目录与UUID文件路径；`cleanup.go` 负责已授权删除的顺序与重试。业务独占同ID的写入/读出/删除锁并实现CleanupStore事务，框架不导入业务models。`pkg/testdb/schema.go` 只清理本次独占创建的测试库。回归 `go test -race ./pkg/temporary ./pkg/testdb`；契约见 [TEMPORARY_RESOURCES.md](TEMPORARY_RESOURCES.md)。

历史表盘点：`pkg/strategymigrate/inspect.go`只读事务盘点表存在性及行数，`cmd/papa/strategy_migrate.go`提供inspect命令及64KiB业务manifest。未实现任何写模式，报告migration_ready=false。`inspect_test.go`检查只读事务、缺表非零行、错误不能冒充完成、表名/清单校验；CLI测试检查拒绝写命令和输出不覆盖。详见STRATEGY_MIGRATION.md。

## 后台资源与移除功能的回归

运行治理回归：`go test ./pkg/account ./pkg/governance ./pkg/middleware/proxy ./admin/configstore ./engine ./internal/app`；并发路径运行相同包的 `go test -race`（Windows 需 `CGO_ENABLED=1` 和 C 编译器）。`pkg/governance` 使用可注入时钟/采样器验证速率、远端期限、滞回和单许可公平性；代理/账号失败使用本地合成服务，不访问业务站点。

真实浏览器用例读取 `PAPA_EGRESS_BROWSER_BIN`；包含页面导航、XHR、子资源、重定向、第三方排除以及 OA 草稿/冲突交互，未配置时明确跳过。覆盖只针对当前受管 page target 与显式 origin，不把 popup/worker/raw Rod 页面算作已经验证。MySQL CAS 回归 `TestRuntimePolicyMySQLConcurrentCAS` 用 `PAPA_RUNTIME_TEST_CONFIG` 指向连接配置，独占创建测试库并清理；普通假库测试不能替代它。脚手架 `TestScaffoldBuildsAndUpgradePreservesAppDocs` 实际 new → tidy → build，并检查升级保留应用文档。

后台只注册现有通用能力。`admin/server/ui_behavior_test.go` 的 `TestMonitorTemplateLocalAssetsExist` 检查模板引用的本地资源确实嵌入；`TestOperationsJavaScriptBehavior` 执行 Node 行为测试，覆盖导航、身份切换、草稿保护、队列及工作台。`navigation_test.js` 检查当前内置模块和对应页面，`taskflow.go` / `taskflow.js` 保留动态任务流入口。

`admin/frameworkdata/tables_test.go` 使用现有框架模型验证只读目录、鉴权、查询投影、稳定排序、脱敏和缺表恢复，不依赖已删除的 Agent/规则模型。历史旧表仅在 `pkg/strategymigrate` 的只读预检清单中保留，不会自动迁移或删除。

`admin/frameworkdata/tables.go` 根据 GORM schema 向 `source.go` 传递 JSON 列清单，查询结果按 JSON 递归脱敏，不仅依赖 `_json` 字段名后缀；现有任务流的 `policy` 同样受保护。

移除无效的 `configstore.NewWithModelKeyFile` 后，配置存储统一使用 `configstore.New(db)`；任务流操作日志回调类型为 `server.TaskFlowActionEvent`。这不影响 `pkg/account` 的凭据加密存储。

### 任务身份实现入口

任务时间另见 `docs/TASK_TIMING.md`：`models/task_timing.go` 负责首次相对排期，`models/utc_time.go` 隔离 DATETIME 与驱动时区，`engine/task_timing.go` 负责执行记录及延迟恢复；`engine/task_timing_test.go`、`engine/task_timing_mysql_test.go` 和 `models/utc_time_test.go` 验证持久化、恢复、失败隔离和时区。

- `core/task_identity.go`：纯函数，规范 URL 并生成标准身份。
- `internal/database/task_identity.go`：显式迁移中的分批回填、冲突拒绝及旧索引移除；普通启动不调用。
- `engine/task_identity_mysql_test.go`：独立随机 MySQL 测试库验证同 URL 不同 ID、恢复去重、真正旧表升级、重复迁移和冲突拒绝。
