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

## 6. 包结构与职责边界

完整目录和包职责统一维护在 [Papa 目录说明书](package_structure/README.md)。新增文件、公开入口或可变状态时，直接更新对应分册的文件主表、状态/锁表及生命周期说明；不要只在文末追加说明而让主表保留旧结论。

| 范围 | 职责分册 |
| --- | --- |
| 根包、config、core、models、文档及仓库配套 | [ROOT_STRUCTURE.md](package_structure/ROOT_STRUCTURE.md) |
| 后台及前端资源 | [ADMIN_STRUCTURE.md](package_structure/ADMIN_STRUCTURE.md) |
| 引擎和引擎专属内部组件 | [ENGINE_STRUCTURE.md](package_structure/ENGINE_STRUCTURE.md) |
| App 组合根、声明、运行服务、存储和通用内部设施 | [INTERNAL_STRUCTURE.md](package_structure/INTERNAL_STRUCTURE.md) |
| 账号、浏览器、HTTP、下载、出口和治理等通用能力 | [PKG_STRUCTURE.md](package_structure/PKG_STRUCTURE.md) |
| 观测、分析和报告 | [OBS_STRUCTURE.md](package_structure/OBS_STRUCTURE.md) |
| CLI、脚手架与合成演示 | [CMD_STRUCTURE.md](package_structure/CMD_STRUCTURE.md) |

**包职责清晰是长期开发硬规则。** 新增或修改功能先明确状态拥有者、接口、允许依赖、原子性和关闭契约，再选择文件；按职责拆分，不按文件数机械拆包。新增包/重要文件同步对应分册和说明书总索引，不在本节再维护第二份逐文件清单。

完整跨组件规则见 [ARCHITECTURE.md](ARCHITECTURE.md)。`TestPackageResponsibilityBoundaries` 只检查部分依赖与资源归属；改动并发、取消、排空、SQL 或页面时仍须按本手册选择行为验证。公开 App/Engine/Task/Fetcher 及既有别名继续保留兼容。

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
| 声明 | `Key` / `BaseURL` / `Headers` / `RestrictedKeywords` / `StartPaused` / `Breaker` / `M3U8` / `FileDown` / `Stages` | `internal/sitedecl/SiteSpec`（`internal/app` 与根包 `papa` 以类型别名透出） |
| 执行 | **每站每阶段一个 workerpool**（阶段键是 `core.StageKey(site, stage)`：阶段名只在本站内唯一，两个站的 `catalog` 是两个池子） | `engine.stage.go` 建池那一段 |
| 执行 | **每站一个终态 Detector**；阶段分类用于定位，自动保护统一整站；全阶段验证与在途排空完成后才清零，人工解除不清零 | `internal/sitedecl` 的 `defaultBreakerConfig`、`engine/breaker_runtime.go`、`engine/breaker_recovery.go` |
| 执行 | **暂停是一处信号**（`engine/pause.go`，按 scope：整站 / 某站某阶段）：写它的人有三个 —— 后台/业务手动、熔断触发、声明期的 `SiteSpec.StartPaused`（`applyStartPaused`，启动那一刻下一道整站闸）；两个消费者 —— 取任务点的闸门（冻住已排队的）+ 入队点的寄放（不接新活，寄放到溢出表） | `engine/pause.go`、`engine/stage.go` 的 `ApplyRegisterStage`、`engine/submit.go` 的 `submitToStage`、`engine/spill.go` 的 `drainSpilled` |
| 执行 | 告警事件带 `site` | `core.TaskError.Site`（`core.AlertEvent` 内嵌它） |
| 统计 | 阶段快照的键是 `site:stage`，条目里另带裸阶段名 `Stage` 与 `Site`（后台按 `site` 分组、标题用 `stage`）；熔断状态带 `Site` | `core.StageStats` / `core.BreakerStatus` |
| 统计 | 站点快照（**内存是源**：启动读 `crawler_config` 的 `sites` 键（没有就按声明生成）、按声明对齐；`StartPaused` 也在这**一次**写里落成暂停态，手动暂停/恢复整块回写） | `engine/sitestat.go`、`admin/configstore/sites.go` |
| 后台 | 顶部站点 Tab、站点概要；任务表有 `site` 列与筛选 | `admin/monitoring/static/controller.js`、`admin/tasksource/table.go` |
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

后台报告切换回归在 `admin/server/obsreport_race_test.go` 的 `TestObsReportLatestRequestWins`：有 Node.js 时执行 `testdata/obs_report_race_test.js`，以受控响应顺序运行真实的报告加载函数，覆盖响应头/正文晚到、旧请求失败、文件切换、A→B→A 和空报告。没有 Node.js 的环境跳过该行为测试；可运行 `go test ./admin/server -run TestObsReportLatestRequestWins -count=1`，测试会导出实际组合资源再执行 Node。

报告运行筛选回归在 `obs/load_run_scope_test.go` 的 `TestLoadSourcesGapCountFollowsRunAndWindow`：从临时 JSONL 与 gzip 轮转文件加载交错、多运行、重复及乱序事件，验证单次运行的缺口和 `event_gaps` 检查、全量汇总及时间窗口边界；文件读取诊断仍保留原范围。

验证包含 `TestScaffoldBuildsAndUpgradePreservesAppDocs`（生成真实项目并 tidy/build）、`TestCheckEndToEnd`（新站点目录校验及非法声明拒绝）、`TestLayoutDetectionAndLegacyUpgrade`（旧布局与优先级）及 `TestUpgradeWriteOnlyTouchesFrameworkCopies`（升级只写框架副本）。

浏览器临时目录：`pkg/browser/temp_dir.go` 统一解析、写入验证和独立子目录创建；`pool.go` 保存实际根目录。配置入口为 `config.BrowserConfig.TempDir`，引擎与调试 CLI 均传入池。



运营首页位于 `admin/monitoring/static/workbench.js` / `workbench.css`；`buildWorkbench` 只派生当前监控快照，`renderWorkbench` 与 `setWorkbenchConnection` 分别接到现有渲染及连接状态。`workbench_test.js` 覆盖统计口径、站点并集、缺失数据、采样时间、XSS、导航作用域、同名按钮焦点、折叠状态和 `__all__` 站点名；`TestWorkbenchTemplateKeepsOperationsAndDetails` 验证模板及嵌入资源接线；`TestDemoTaskSiteFilter` 让演示数据能真实验证本站任务跳转。此路径不增加 HTTP 请求或数据库读取。









### 可视化与采样验证

实现归属见 [Admin 分册](package_structure/ADMIN_STRUCTURE.md)。以下保留验证方式。

- `visual_assets_test.go` 的 `TestMonitorOptionalVisualReferencesMatchLinkedDependency` 在本地新版与发布旧版两配置下复验，确保 HTML 不引用旧依赖不存在的资源。
- `admin/server/monitor_metadata_test.go`：Monitor 实例标识、响应时间、系统未采样元数据及旧字段兼容。
- `system_sampling_test.go`：系统采样回归覆盖目录快照、可用性、成功时间、合法零值、采样失败与读取不阻塞。
- `testdata/visual_state_test.js` / `visuals_test.js`：真实生产 JS 的时间、身份、阶段集合、采样周期、同时间戳重连、旧响应、缺失值、前11项+其他总计、降级和销毁回归，由 `TestOperationsJavaScriptBehavior` 自动执行。`monitor_requests_test.js` 同时验证迟到/超时响应不进入观测历史。
- 通用图表、motion 与表格生命周期代码在独立 oao 仓库；Papa 不复制组件实现，不引入真实站点业务依赖。

当前默认以 `GOWORK=off` 验证 go.mod 中已发布的 oao v1.4.0。修改尚未发布的 oao 时再用临时 go.work 联调；旧版兼容用独立临时 modfile 指向真实发布版本，不修改正式依赖或模块缓存。目标包 `go test ./admin/... ./cmd/monitor-demo`，并发 `go test -race ./admin/server ./admin/sysinfo ./cmd/monitor-demo`，全包静态检查与构建 `go vet ./...` / `go build ./...`。Node 不存在时行为测试显式跳过，不能称前端行为验证通过。

提交前二次复核新增 `monitor_identity_test.js`（外部凭据更换、晚到401、轮询恢复、新旧表格导航隔离），`workbench_test.js` 覆盖站点全部消失及当前模块清空筛选。`TestMonitorReducedMotionDisablesFeedbackTransforms` 守卫减弱动效下的反馈位移；`visuals_test.js` 核对系统资源范围。`TestCollectorSnapshotDirsAreIndependent` / `TestCollectorOwnsDirectoryConfig` 验证 map 所有权；`TestHarvestStageTotalsRetiresCurrentSnapshotAtomically` / `TestHarvestStageTotalsPreservesReplacementSnapshot` 以真实 worker pool 验证回收累计与重建身份。回收不在引擎锁内等待 worker 停止。

oao 独立运行 8 个 Node 行为脚本，新增 `oao_lifecycle_race_test.js` 覆盖迟到 POST、401重试、同步/异步 render 失败、清理重入与弹窗重入；这些脚本不由 oao 的 `go test` 自动执行。浏览器补验 CSS 跨宿主隔离、数据表滚动焦点、减弱动效与窄屏。


### 四类导航与概览简化

导航、外观、图表和表格适配的职责边界统一见 [Admin 分册](package_structure/ADMIN_STRUCTURE.md)；页面扩展和 Owns 校验见 [Internal 的 App 章节](package_structure/INTERNAL_STRUCTURE.md#app)。具体字段和分类用法见 [MONITOR.md](MONITOR.md)。

`TestMonitorLayoutHasFourCategoriesAndSeparateLogs`、`TestMonitorOverviewHasOneDefaultChart` 守卫模板结构；`navigation_test.js`、`monitor_requests_test.js`、`monitor_identity_test.js` 执行真实生产 JS，覆盖四类映射、兼容回退、文本安全、未保存阻止导航、晚到清单/日志和凭据切换。`visuals_test.js` 覆盖 1→3→6 实例、折叠销毁、离页/后台不复活和旧依赖回退。`internal/app/page_sections_test.go` 通过真实 HTTP handler 验证 Section 契约，`cmd/papa/scaffold_page_sections_test.go` 检查新建模板。


旧 oao 的兼容回归 `testdata/legacy_oao_test.js` 通过 `ui_behavior_test.go` 读取当前链接依赖的真实 `oao.js`，使用仓内最小事件 DOM helper；不依赖相邻 oao 仓或复制动作实现。旧版验证查询/筛选/分页、确认 Promise 取消、强制旧动作的传输拒绝与过期401/正文；新版执行真实 render、筛选、正常 POST、销毁确认与迟到401/正文隔离，并断言不安装兼容包装。`admin/shell/static/table-adapter.js` 的适配只接管同源独立虚拟前缀，其余 fetch 原样透传，不改服务端权限；旧版表格只读限制见 MONITOR 的版本与部署说明。

`TestMonitorLinkedVisualsBehavior` 从当前链接 oao 的 StaticFS 读取真实图表与动效脚本，运行 `testdata/linked_visuals_test.js`，覆盖观测边界、图表折叠/离页、pagehide 迟到回调、KPI 动画和滚动焦点保留。`monitor_identity_test.js` 联合真实 mo/requests 验证冻结页面漏收 storage 事件后的凭据核对。DOM、动画调度及请求仍为受控替身，浏览器排版与实际 BFCache 命中须另验。

### 外观主题验证

主题资源与外观状态归属见 [Admin 分册](package_structure/ADMIN_STRUCTURE.md)。

- `appearance_layout_test.go`：主题根变量隔离、导航/浮层覆盖及动效结构守卫。`testdata/appearance_test.js` 执行首帧、实际主题/设置代码，覆盖7组状态行为；`storage_startup_test.js` 完整执行后台启动，4项验证存储读取失败仍保持未登录、无请求与无旧凭据复用。两者均由 `ui_behavior_test.go` 自动执行。

主题切换不改Go API、数据库或oao版本。业务适配在业务模块内，不能把站点类名写入Papa。浏览器需另验经典/简约×浅色/深色、390窄屏、表格浮层、原生弹窗、草稿与焦点；截图不能代替迟到请求/存储异常的确定性回归。

## 动态阶段调度验证

实现职责见 [Engine 分册](package_structure/ENGINE_STRUCTURE.md)，使用契约见 [动态阶段调度](TASK_FLOW.md)。验证包括独立 MySQL 并发与恢复、策略纯函数、后台请求与生产页面组件；终态通知只负责唤醒，不能替代持久状态核对。

### 后台默认鉴权回归（2026-11-07）

`admin/auth` 的 Verifier/Confirmer 逐请求查库，不允许用令牌数量缓存授权；Required 恒为 true。`admin/server` 未注入 VerifyToken 时拒绝受保护接口，AuthRequired 仅影响登录框。首次令牌通过本机 CLI 创建，业务扩展继续默认套 Monitor.Auth。

新增 `TestDefaultAuthenticationNeverAuthorizesEmptyStore`、`TestAuthenticationIgnoresStaleZeroCounts`、`TestDefaultAuthProtectsManagementAndExtensionRoutes`、`TestFirstTokenImmediatelyAuthenticatesAndRevocationImmediatelyDenies`。无关 handler 测试显式注入测试身份校验器，不借用生产匿名放行；HTTP 扩展测试用桩处理器验证鉴权前无副作用，并不替代各业务 API 自身的回归。


### 网络出口实现与验证（2026-11-07）

出口策略、浏览器网关和资源所有权详见 [Pkg 分册](package_structure/PKG_STRUCTURE.md)。以下说明对应验证层次。

`egress_test.go` 覆盖默认拒绝、显式授权、混合 DNS 答案、固定 IP、重定向、上游 CONNECT、关闭隧道；`integration_test.go` 从实际 HTML/下载器/通知 API 验证目标无请求及 M3U8 四类依赖。`pkg/browser/egress_test.go` 覆盖入口/参数与真实 Chrome 子资源、跳转、弹窗；真实用例需 `PAPA_EGRESS_BROWSER_BIN` 指定本机可执行文件，未配置显式跳过。其余浏览器回归可用 Rod 的 `-rod=bin=<绝对路径>` 指定本机 Chrome。测试仅用合成站点，外网模型与真实业务不参与普通回归。

调用方提供的代理和 Go 回调属于可信宿主配置，无法通过框架验证任意自定义网络代码；不要把这层等同于容器/系统防火墙。HTTPS 上游代理与明文 WebSocket 暂不支持的边界、代理连接复用代价写在 CORE_CONFIG 中。

`TestDownloaderConfigCopiesCannotExpandPrivateGrant` 验证外部配置和 Config 副本不能改变 With/SetClient 授权；`TestHTMLHotReloadCannotGrantPrivateOrigin`、`TestInvalidDownloaderProxyNeverFallsBackToDirect` 验证热更与代理错误不会扩大权限。`pkg/middleware/m3u8/egress_ffmpeg_test.go` 的真实合并测试需 `PAPA_EGRESS_FFMPEG_BIN`，同时验证本地片段成功与嵌套网络清单被拒绝。


导航归属校验和前端高亮的配合见 [Internal](package_structure/INTERNAL_STRUCTURE.md#app) 与 [Admin](package_structure/ADMIN_STRUCTURE.md) 分册；前端分类不改变后端授权。

临时资源、恢复清理和隔离测试库的职责见 [Pkg 分册](package_structure/PKG_STRUCTURE.md)，使用契约见 [TEMPORARY_RESOURCES.md](TEMPORARY_RESOURCES.md)。回归命令为 `go test -race ./pkg/temporary ./pkg/testdb`。

历史表盘点的实现归属见 Pkg 和 [Cmd 分册](package_structure/CMD_STRUCTURE.md)，使用范围见 [STRATEGY_MIGRATION.md](STRATEGY_MIGRATION.md)。`inspect_test.go` 检查只读事务、缺表非零行、错误传播、表名/清单校验；CLI 回归检查拒绝写命令和输出不覆盖。

## 后台资源与移除功能的回归

运行治理回归：`go test ./pkg/account ./pkg/governance ./pkg/middleware/proxy ./admin/configstore ./engine ./internal/app`；并发路径运行相同包的 `go test -race`（Windows 需 `CGO_ENABLED=1` 和 C 编译器）。`pkg/governance` 使用可注入时钟/采样器验证速率、远端期限、滞回和单许可公平性；代理/账号失败使用本地合成服务，不访问业务站点。

真实浏览器用例读取 `PAPA_EGRESS_BROWSER_BIN`；包含页面导航、XHR、子资源、重定向、第三方排除以及 OA 草稿/冲突交互，未配置时明确跳过。覆盖只针对当前受管 page target 与显式 origin，不把 popup/worker/raw Rod 页面算作已经验证。MySQL CAS 回归 `TestRuntimePolicyMySQLConcurrentCAS` 用 `PAPA_RUNTIME_TEST_CONFIG` 指向连接配置，独占创建测试库并清理；普通假库测试不能替代它。脚手架 `TestScaffoldBuildsAndUpgradePreservesAppDocs` 实际 new → tidy → build，并检查升级保留应用文档。

后台只注册现有通用能力。`admin/server/ui_behavior_test.go` 的 `TestMonitorTemplateLocalAssetsExist` 检查模板引用的本地资源确实嵌入；`TestOperationsJavaScriptBehavior` 执行 Node 行为测试，覆盖导航、身份切换、草稿保护、队列及工作台。`navigation_test.js` 检查当前内置模块和对应页面，`admin/taskflow/handler.go` / `static/taskflow.js` 保留动态任务流入口。

`admin/frameworkdata/tables_test.go` 使用现有框架模型验证只读目录、鉴权、查询投影、稳定排序、脱敏和缺表恢复，不依赖已删除的 Agent/规则模型。历史旧表仅在 `pkg/strategymigrate` 的只读预检清单中保留，不会自动迁移或删除。

框架表目录和 JSON 投影的职责见 [Admin 分册](package_structure/ADMIN_STRUCTURE.md)；回归不能只使用 `_json` 字段名，还需覆盖任务流 `policy` 等由 schema 标记的 JSON 列。

移除无效的 `configstore.NewWithModelKeyFile` 后，配置存储统一使用 `configstore.New(db)`；任务流操作日志回调类型为 `server.TaskFlowActionEvent`。这不影响 `pkg/account` 的凭据加密存储。

### 任务身份与时间验证

身份值与模型 hook 见 [根目录分册](package_structure/ROOT_STRUCTURE.md)，显式回填迁移见 [Internal 分册](package_structure/INTERNAL_STRUCTURE.md)，执行记录及延迟恢复见 [Engine 分册](package_structure/ENGINE_STRUCTURE.md)。时间使用规则见 [TASK_TIMING.md](TASK_TIMING.md)。

`engine/task_timing_test.go`、`engine/task_timing_mysql_test.go` 和 `models/utc_time_test.go` 验证持久化、恢复、失败隔离和时区。`engine/task_identity_mysql_test.go` 使用独立随机 MySQL 测试库验证同 URL 不同 ID、恢复去重、真正旧表升级、重复迁移和冲突拒绝。

`obs/analyze/breaker_history_limit_test.go` 验证日报截断不会推断错误截止状态。

单实例调度、报告输入和下载预算的装配 API、有限默认值及超限处理见 [资源预算与背压](RESOURCE_BUDGETS.md)。这些预算与站点/阶段的业务并发配置分开，不是新增 YAML 键；旧公开配置布局保留。

Trace 文件存储的包依赖、状态锁和文件归属见 [Internal 分册](package_structure/INTERNAL_STRUCTURE.md) 的 tracestore 与 [Engine 分册](package_structure/ENGINE_STRUCTURE.md) 的 Trace 章节。架构守卫限制 tracestore 仅依赖标准库与 core 值类型。
