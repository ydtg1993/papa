# Papa CLI 命令手册

> 面向：用 `papa` 命令行排查「页面实际抓到了什么」，以及生成新项目。
> 用途：`papa new` 生成项目骨架；`papa html/rod/diff/select` 是**调试命令**——复用生产配置（`config.yaml` 的 headers / proxy / timeout）单独抓一次页面，看真实结果，不用起整个爬虫。

---

## 0. 安装

当前源码的模块路径为 `github.com/ydtg1993/papa/v4`。安装发布版本使用下方 Go 命令；验证本工作区代码时从仓库构建 CLI，并用 `papa new <name> --replace <本地 Papa 路径>` 指定本地依赖。`@latest` 由模块代理解析，不保证等同于工作区源码。

v4 是独立的新项目契约，不提供旧版项目、配置或数据库迁移兼容。`check`、`upgrade`、`migrate` 及 `new --missing-only` 会在写入或数据库连接前拒绝 go.mod 中未版本化及 /v1、/v2、/v3 的 Papa 模块引用（包括 replace 目标）。第三方 yaml/v3 等独立模块不受影响；`upgrade` 不改业务源码与依赖。

```bash
# 免克隆安装（推荐）：直接装最新版，得到 papa 命令
go install github.com/ydtg1993/papa/v4/cmd/papa@latest

# 已在 papa 仓库内（贡献/调试）：装当前源码
go install ./cmd/papa

# 不想装：在仓库根目录 go run ./cmd/papa <子命令> ...
```

## 1. 命令总览

| 命令 | 作用 |
| --- | --- |
| `papa new <name> [--replace <path>] [--missing-only]` | 生成新爬虫项目骨架（含 `internal/models/` 与 `internal/monitor/`）。默认拒绝已存在的目标；`--missing-only` 显式补齐缺失文件，保留已有文件。`<name>` 同时是 module 路径与目录名，只允许字母、数字与 `._~+-`，分隔用 `/`；非法名字在写文件前拒绝 |
| `papa html <url> [flags]` | 静态 HTML 抓取（htmlfetch，不走浏览器） |
| `papa rod <url> [flags]` | 浏览器渲染后抓取（Rod，走完整 JS 渲染） |
| `papa diff <url> [flags]` | 同一 URL 分别用 html 与 rod 抓取并对比 |
| `papa select <css> <url\|文件> [flags]` | 选择器测试（URL 抓取后查，或本地 html 文件离线查） |
| `papa migrate [-c <配置路径>]` | 建/补表。在业务项目目录里跑会转交项目自己的迁移（`go run . -migrate`，连业务模型一起建），其余情况只建框架自带的表。详见 [CORE_CONFIG.md](./CORE_CONFIG.md) 第 2 节 |
| `papa check [目录] [-c <配置路径>]` | 离线校验全局配置及站点/阶段声明（**不用起库**）：阶段名重复、全局三个治理队列漏写/缺项、熔断阈值、`Delay` 与 `Backoff` 写法 |
| `papa upgrade [目录] [--write]` | 把**已经生成过的**项目升到当前脚手架：刷新框架副本（`docs/framework/` 与 `.claude/skills/` 里的通用文件）+ 几项不用起库的检查。默认只报告，`--write` 才落盘；业务文件一律不碰 |
| `papa token add --operator <名字> [--note "..."] [-c <配置路径>]` | 首次部署或无可用令牌时在服务器本机创建凭据（明文只打印一次；管理 API 默认拒绝匿名），详见 [MONITOR.md](./MONITOR.md) 第 1 节 |
| `papa report generate [-c <配置路径>] [--since/--until] [--run] [--snapshot 或 --acceptance] [--out] [--no-logs] [--fail-on-verdict]` | **只读**生成运行报告（`report.md`、`report.json`、`ai-brief.md`、`ai-brief.json`）：默认运行快照，按本地日历日拆分有事件的每天；`--acceptance` 显式验收，`--snapshot` 只生成最后一天，两者互斥且优先于配置；无时间/run 筛选时生成后按 `obs.report.retention` 清理过期目录；显式筛选不清理。业务段由业务自己的离线 obs.Generate 命令提供，papa CLI 只有框架通用段 |

### `papa new` 生成什么

已有 v4 手工业务模块需要补齐骨架时，在父目录执行 `papa new <原模块名> --missing-only`。已有 go.mod 的 module 必须与项目名一致；现有配置、代码、文档、技能与 go.mod 均保持原样，`--replace` 只作用于本次新建的 go.mod。补齐不会合并代码或解析已有配置；新补的站点和入口仍是模板，需要按业务审阅后再启动。文件独占创建，链接路径拒绝写入；中途失败时已新建的文件保留，可修正后再次补齐。升级通用文档/技能使用 `papa upgrade --write`，不以 new 覆盖业务。

路径保护包括 Windows junction 与异常 reparse point，不能通过目录联接写入其它位置。`--replace` 必须跟一个非空本地路径，不能把后面的选项当成路径；路径含空格时，在 shell 中将参数加引号，例如 `--replace "../local papa"`，CLI 会生成合法的 go.mod 引号。相对 replace 路径以生成项目的 go.mod 所在目录为基准。

完整 module 路径用于 `go.mod` 与 Go import；模块最后一段作为本地项目名，用于站点文件、默认站点键、构建产物和 Compose 服务/容器名称。例如 `papa new example.com/demo` 在 `example.com/demo/` 下生成项目，站点为 `internal/sites/demo.go`，产物为 `bin/demo`（Windows 为 `demo.exe`），import 仍是 `example.com/demo/internal/...`。`papa new example.com/demo/v2` 的本地项目名为 `v2`。已有项目不会被自动改名或迁移。

```
mycrawler/
  main.go                  薄入口：app.Run(os.Args[1:])
  internal/
    app/run.go             CLI、模型注册、迁移分支、后台及站点组装
    sites/<项目名>.go      站点与阶段声明，init 自登记
    fetcher/               抓取与入口提交
    models/models.go       业务模型与迁移清单
    monitor/               tables、router、middleware、controller、view
  configs/                 config.yaml（本机）、config.yaml.example（范本）
  deploy/docker/           Dockerfile、docker-compose.yml
  docs/
    README.md              应用文档导航（项目所有）
    operations/DEVELOPMENT.md  应用目录与开发说明（项目所有）
    framework/             Papa 手册副本（upgrade 可刷新）
      package_structure/   Papa 目录说明书：总入口 + 七份职责分册
  README.md · Makefile · logs/ · .gitignore
  .claude/skills/          开发技能（通用文件可刷新，项目约定不覆盖）
```

后台自定义页模板 `internal/monitor/view/view.go` 显式演示 `Page.Section: "data"`。可按业务改为 flow/monitor/system；不填写仍回退数据与资源，Group 只在类内分组。`upgrade` 不改已有业务注册。

框架目录说明书见 [package_structure/README.md](package_structure/README.md)，包含根目录、Admin、Engine、Internal、Pkg、Obs、Cmd 分册；App 职责并入 Internal。新建与升级保留这些相对路径。历史 issues/superpowers 不分发；升级不会删除项目自行保留的文档副本。

几句要点：

- **加一个站 = 加一个 `internal/sites/<站名>.go` 文件**：它在自己的 `init()` 里自登记，框架用
  `papa.Sites()` 收集，`internal/app/run.go` 不用动（站点键必填；阶段名只在本站内唯一，两个站可以同名）。
- **阶段结构/物理容量在 Go 声明里**：`runtime` 可覆盖已有阶段的有效执行并发、固定任务间隔与账号池；
  `crawler.stages` 仍是未知键。模板默认关闭请求自适应、资源与代理健康，字段与覆盖优先级见 [RUNTIME_CONTROL.md](RUNTIME_CONTROL.md)。
- **三个治理队列（error_queue / recover_queue / repeat_queue）在 `config.yaml` 里全局配**：
  没写哪段 = 那条不跑；开了（`enabled: true`）就得写全，缺一项 / 越界启动就报。
- **`.claude/skills/papa-crawler-development/` 带一份开发技能**：Claude Code 会从 `<项目>/.claude/skills/`
  自动加载它，所以新项目开箱就有「站点怎么加、任务怎么派、后台表格怎么写、审计怎么查」的现成指导，
  不用先去翻手册。`.gitignore` 特意放行这一目录（`.claude/` 下的**其余**条目仍忽略）；
  它由 `papa new` 写入；`papa upgrade` 会刷新其中的**通用文件**（`SKILL.md` 与 `references/` 下那三份），
  而 `references/project-conventions.md` **只在缺失时播种、之后不覆盖** —— 它属于你的项目
  （记本项目的布局与「已知漂移」），由你自己维护。
- 生成后：`go mod tidy` → 改 `db.dsn` → `papa migrate`（或 `make migrate`）建表 → `go run .`。

业务自行管理凭据文件和忽略规则，`upgrade` 不覆盖已有 `.gitignore`。

`papa new` 生成的本机配置和 example 配置使用 `user:CHANGE_ME` 数据库凭据占位；请先在本机填真实连接，再执行 `papa migrate`。已有项目的 `papa upgrade --write` 只刷新框架手册/通用技能，不替换已配置的 DSN；`--missing-only` 同样保留现有配置。

运行治理升级仍使用现有 `papa migrate`，没有新的隐式启动迁移命令。它补齐 `crawler_config.revision`、`crawler_tasks.account_pool` 及账号池/账号管理表；普通启动会检查结构和持久策略，不在缺列时回退默认运行。`papa upgrade --write` 分发包括 `RUNTIME_CONTROL.md` 的框架手册，但不会替业务修改 runtime 配置、注册账号或启动采集。YAML 时长写字符串，OA API 的 duration 用整数纳秒。

### `papa check` 校验声明（不用起库）

```bash
papa check [目录] [-c <配置路径>]              # 目录默认当前目录；配置默认 <目录>/configs/config.yaml
papa check [目录] --boot                       # 再往前一步：连库跑到 RegisterSites（不 Run）
```

站点与阶段声明是**项目自己的 Go 代码**（`internal/sites/*.go` 在 `init()` 里自登记），而框架那套
「声明有问题启动即失败」的校验平时要连库、建池才跑得到。`papa check` 把这两件事解开：

- 它在你项目里临时生成一个 **一次性校验程序**（`<目录>/.papa-check-<随机后缀>/main.go`，
  目录名唯一、只删自己建的那个），`go run` 一次，跑完立刻删掉 —— 只有编译进项目才读得到那些声明
  （CLI 是另一个模块，看不见项目的包）；
- 那个程序只调框架的**纯校验** `papa.CheckSites`，**不连库、不建池、不改任何状态**。

脚手架的 `.gitignore` 忽略 `.papa-check-*/` 临时校验目录；正常退出清理本次目录，异常终止可能留下临时文件。

查的是 `RegisterSites` 会在启动时 panic 的那些事，而且**一次报全**（不是报第一条就停）：

```
站点 b 的第 2 个阶段: 阶段 "same" 在本站重复声明（站点 b 已经声明过）—— 同一站内阶段名必须唯一…
```

> 它会顺带把 `config.yaml` 也读一遍（框架的键与值域校验，同样不用连库）；配置不在默认位置就 `-c` 指定。

**`--boot`：连库把"注册那条链"也跑一遍。** 上面那版是纯校验（不碰库）；`--boot` 会在同一个一次性
程序里接着走真的启动路径 —— `papa.New()`（读配置 + 连库 + 建引擎）→ `app.RegisterSites(papa.Sites()...)`，
**到此为止**：不 `Run`，所以不投入口任务、不跑启动恢复、不开 HTTP 服务、也不抓任何页面。
声明没问题、库也通，它才会打印：

```
✓ 注册通过：1 个站点、3 个阶段（连库跑的是启动路径前半段，没有 Run：不投入口任务、不跑启动恢复、不开 HTTP 服务、不抓任何页面）
    example：catalog / detail / episode
```

它同时校验"声明与引擎接线对得上"（每个声明的站点都得能在 `app.Engine.Site(key)` 里查到）。
要连库，所以比不带 `--boot` 的那版多一层前提：库得连得上。
>
> 首次在**刚生成**的项目里跑，可能会提示先 `go mod tidy` —— 脚手架生成的 `go.mod` 本来就不带
> `require`（那是 `tidy` 的事），校验程序要编译就得先把依赖落下来。

### `papa upgrade` 刷新已有 v4 项目

仅刷新 v4 项目的 `docs/framework/` 与通用技能，保留应用 `docs/README.md` 与业务/运维文档。没有旧 `docs/` 布局回退；站点声明由 `papa check` 编译 `internal/sites/` 校验。

```bash
papa upgrade [目录] [--write]      # 目录默认当前目录
```

`papa new` 只管"从零生成"；已经落地的项目用它升级。**默认只报告不落盘**，加 `--write` 才写。

它只做框架**有把握**的两件事：

1. **刷新框架副本**：两处，都以框架为准（按"新增 / 更新"写回；具体数量随版本变化）。
   - `docs/framework/` 中由框架嵌入的手册按相对路径刷新，包括 `package_structure/` 分册。该目录顶层的项目独有 `.md` 会列出但不修改；项目独有子目录不递归列举，也不清理。
   - `.claude/skills/papa-crawler-development/` 里的**通用文件**（`SKILL.md` 与 `references/` 下那三份）。
     `references/project-conventions.md` 是唯一的例外：它记的是**你这个项目**的布局与已知漂移，
     **只在缺失时播种，存在就一律不碰**（与 `main.go` 那类业务文件同一个待遇）。缺失技能目录的
     v4 项目，跑一次就补齐整套。
   （比内容时忽略行尾：生成物在不同 git 配置下会是 CRLF，那不是"你改过"。）
2. **配置检查**：加载 `configs/config.yaml`，报告未知键、字段和值域错误，不连接数据库。站点/阶段声明使用 `papa check` 校验，upgrade 不再分析旧 API 类型或提供迁移提示。

**业务文件一律不碰**：`main.go` / `internal/monitor/` / `internal/models/` / `internal/fetcher/` / `internal/sites/` / `configs/config.yaml` /
`go.mod` 都不会被覆盖 —— 生成物一旦落地就归项目所有，照模板覆盖等于抹掉你的改动。它们该改什么，
报告最后给一张清单（含从 `go.mod` 读出的框架版本与 `replace` 目标）。

> 框架版本还没发布时，`go.mod` 里用 `replace github.com/ydtg1993/papa/v4 => <本地 papa 仓库>`
> 先跑起来（`papa new --replace` 加的就是这行）；发布后改成 require 一个新版本号即可。
>
> 全局配置或站点/阶段声明改完（尤其是启用后要求字段齐全的三个治理队列）跑一次 **`papa check`**：它不用起库就能告诉你漏没漏，
> 不必等下次启动才发现；要连库把启动路径也走一遍，就加 **`--boot`**。

### `papa report` 生成运行报告（只读）

```
papa report generate [-c configs/config.yaml] [--since RFC3339] [--until RFC3339] \
                     [--run <run_id>] [--snapshot | --acceptance] [--out <dir>] [--no-logs] [--fail-on-verdict]
papa report list [--out <dir>]
```

- 数据源：`log.dir/events.jsonl`（含轮转备份与 `.gz`，见 [CORE_CONFIG.md](./CORE_CONFIG.md) 的 `obs.events`）+ `log.dir/*.log` 的证据索引 + `crawler_tasks` 状态。
- 产物：默认按**本地日历日拆分**，`<obs.report.dir 或 --out>/<2006-01-02>/` 下 4 个文件：`report.md`（人读）、`report.json`（机读指标）、`ai-brief.md`（AI 排查细则）、`ai-brief.json`（同构机读）；同一天重跑刷新当天那份。`--run <run_id>` 是例外：只出单份 `<obs.report.dir 或 --out>/<run_id>/`，不按天拆。无时间/run 筛选时，生成后按 `obs.report.retention`（默认 7 天，负数 = 永久保留）清理过期目录，并打印「清理过期报告 N 份」。CLI 指定 `--since`、`--until` 或 `--run` 时只生成、不执行保留清理，保证显式历史产物可以取走；无这些筛选的 CLI 生成仍按离线保留期清理；在线另采用有限目录/已知产物清理。历史产物之后仍可能被自动清理，需要长期保存时请使用独立 `--out` 目录。特殊 run_id（如 `..`、Windows 设备名）归一为安全单段目录名；正常 run_id 命名不变。
- 模式：默认按 `obs.report.mode`，`auto` 在本离线命令中为 `snapshot` 运行报告；在线固定 snapshot；配置明确写 `acceptance` 仍使用验收口径。`--acceptance` 强制检查「这轮是否通过」，`--snapshot` 强制运行快照，两者互斥，均优先于配置。默认有事件的每个本地日各生成一份；只有显式 `--snapshot` 保留单日用法（缺省取最后一个事件所在日；papa CLI 显式窗口跨本地日会报错）。`--run` 仍只生成单次运行报告。模式写在 `report.md` 头部和 `report.json` 的 `mode`。
- 判定：任一 `fail` 级检查未通过 → 不通过；只有 `warn` 未通过 → 带风险通过；一个可判定的检查都没有 / 数据降级（`degraded` 非空）全过 / 暂停快照全过 → **未判定**（`unknown`）；否则通过。`--fail-on-verdict` 让**任一日报**「不通过」**或「未判定」**时以非零码退出（CI / 自动化门禁；`warn` 仍放行）。
- 只读：不写业务表、不重投任务、不调用接收端。
- 业务报告段由业务自己的离线命令通过 `obs.Generate` 显式提供；App 在线 Section/Pattern 注册入口已移除，Papa CLI 不包含业务模型。离线允许有界读取历史和查库对账，在线服务不执行这些操作。

## 2. 通用 flags（html / rod / diff 共用）

| Flag | 说明 |
| --- | --- |
| `-c, --config <path>` | 复用项目配置；回退链 `PAPA_CONFIG` → `configs/config.yaml`。**可缺省**：零配置也能跑（用内置默认 UA）。**但显式给的那份读不了 / 解析不了会直接报错** —— 不静默退回零配置（那会表现为"请求头/超时/代理全不生效，还一声不吭"） |
| `--proxy` | 走代理（读 `config.proxy`）；**默认直连**，避免调试时烧代理配额 |
| `--timeout <duration>` | 覆盖请求/导航超时 |
| `--ua <string>` | 覆盖 User-Agent |
| `--header "k=v"` | 追加/覆盖请求头（可重复） |
| `--json` | 输出结构化 JSON（见第 5 节） |
| `-o, --output <file>` | HTML 落盘到文件 |

> flag 可放在 URL 前后任意位置，如 `papa html https://x --json` 或 `papa html --json https://x` 都行。

## 3. 输出形态（html / rod，选一个，默认输出 HTML）

| Flag | 输出 |
| --- | --- |
| （默认） | HTML 原文 |
| `--links` | 所有 `<a href>`（按最终 URL 绝对化，一行一个） |
| `--text` | `<body>` 可读文本 |
| `--select <css>` | 选择器匹配节点的文本（`序号\t文本`） |

## 4. rod 专属 flags

| Flag | 说明 |
| --- | --- |
| `--screenshot <file>` | 保存 PNG 截图 |
| `--full-page` | 整页截图（配合 `--screenshot`） |
| `--wait <duration>` | 加载完成后额外等待，暴露懒加载/异步渲染内容 |
| `--act <动作>:<参数>` | 页面动作，可重复，按序执行（见下） |
| `--show` | 有头浏览器 + 人工操作，回车后导出结果 |
| `--devtools` | 同 `--show`，并自动打开 DevTools |

> `rod` 的 headless 取自 `config.browser.headless`；**无配置文件时默认 headless**（不弹窗）。
> `--show` / `--devtools` 强制有头。
> 浏览器来源与启动参数同样取自配置（`browser.binary` / `browser_path` / `browser.flags`），
> 与生产抓取走的是同一套解析；`browser.self_check` 在调试命令里不生效（马上就启动，不再自检一遍）。

### `--act` 动作

静态抓取永远只能拿到首屏。涉及下拉加载、点「加载更多」、切 Tab 的页面，用 `--act` 把交互写进命令：

| 动作 | 说明 |
| --- | --- |
| `scroll:bottom` | 滚到文档底部 |
| `scroll:top` | 回到顶部 |
| `scroll:<N>` | 向下滚 N 屏（`scroll:3`） |
| `scroll:<css>` | 滚动到该元素可见 |
| `click:<css>` | 点击元素（自动先滚入视口） |
| `input:<css>=<文本>` | 清空并输入文本 |
| `hover:<css>` | 悬停 |
| `wait:<duration>` | 固定等待（`wait:2s`） |
| `wait:<css>` | 等到元素出现 |
| `eval:<js>` | 在页面上下文执行任意 JS，返回值打到 stderr（`eval => ...`） |

> `wait:` 的参数能解析成时长就是等待，否则按 CSS 选择器处理——合法选择器不可能是合法时长，不会歧义。
>
> `eval:` 收的是**表达式或语句**（`document.title`、`document.title='x'`、`document.querySelectorAll('.item').length` 都行），
> 不是函数——内部会包一层 `eval()` 求值，返回值打到 stderr。页面 CSP 禁止 `eval` 时会报错。
>
> `--timeout` 同时作用于导航与**每个动作**（等元素、点击等），动作卡住不会无限等待。
> **不传时两者默认值不同**：导航 30s、单个动作 15s（`cmd/papa/act.go` 的 `actDefaultTimeout`）——
> 传了 `--timeout` 就统一按它来。

**每个动作结束后会自动等页面稳定**（DOM 不再变化、网络不再请求，最多等 5 秒，超时只告警不中断），
避免下拉完立刻取值拿到半截数据。有些页面（轮播、时钟）永不静止，这时每个动作会固定耗满 5 秒，
可在动作后补 `--act wait:2s` 并接受告警。

`--act` 顺序执行，任一动作失败立即报错退出（不会静默跳过），并带上是第几个动作失败。

**无限滚动的页面**一次 `scroll:bottom` 往往只加载一屏，重复写多次；`scroll:3` 也行：

```bash
# 下拉三次触发懒加载，再取所有条目标题
papa rod "https://example.com/list" \
  --act scroll:bottom --act wait:1s --act scroll:bottom --act wait:1s --act scroll:bottom \
  --select ".item-title"

# 点「加载更多」按钮两次
papa rod "https://example.com/list" --act click:.load-more --act wait:.item:nth-child(21)

# 搜索框输入后查询
papa rod "https://example.com" --act input:#kw=火影 --act click:.search-btn --act wait:.result

# 页面上滚到某个区块并截图
papa rod "https://example.com" --act scroll:#comments --screenshot comments.png

# 直接取值：看列表实际渲染了多少条
papa rod "https://example.com/list" --act scroll:bottom --act "eval:document.querySelectorAll('.item').length"
```

### `--show` 人工操作

调反爬、验证「到底要点哪里」时，最省事的是让浏览器开着、自己点，点完再导出：

```bash
papa rod "https://example.com/detail/1" --show --select ".episode-list"
papa rod "https://example.com" --devtools --screenshot page.png
```

流程：打开有头浏览器 → 你在窗口里操作（下拉、点击、登录都可以）→ 回终端按回车 →
命令按 `--select` / `--text` / `--links` / `--screenshot` 导出**当前**页面状态。

`--show` 可与 `--act` 组合：先自动跑一遍动作，再交给你手动补充。

> stdin 不是终端（管道、重定向）时不会等待回车，直接导出，避免脚本里卡住。

## 5. JSON 输出结构

**`--json`（html / rod，默认 HTML 形态）**：

```json
{
  "engine": "html",          // 或 "rod"
  "input_url": "https://...",
  "final_url": "https://...", // 重定向后
  "status": 200,
  "content_type": "text/html",
  "title": "Example Domain",
  "html_len": 12732,
  "elapsed_ms": 309
}
```

**`--json` + `--links` / `--text` / `--select`** → 包一层 meta：

```json
{ "meta": { "...上述字段..." }, "items": ["https://...", "..."] }
```

**`diff --json`**：

```json
{
  "input_url": "https://...",
  "html": { "status": 200, "final_url": "...", "html_len": 713 },
  "rod":  { "status": 200, "final_url": "...", "html_len": 12732 },
  "html_equal": false,
  "links": { "html_only": [], "rod_only": [], "common": 1 }
}
```

**`select --json`**：

```json
{ "selector": "a", "count": 2, "items": ["第一项", "第二项"] }
```

## 6. 典型排查场景

```bash
# 看某页静态 HTML 到底拿到了啥
papa html "https://example.com/list" --json

# 看浏览器渲染后的 HTML（JS 动态内容）
papa rod "https://example.com/detail/1" -o rendered.html

# 对比「静态 vs 渲染」——判断这页到底需不需要浏览器
papa diff "https://example.com/list" --json

# 测一个选择器还能不能匹配到内容（本地 html 文件离线反复调）
papa html "https://example.com/list" -o page.html
papa select ".item a" page.html

# 截图看反爬拦截页 / 验证码
papa rod "https://example.com" --screenshot shot.png --full-page --wait 3s

# 复用生产代理 + 自定义 UA
papa rod "https://example.com" --proxy --ua "Mozilla/5.0 ..." --json
```

## 7. 说明

- **零配置**：不指定 `--config` 且无 `configs/config.yaml` 时也能跑，用内置默认请求头；`rod` 默认 headless。
- **退出码**：0 = 成功，1 = 抓取失败，便于脚本串联。
  **注意 `rod` 对非 2xx 不算失败**：浏览器能打开 404 页面，它只把状态码记进 JSON 的 `status` 字段，
  进程仍退 0；`html` / `select`（走 htmlfetch）对非 2xx 会报错退 1。要在脚本里判状态码，读 JSON 的 `status`。
- **`--header` 格式**：`--header "User-Agent=xxx"`（`key=value`），可多次传入。
- **`-o` 与 `--json`**：`-o` 把 HTML 落盘；带 `--json` 时 JSON 走 stdout、HTML 落盘。


### debug 的出口授权

`papa html`、`papa rod`、`papa diff` 和在线 `papa select` 都使用配置中的 `network.private_origins`。测试内网服务前，在业务自己的配置中填完整 origin；代理列表 API 的内网地址也须列入。不会因为调试命令、localhost 或使用代理而自动放行。`papa check`/配置加载会拒绝畸形授权，详见 CORE_CONFIG 的网络出口段。本次没有数据库迁移。

Trace 采用文件存储，不需要运行 migrate 建 Trace 表；已有 SQL Trace 表不删除、不读取。新建项目模板提供 crawler.trace_files 有限预算配置。
