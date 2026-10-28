# Papa 开发工具手册

> 面向：构建 / 测试 / 竞态检测等日常开发操作。
> 配套：各模块手册见 [README.md](./README.md) 的「阅读路径」。

---

## 0. 一句话

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
papa.go · task.go · errors.go   门面：全是 type/var 别名，没有实现
core/                           零依赖叶子包：跨包流动的纯值 + 不依赖任何内部包的纯函数
                                （告警 / 错误分类 / 监控快照 DTO、ctx 携带的请求头与代理、URL 解析与同站判定、
                                页面身份与任务幂等键 identity.go、站点地址回钉 siteurl.go）
config/                          配置结构与校验（纯数据，不依赖任何内部包）
engine/                          引擎：Task · Trace · Fetcher · Engine
  engine.go    结构体 + 生命周期 + 依赖注入 + 结果读写
  task.go      Task 结构与状态更新（Insert / UpdateStatus / IncrRetry / Unique）
               + 两个 fetcher 接口：Fetcher（GetStage / FetchHandler）与 EntrySubmitter（可选的入口任务）
  stage.go     阶段装配与 worker 循环（ApplyRegisterStage / runAttempt）
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
  auth/         访问令牌校验与操作人身份
  tokenadmin/   令牌 CRUD 接口
  oplog/        操作日志（异步写入器 + 只读表格页）
  tasksource/   内置「任务」表，动作接线的参考实现
  gormsource/   配置驱动的 oao.Source，业务表格页直接复用
  configstore/  运行期配置存储（crawler_config 表）+ 读缓存层（内存 / TTL 装饰器）
  sysinfo/      机器指标 / 目录占用采集
internal/                       真正的内部件：不对外承诺 API，**模块外也 import 不到**
  workerpool/ database/ msgqueue/ track/ breaker/
  app/                          装配层：把上面这些接成 App（门面 papa.App 指向它）
pkg/                             可独立复用的件
  events/                        结构化运行事件：契约（event.go）+ 异步 emitter + JSONL sink
  browser/                       池与页面生命周期（pool.go / rod.go）、网络请求捕获（capture.go）、
                                 等待条件（wait.go）、按页资源拦截（block.go）
  htmlfetch/ loggers/ notify/ middleware/{proxy,m3u8,filedown}
obs/                             运行报告引擎（通用、只读）：obs.go 契约 / load.go 三层装载 /
                                 eventlog/ 事件读取 / logidx/ 日志证据 / analyze/ 通用分析节 /
                                 pattern/ 内置模式库 / report/ 4 产物渲染
models/                          框架自带的表结构
cmd/papa/                        CLI（new / check / upgrade / migrate / token / html / rod / diff / select / report）
```

> **`admin/` 与 `internal/` 的区别就一条**：Go 规定 `internal/` 下的包只能被 `internal/` 的
> 父目录（也就是本模块）内的代码 import。所以业务项目**能** import `admin/gormsource`，
> **不能** import `internal/workerpool` —— 后者是框架自己的实现细节，改了不算破坏性变更。

**依赖方向**（箭头 = import 方向，不允许反向）：

```
papa 门面 ──► internal/app ──► engine ──► internal/{workerpool,database,track,breaker}
                 │               │              pkg/{browser,htmlfetch,loggers,middleware}
                 │               └──► core ◄── config
                 └──► admin/{server,auth,tokenadmin,oplog,tasksource,sysinfo,...} ──► core
                         └──► tasksource/oplog 还依赖 admin/gormsource
pkg/notify ──► core
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
| 落盘 | 页面归档按 `{dir}/{site}/{stage}/` 分两层（同名阶段不再混在一起） | `engine/archive.go` 的 `relPath` |

### 全局，且**有意**如此（别想着分站）

- **进程级**：`crawler.stop_timeout`（停机怎么等）、`crawler.drain_interval`（溢出回灌跳多久一次）、
  `server.*`（HTTP 服务/白名单/采样间隔）、`db.*`、`log.*` —— 一个进程只有一份。
- **客户端级**：`html.*` / `browser.*`（超时、体积上限、空闲回收、全局 headers）。站点要自己的头就用
  `SiteSpec.Headers`（同键覆盖全局那层）；单次请求还能 `papa.WithHeaders`。
- **代理出口**：框架只提供 `engine.NextProxy()` 这个"随用随取"，**不替业务决定**用哪个 ——
  按站点分流出口是业务自己的事（`papa.WithProxyURL`）。
- **操作日志/访问令牌**：与站点无关（前者记的是后台动作，后者是后台的凭据）。

### 按站点切的进度：三队列已**并回全局**；`crawler` 那四项**有意**保持全局

- **`error_queue` / `recover_queue` / `repeat_queue` 现在是全局各一条**（不再按站点拆）：
  各一段写在 `config.yaml` 里，后台各占一行。当初按站点拆没有收益（三队列本质都是对
  `crawler_tasks` 表的一次复查），反而让"一个阶段占一个协程空等"的多站开销被放大 —— 已并回全局。
- **`crawler` 的四项有意留在 `config.yaml`（定过的，不是待办）**：`queue_watermark`（`engine/stage.go`
  建池那行）、`archive.*`（`engine/archive.go`，落盘路径按 `{site}/{stage}/` 分两层）、`trace.*`
  （`engine/trace.go`，步骤记录本来就带站点）、`dedup_cache_size`（`engine.NewEngine`）。
  它们是**进程级资源**那类数值（水位、去重预算、保留期、归档总闸），调的是"这个进程扛不扛得住"，
  而不是"这个站要怎么抓"；目前没有逐站调的真实需求，按站点拆等于把一句话摊成 N 份。
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
