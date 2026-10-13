# Papa CLI 命令手册

> 面向：用 `papa` 命令行排查「页面实际抓到了什么」，以及生成新项目。
> 用途：`papa new` 生成项目骨架；`papa html/rod/diff/select` 是**调试命令**——复用生产配置（`config.yaml` 的 headers / proxy / timeout）单独抓一次页面，看真实结果，不用起整个爬虫。

---

## 0. 安装

```bash
# 免克隆安装（推荐）：直接装最新版，得到 papa 命令
go install github.com/ydtg1993/papa/v3/cmd/papa@latest

# 已在 papa 仓库内（贡献/调试）：装当前源码
go install ./cmd/papa

# 不想装：在仓库根目录 go run ./cmd/papa <子命令> ...
```

## 1. 命令总览

| 命令 | 作用 |
| --- | --- |
| `papa new <name> [--replace <path>]` | 生成新爬虫项目骨架（含根 `models/` 数据层与 `monitor/` 后台分层包，见 [MONITOR.md](./MONITOR.md) 第 5 节） |
| `papa html <url> [flags]` | 静态 HTML 抓取（htmlfetch，不走浏览器） |
| `papa rod <url> [flags]` | 浏览器渲染后抓取（Rod，走完整 JS 渲染） |
| `papa diff <url> [flags]` | 同一 URL 分别用 html 与 rod 抓取并对比 |
| `papa select <css> <url\|文件> [flags]` | 选择器测试（URL 抓取后查，或本地 html 文件离线查） |
| `papa migrate [-c <配置路径>]` | 建/补表。在业务项目目录里跑会转交项目自己的迁移（`go run . -migrate`，连业务模型一起建），其余情况只建框架自带的表。详见 [CORE_CONFIG.md](./CORE_CONFIG.md) 第 2 节 |
| `papa check [目录] [-c <配置路径>]` | 离线校验全局配置及站点/阶段声明（**不用起库**）：阶段名重复、全局三个治理队列漏写/缺项、熔断阈值、`Delay` 与 `Backoff` 写法 |
| `papa upgrade [目录] [--write]` | 把**已经生成过的**项目升到当前脚手架：刷新框架副本（`docs/` 与 `.claude/skills/` 里的通用文件）+ 几项不用起库的检查。默认只报告，`--write` 才落盘；业务文件一律不碰 |
| `papa token add --operator <名字> [--note "..."] [-c <配置路径>]` | 建一把后台访问令牌（明文只打印一次），详见 [MONITOR.md](./MONITOR.md) 第 1 节 |

### `papa new` 生成什么

```
mycrawler/
  main.go                  装配：monitor.Register → （可选）SetProxy → RegisterSites(papa.Sites()...) → Run
                           （下载器不在这里配：框架按 DefaultConfig 备好基座，要各站/各阶段不同就写在站点声明里）
  configs/
    config.yaml            配置（先把 db.dsn 指向你的库）
    whitelist              后台来源白名单（全注释＝不限制，启动会警告）
    sites/<项目名>.go      ★站点与阶段声明：Key / BaseURL / Headers
                            + 每个阶段的 worker_count / queue_size / delay / retry / AutoStart
  fetcher/                 抓取实现（一个阶段一个 fetcher；想按站点分包也行，加站不必先分包）
  models/                  业务表模型 + 建表清单（models.go）
  monitor/                 后台分层：表格页 / 控制器 / 视图 / 路由与中间件（见 MONITOR.md 第 5 节）
  docs/                    这套手册的副本（离线可查）
  .claude/skills/          开发技能（Claude Code 从 <项目>/.claude/skills/ 自动加载，见下面要点）
  docker/ · Makefile · logs/ · .gitignore
```

几句要点：

- **加一个站 = 加一个 `configs/sites/<站名>.go` 文件**：它在自己的 `init()` 里自登记，框架用
  `papa.Sites()` 收集，`main.go` 不用动（站点键必填；阶段名只在本站内唯一，两个站可以同名）。
- **阶段参数不在 `config.yaml` 里**：都写在那份声明（Go 里一份写全，加阶段不用改两个地方）；
  yaml 里留 `crawler.stages` 段会被"未知配置键"拦下。
- **三个治理队列（error_queue / recover_queue / repeat_queue）在 `config.yaml` 里全局配**：
  没写哪段 = 那条不跑；开了（`enabled: true`）就得写全，缺一项 / 越界启动就报。
- **`.claude/skills/papa-crawler-development/` 带一份开发技能**：Claude Code 会从 `<项目>/.claude/skills/`
  自动加载它，所以新项目开箱就有「站点怎么加、任务怎么派、后台表格怎么写、审计怎么查」的现成指导，
  不用先去翻手册。`.gitignore` 特意放行这一目录（`.claude/` 下的**其余**条目仍忽略）；
  它由 `papa new` 写入；`papa upgrade` 会刷新其中的**通用文件**（`SKILL.md` 与 `references/` 下那三份），
  而 `references/project-conventions.md` **只在缺失时播种、之后不覆盖** —— 它属于你的项目
  （记本项目的布局与「已知漂移」），由你自己维护。
- 生成后：`go mod tidy` → 改 `db.dsn` → `papa migrate`（或 `make migrate`）建表 → `go run .`。

### `papa check` 校验声明（不用起库）

```bash
papa check [目录] [-c <配置路径>]              # 目录默认当前目录；配置默认 <目录>/configs/config.yaml
papa check [目录] --boot                       # 再往前一步：连库跑到 RegisterSites（不 Run）
```

站点与阶段声明是**项目自己的 Go 代码**（`configs/sites/*.go` 在 `init()` 里自登记），而框架那套
「声明有问题启动即失败」的校验平时要连库、建池才跑得到。`papa check` 把这两件事解开：

- 它在你项目里临时生成一个 **一次性校验程序**（`<目录>/.papa-check/main.go`），`go run` 一次，
  跑完立刻删掉 —— 只有编译进项目才读得到那些声明（CLI 是另一个模块，看不见项目的包）；
- 那个程序只调框架的**纯校验** `papa.CheckSites`，**不连库、不建池、不改任何状态**。

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
    huangguo：catalog / detail / episode
```

它同时校验"声明与引擎接线对得上"（每个声明的站点都得能在 `app.Engine.Site(key)` 里查到）。
要连库，所以比不带 `--boot` 的那版多一层前提：库得连得上。
>
> 首次在**刚生成**的项目里跑，可能会提示先 `go mod tidy` —— 脚手架生成的 `go.mod` 本来就不带
> `require`（那是 `tidy` 的事），校验程序要编译就得先把依赖落下来。

### `papa upgrade` 升级已有项目

```bash
papa upgrade [目录] [--write]      # 目录默认当前目录
```

`papa new` 只管"从零生成"；已经落地的项目用它升级。**默认只报告不落盘**，加 `--write` 才写。

它只做框架**有把握**的两件事：

1. **刷新框架副本**：两处，都以框架为准（按"新增 / 更新"写回；具体数量随版本变化）。
   - `docs/` 根目录下由框架嵌入的使用手册是生成时逐字复制的。你自己加在 `docs/` 里的 `.md`、以及子目录
     （如 `docs/samples/`）只列出来，**不删不改**。
   - `.claude/skills/papa-crawler-development/` 里的**通用文件**（`SKILL.md` 与 `references/` 下那三份）。
     `references/project-conventions.md` 是唯一的例外：它记的是**你这个项目**的布局与已知漂移，
     **只在缺失时播种，存在就一律不碰**（与 `main.go` 那类业务文件同一个待遇）。生成时还没有这个目录的
     老项目，跑一次就补齐整套。
   （比内容时忽略行尾：生成物在不同 git 配置下会是 CRLF，那不是"你改过"。）
2. **几项不用起库的检查**：
   - **`configs/config.yaml` 能不能被框架读** —— 升级最常见的坑就在这儿：旧版本留下的键现在会被
     「未知配置键一律拒绝启动」拦下，而**报错自带"搬去哪儿"的指路**。这条不需要连数据库，
     `papa upgrade` 直接把它跑一遍给你看（就是启动时那条 panic 的原文）。
   - **`configs/sites/*.go` 的可疑点**（文本级提醒，不解析）：还在用改过名的旧类型，或把已经并回 `config.yaml` 全局段的三个治理队列继续写在站点声明里。

**业务文件一律不碰**：`main.go` / `monitor/` / `models/` / `fetcher/` / `configs/sites/` / `config.yaml` /
`go.mod` 都不会被覆盖 —— 生成物一旦落地就归项目所有，照模板覆盖等于抹掉你的改动。它们该改什么，
报告最后给一张清单（含从 `go.mod` 读出的框架版本与 `replace` 目标）。

例子（升到「三队列并回 config.yaml 全局段」那一版时，站点声明里还留着旧类型名的实际输出节选）：

```
【站点声明】
  ！站点声明里还有 ErrorQueueSpec / RecoverQueueSpec / RepeatQueueSpec —— 三个治理队列已回
     config.yaml 的全局段（error_queue / recover_queue / repeat_queue），删掉站点声明里的这些旧类型；
     见 docs/CORE_CONFIG.md
```

> 框架版本还没发布时，`go.mod` 里用 `replace github.com/ydtg1993/papa/v3 => <本地 papa 仓库>`
> 先跑起来（`papa new --replace` 加的就是这行）；发布后改成 require 一个新版本号即可。
>
> 全局配置或站点/阶段声明改完（尤其是启用后要求字段齐全的三个治理队列）跑一次 **`papa check`**：它不用起库就能告诉你漏没漏，
> 不必等下次启动才发现；要连库把启动路径也走一遍，就加 **`--boot`**。

## 2. 通用 flags（html / rod / diff 共用）

| Flag | 说明 |
| --- | --- |
| `-c, --config <path>` | 复用项目配置；回退链 `PAPA_CONFIG` → `configs/config.yaml`。**可缺省**：零配置也能跑（用内置默认 UA） |
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
