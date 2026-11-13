# Cmd 目录结构与职责细则

> 面向：维护命令行入口、业务脚手架、框架文档分发和本地后台演示的开发者。
> 总索引：[目录职责说明书](README.md)。命令用法见 [CLI 手册](../CLI.md)，通用开发约束见 [开发手册](../DEVELOPMENT.md)与[总体架构规则](../ARCHITECTURE.md)。
> 本文描述目录、文件和副作用归属，不重复命令参数手册。

## 1. 定位与目录

`cmd/papa` 是独立 CLI 的组合入口，负责参数解析、目标项目识别、短时资源装配和结果展示。领域实现继续调用配置、数据库、抓取、观测和令牌组件，不能把 CLI 私有函数变成框架运行时依赖。`cmd/monitor-demo` 是本地合成数据演示程序，用真实后台组件验证展示；它不承载真实站点接入。

当前目录有 **2 个 main 包、16 个生产 Go 文件、20 个测试 Go 文件、25 份 `.tmpl` 资源**。文档分发还依赖目录外的 `docs/embed.go`。

```text
cmd/
├── papa/
│   ├── main.go                # 分派、new、模板渲染和文档复制
│   ├── layout.go              # 新旧业务目录识别
│   ├── module_compat.go       # v3 项目前置拒绝
│   ├── scaffold_safe.go       # 创建路径及不覆盖写入
│   ├── skill.go               # 随项目技能的唯一清单与渲染数据
│   ├── upgrade.go             # 差异计划与显式刷新
│   ├── check.go               # 临时程序校验站点/启动装配
│   ├── migrate.go             # 显式迁移与业务入口委派
│   ├── token.go               # 本机创建访问令牌
│   ├── strategy_migrate.go    # 只读表盘点
│   ├── report.go              # 离线报告装配、产物和列表
│   ├── debug.go               # HTML/Rod/对比/选择器调试
│   ├── act.go                 # Rod 页面动作与人工交接
│   ├── templates/             # Go、配置、部署、文档与技能模板
│   └── *_test.go              # 19 份测试文件
└── monitor-demo/
    ├── main.go                # 本地 HTTP、系统指标和合成数据
    ├── oa_demo.go             # 表格、动作和自定义页替身
    ├── token_demo.go          # 内存令牌与真实鉴权接线
    └── main_test.go           # 筛选、回环监听和随机令牌

docs/embed.go                  # 框架手册的嵌入入口（不是 templates 的一部分）
```

## 2. `cmd/papa` 逐文件职责

| 文件 | 当前职责 | 状态、依赖与副作用边界 |
| --- | --- | --- |
| `main.go` | 顶层子命令分派、usage、`scaffoldData`、module/标识符校验、`runNew`、模板渲染与 `copyDocs` | `tmplFS` 只读嵌入 `templates/*`；先校验名称，再生成目标项目。完整 Module 用于 import，本地 Project 取路径末段用于文件/构建名称；Go 产物通过 `go/format` |
| `layout.go` | `siteDirectory`、`frameworkDocsDir`，优先识别 internal/sites 与 docs/framework，兼容旧 sites 与 docs | new/check/upgrade/技能数据共用布局判据；不移动旧项目目录 |
| `module_compat.go` | `rejectLegacyProject` 扫描目标 go.mod 的 v3 模块引用 | check/upgrade/migrate 在后续临时写入、框架文档刷新或建库连接前拒绝混用；不自动重写 imports/require/replace |
| `scaffold_safe.go` | `checkScaffoldPath` 检查路径祖先；`writeScaffoldFile` 独占创建 | 拒绝符号链接、junction 等链接路径；`O_EXCL` 防止创建竞争截断现存文件；missing-only 只跳过已有普通文件 |
| `skill.go` | `fileSpec`、`skillFile`、`skillFiles`、技能内容渲染与项目布局数据 | 唯一登记 `.claude/skills/papa-crawler-development/` 的文件、通用刷新与项目所有权；new 和 upgrade 共同使用 |
| `upgrade.go` | 构建 `upgradePlan`、比对文档/技能、报告配置及声明问题、`writeDocs` / `writeSkill` | plan 是一次调用的结果，无后台状态；默认只报告。显式 write 才刷新登记的框架副本及通用技能；站点检查为文本提示，不等同执行声明校验 |
| `check.go` | 生成并运行普通/boot 两种临时校验程序，解析配置路径、目标 module 和站点目录 | 本次独占 `.papa-check-*` 目录，defer 只清理自己创建的目录。普通路径编译并调用声明校验；boot 走 New/RegisterSites 到 Run 前，不投任务、不启动服务 |
| `migrate.go` | `papaModule`、业务 go.mod 识别、参数处理、框架迁移或 `delegateMigrate` | 业务项目委派 `go run . -migrate`，由业务进程登记业务模型；其余路径调用 `internal/database`。仅此显式命令触发迁移，不让 new/check/普通启动暗中建表 |
| `token.go` | token add 参数、操作人检查、配置/DB 装配，委派 `admin/tokenadmin` 创建令牌 | 明确写入访问令牌表，库中存哈希、明文仅此处显示一次；表缺失提示 migrate，不顺手建表 |
| `strategy_migrate.go` | inspect 参数、版本化业务 manifest、连接及超时、JSON 输出 | 只读查询委派 `pkg/strategymigrate`；manifest 上限 64 KiB，拒绝未知字段/无效表名；输出文件用 `O_EXCL` 和 0600，不覆盖。没有导入、删表或应用启动模式 |
| `report.go` | 来源/窗口/模式/阈值装配，选择框架分析节与内置模式、日拆分、单 run、列表与退出门禁 | 读取事件、日志和框架 DB；借助 `obs/report` 写四份产物，写前检查目录为输出根的单段子目录；显式 --since/--until/--run 不清理，无这些筛选时清理过期报告。只读指被观测数据，不代表文件系统没有写入和删除；不注册业务报告段 |
| `debug.go` | 调试参数、可选配置、代理/请求头，HTML/Rod 抓取、差异、选择器与输出 | 调用 `pkg/htmlfetch` / `pkg/browser` / proxy；可访问网络、打开浏览器、写输出/截图。Rod 池和借用实例在命令路径内归还/关闭，临时目录配置传到池 |
| `act.go` | 页面动作解析、顺序执行、settle、eval、滚动/点击/输入/等待与人工回车交接 | 使用 debug 创建的 page，不拥有池；动作可能改变页面和远端状态，不能把整个调试命令描述成无副作用读取 |

普通 check 的“不连库”指生成程序所调用的框架校验路径；它仍会编译并导入目标项目的站点包，因此目标包的 init 属于执行代码。boot 更进一步构造应用资源，不能用“没有 Run”声称完全不产生本地文件或连接。

CLI 是短生命周期进程。strategy inspect 显式关闭 `sql.DB` 并取消 30 秒 context；debug 显式管理浏览器池。migrate/token/report 的 GORM 连接在各命令内创建，当前没有统一 CLI 资源容器或逐命令 Close 封装，不应将它们说成 App 的关停流程。长驻运行、HTTP 排空和共享资源所有权见 [Internal 分册的 App 章节](INTERNAL_STRUCTURE.md#app)。

## 3. 模板与生成物归属

模板是公开使用范本；一旦生成到项目，业务代码、配置和应用文档归该业务项目维护。真实站点 URL、固定预期和业务模型应在独立业务模块补充，不进入 Papa 的模板验收数据。

| `templates/` 文件 | 生成目标与职责 |
| --- | --- |
| `main.go.tmpl` | 根 main.go，仅向 internal/app.Run 转交参数 |
| `app_run.go.tmpl` | internal/app/run.go；配置、模型、站点与后台装配、迁移分支、信号和 Run 生命周期 |
| `configs_sites_site.go.tmpl` | internal/sites/<Project>.go；站点/阶段声明与登记范本 |
| `fetch_catalog.go.tmpl` | internal/fetcher/fetch_catalog.go；采集器接口和通用提取范本 |
| `models_models.go.tmpl` | internal/models/models.go；业务模型与模型清单边界，不把结果字段塞进框架任务表 |
| `monitor_register.go.tmpl` | internal/monitor/register.go；业务后台的统一登记入口 |
| `monitor_router.go.tmpl`、`monitor_middleware.go.tmpl` | internal/monitor/router.go、middleware.go；路由与中间件装配 |
| `monitor_tables.go.tmpl` | internal/monitor/tables.go；表格展示声明，模型仍由业务 models 拥有 |
| `monitor_controller_ping.go.tmpl` | internal/monitor/controller/ping.go；HTTP handler 示例 |
| `monitor_view.go.tmpl` | internal/monitor/view/view.go；自定义页、JS/CSS 登记及页面分类示例，默认 data |
| `config.yaml.tmpl` | configs/config.yaml 与 config.yaml.example；配置字段说明及占位凭据，两份共用模板 |
| `go.mod.tmpl`、`gitignore.tmpl` | go.mod、.gitignore；module、本地 replace 及生成物忽略规则 |
| `Makefile.tmpl` | 构建/运行/迁移/测试/容器目标，按 Windows 补可执行文件后缀 |
| `Dockerfile.tmpl`、`docker-compose.yml.tmpl` | deploy/docker/；开发镜像与源码挂载，不是正式业务部署验收 |
| `README.md.tmpl` | 项目根 README.md；项目启动与阅读入口 |
| `docs_README.md.tmpl`、`docs_DEVELOPMENT.md.tmpl` | docs/README.md 与 docs/operations/DEVELOPMENT.md；应用自己的导航和职责说明，区别于框架副本 |
| `skill_SKILL.md.tmpl` | 项目技能 SKILL.md，框架管理并随 upgrade 刷新 |
| `skill_doc-map.md.tmpl`、`skill_implementation-patterns.md.tmpl`、`skill_review-checklist.md.tmpl` | 技能 references 下的文档地图、实现范本和审查清单；框架管理并刷新 |
| `skill_project-conventions.md.tmpl` | 技能 references/project-conventions.md；首次播种后归项目所有，升级只补缺失文件 |

`new` 的普通模式拒绝已存在目标；显式 missing-only 补缺失骨架，同时核对已有 module 是否一致，保留已有 go.mod、配置、业务文件和文档。replace 仅作用于新生成的 go.mod，不追加到已有模块。生成过程是逐文件创建，当前不是整棵目录事务；失败时不能假定此前创建的文件已回滚。

## 4. 框架文档分发链路

`docs/embed.go` 的 `//go:embed *.md package_structure/*.md` 是分发源白名单：嵌入顶层 Markdown 手册及目录职责分册，**排除 issues/、superpowers/ 历史记录**，不递归收进其它文档目录。该 Go 文件本身和历史记录均不是复制给业务的手册文件。

1. `main.go` 的 `tmplFS` 管业务骨架模板，`docs.Files` 管框架手册，两者分开。
2. `upgrade.go` 的 `embeddedDocs` 仅遍历 `docs.Files`，收集 `.md` 相对路径并排序；不会把目标项目的文档扫描结果加入写入清单。
3. `main.go` 的 `copyDocs` 在 new 时按这些相对路径调用 `writeScaffoldFile`，写入新布局 docs/framework/，保留 package_structure/ 子目录，也遵守 missing-only。
4. `upgrade.go` 的 `planDocs` 比对登记手册并生成 DocAdds/DocUpdates；`layout.go` 决定使用 docs/framework/，还是兼容旧项目 docs/。
5. 显式 upgrade write 后，`writeDocs` 只写计划中的框架手册，并创建必要子目录；项目独有文件不删除。顶层项目独有 Markdown 可列入 DocStale，未递归列举的项目子目录也不会因此被清理。

因此新分册放入 package_structure/ 后，通过同一嵌入和复制机制保留相对链接；修改分发目录时需同时检查 embed 白名单、`embeddedDocs`、new/upgrade 的目标路径与回归。应用自己的 docs/README.md、operations/、business/ 属于业务；新版布局不会用框架 README 覆盖应用导航。旧布局保留旧位置，不自动迁移成新版目录。

writeDocs/writeSkill 逐文件直接写入，不是整批事务，也没有临时文件 rename 的原子替换保证；中途失败时，已写文件不会自动回滚。

升级的默认行为始终是报告差异。只有显式 write 才刷新框架登记手册、通用技能及缺失的项目约定技能；不改业务源代码、go.mod、配置、模板生成的应用文档，也不删除旧文档。

## 5. `cmd/monitor-demo` 的边界

| 文件 | 职责与资源归属 |
| --- | --- |
| `main.go` | 解析本地端口，建立信号 context、真实 sysinfo 采集器、Monitor/oao 和 HTTP mux；构造阶段、站点、队列、表格数据及临时报告目录 |
| `oa_demo.go` | 合成订单/操作日志表、演示动作、自定义页面与脚本/CSS 字符串；动作只是演示成功反馈，不实现生产事务 |
| `token_demo.go` | 随机启动令牌、`tokenadmin.MemStore`、auth 校验和 tokenadmin handler；登录与管理共用同一内存存储，修改仅在演示进程内有效 |

演示依赖 `admin/server`、`admin/sysinfo`、`admin/auth`、`admin/tokenadmin`、core/engine 的通用类型及 oao；正式功能页面仍归 `admin/*`，不会将演示页提升为框架默认页。没有独立静态资源目录，演示扩展内容在 Go 字符串中，正式后台资源来自其所属包。

主入口拥有本次临时报告目录并在正常返回时删除；采集器受信号 context 控制。服务默认绑定 `127.0.0.1`，令牌每次随机生成并显示在本机日志。当前使用 ListenAndServe goroutine 和进程退出，没有生产 App 的 HTTP Shutdown/排空链路；这个 demo 的退出路径不能当成正式生命周期范本。演示可读取本地 logs 并采样 downloads/logs 目录，报告是假数据，不证明真实爬取或数据库工作正常。

## 6. 测试分工和新增功能落点

| 测试位置 | 当前分工 |
| --- | --- |
| `main_test.go`、`scaffold_credentials_test.go`、`scaffold_page_sections_test.go` | 生成文件、配置装载、名称/参数、凭据占位和自定义页面分类 |
| `scaffold_safe_test.go`、`scaffold_review_test.go` | 已有项目保护、missing-only、module 不一致、链接/junction、创建竞争、replace 路径 |
| `module_path_test.go`、`module_version_test.go` | 多级/连字符/主版本 module 的实际生成编译；v4 模块、模板、兼容拒绝一致性 |
| `layout_test.go`、`docs_structure_test.go` | 新旧布局、实际 new → tidy → build、应用文档保留；`TestScaffoldAndUpgradePreserveStructureManuals` 守卫分册嵌入、子目录复制和升级 |
| `upgrade_test.go`、`upgrade_helpers_test.go` | 差异计划、只刷新框架副本、通用技能刷新与项目约定保留、辅助输出 |
| `check_test.go`、`migrate_test.go` | 临时程序内容/参数/端到端声明拒绝、业务 module 识别；不等同真实 DB 迁移验收 |
| `debug_test.go`、`act_test.go` | 参数、HTML 提取/链接/选择器/输出和动作语法；解析测试不证明真实浏览器动作已执行 |
| `report_test.go`、`report_mode_test.go` | 日报拆分/保留期、单 run、模式/窗口、判定门禁和自证字段 |
| `token_test.go`、`strategy_migrate_test.go` | 令牌参数/配置路径、只读命令与 manifest 拒绝、不覆盖盘点输出；持久令牌和盘点 SQL 测试归对应功能包 |
| `cmd/monitor-demo/main_test.go` | 合成任务按站点筛选、默认回环监听和随机令牌 |

主版本调整必须同步 module 识别、临时校验源码、模板与文档；应使用 GOWORK=off 验证，并运行真实生成的合成项目 tidy/build/check，避免工作区依赖掩盖错误。`TestRunNewNamespacedModuleBuilds`、`TestScaffoldBuildsAndUpgradePreservesAppDocs`、`TestCheckEndToEnd`、`TestLayoutDetectionAndLegacyUpgrade`、`TestUpgradeWriteOnlyTouchesFrameworkCopies` 分别承担这些边界的已有回归。

新增命令的参数和适配放在职责明确的 cmd/papa 文件，领域算法放其所属库包；新生成物先确定是项目所有还是框架可刷新，再改 main.go/skill.go 清单及对应模板。新增共享布局判据归 layout.go；新增只读报告规则归 [Obs 分册](OBS_STRUCTURE.md)中的组件；新增正式后台功能归 [Admin 分册](ADMIN_STRUCTURE.md)。框架根契约和通用包归属见 [Root 分册](ROOT_STRUCTURE.md)、[Pkg 分册](PKG_STRUCTURE.md)。

本次文档盘点不宣称测试通过。验证应按改动选择 `go test ./cmd/papa ./cmd/monitor-demo` 及相关库包；真实浏览器、数据库和生产部署需要各自的验证证据，不能由模板存在或 demo 截图替代。

report.go 通过有预算的 Context 输入和逐日消费生成报告；文件解码、去重与临时分桶属于 obs，CLI 只负责参数、错误返回与产物输出。输入预检超限保留已有产物；新资源手册随顶层文档自动分发。
