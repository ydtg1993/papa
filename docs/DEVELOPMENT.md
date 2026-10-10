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

## 2. 业务项目（`papa new` 生成）

生成的 `Makefile` 在框架基础上多几个目标：

| 目标 | 作用 |
| --- | --- |
| `build` | 编译业务二进制到 `bin/crawler(.exe)`（自动按 OS 补 `.exe` 后缀） |
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
                                （告警 / 错误分类 / 监控快照 DTO、ctx 携带的请求头与代理、URL 解析与同站判定）
config/                          配置结构 + 运行期覆盖层（纯数据，不依赖任何内部包）
engine/                          引擎：Task · Trace · Fetcher · Engine
  engine.go    结构体 + 生命周期 + 依赖注入 + 结果读写
  stage.go     阶段装配与 worker 循环（ApplyRegisterStage / runAttempt）
  submit.go    提交路径（去重 → 落库 → 入队 → 认领）
  spill.go     高水位溢出与回灌
  batch.go     各治理队列共用的分页扫描（keyset 游标）
  dedup.go     内存去重表 + 活跃任务装载
  sitestat.go  站点表与站点维度快照（启动播种、轮询统计回写、熔断状态）
  stats.go     监控快照
  fetch.go     浏览器池 / 静态 HTML 客户端
  archive.go   页面归档：失败那一刻的原始页面 + trace 步骤
  entry.go     阶段入口（fetcher 可选实现 SubmitEntries，框架启动时调）
  restricted.go 受限页判定（站点词表从 task.Site 取）
  deps.go      阶段依赖声明（NeedsFiledown / NeedsM3U8 的启动期校验）
  queuestats.go 三条治理队列的运行快照与积压（按站点拆）
  alias.go     给 core 里的值类型留的别名（见文末）
  recoverqueue.go · errorqueue.go · repeatpoll.go · delayqueue.go · taskadmin.go · trace.go
admin/                          ★后台模块：**对外公开**，业务项目可以直接 import 复用
  server/       监控后台（HTML / API / 静态资源）
  auth/         访问令牌校验与操作人身份
  tokenadmin/   令牌 CRUD 接口
  oplog/        操作日志（异步写入器 + 只读表格页）
  tasksource/   内置「任务」表，动作接线的参考实现
  gormsource/   配置驱动的 oao.Source，业务表格页直接复用
  sysinfo/      机器指标 / 目录占用采集
  scheduler/    业务定时任务（RegisterCronJob 的落点）
internal/                       真正的内部件：不对外承诺 API，**模块外也 import 不到**
  workerpool/ database/ metrics/ msgqueue/ track/ breaker/
  app/                          装配层：把上面这些接成 App（门面 papa.App 指向它）
pkg/                             可独立复用的件
  browser/ htmlfetch/ loggers/ notify/ middleware/{proxy,m3u8,filedown}
models/                          框架自带的表结构
cmd/papa/                        脚手架 CLI（new / migrate / token / html / rod / diff / select）
```

> **`admin/` 与 `internal/` 的区别就一条**：Go 规定 `internal/` 下的包只能被 `internal/` 的
> 父目录（也就是本模块）内的代码 import。所以业务项目**能** import `admin/gormsource`，
> **不能** import `internal/workerpool` —— 后者是框架自己的实现细节，改了不算破坏性变更。

**依赖方向**（箭头 = import 方向，不允许反向）：

```
papa 门面 ──► internal/app ──► engine ──► internal/{workerpool,database,metrics,track,breaker}
                 │               │              pkg/{browser,htmlfetch,loggers,middleware}
                 │               └──► core ◄── config
                 └──► admin/{server,auth,tokenadmin,oplog,tasksource,scheduler,...} ──► core
                         └──► tasksource/oplog 还依赖 admin/gormsource
pkg/notify ──► core
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

### 已按站点切

| 层 | 是什么 | 在哪儿 |
| --- | --- | --- |
| 声明 | `Key` / `BaseURL` / `Headers` / `RestrictedKeywords` / `Breaker` / `AutoRepeat` / 三个治理队列（`ErrorQueue` / `RecoverQueue` / `RepeatQueue`）/ `Stages` | `internal/app/SiteSpec` |
| 执行 | **每站每阶段一个 workerpool**（阶段名跨站唯一，所以"阶段"天然等于"站点+阶段"） | `engine.stage.go` 建池那一段 |
| 执行 | 每站一把熔断闸门（默认 scope 用 `crawler.breaker` 那把） | `engine.SetSiteBreaker` / `breakerFor` |
| 执行 | **每站一条周期轮询队列**（各自的锁、计数、ticker、手动入口） | `engine.repeatpoll.go`（队列名 `repeat_queue:<站点>`） |
| 执行 | 告警事件带 `site` | `core.TaskError.Site`（`core.AlertEvent` 内嵌它） |
| 统计 | 阶段快照带 `site`（后台的站点 Tab 靠它分组）；队列快照按队列名分站；熔断状态带 `Site` | `core.StageStats` / `core.QueueStat` / `core.BreakerStatus` |
| 统计 | `crawler_sites` 站点表（启动按声明播种、轮询统计按列回写）+ 内存快照 | `models/crawler_site.go`、`engine/sitestat.go` |
| 后台 | 顶部站点 Tab、每站一行轮询队列、站点概要；任务表有 `site` 列与筛选 | `admin/server/static/mo.js`、`admin/tasksource/table.go` |
| 数据 | 任务的 `site` 列（引擎按目标阶段自动填，重投路径照抄回来） | `models/crawler_task.go`、`engine.submit.go` |

### 全局，且**有意**如此（别想着分站）

- **进程级**：`crawler.stop_timeout`（停机怎么等）、`crawler.drain_interval`（溢出回灌跳多久一次）、
  `server.*`（HTTP 服务/白名单/采样间隔）、`db.*`、`log.*` —— 一个进程只有一份。
- **客户端级**：`html.*` / `browser.*`（超时、体积上限、空闲回收、全局 headers）。站点要自己的头就用
  `SiteSpec.Headers`（同键覆盖全局那层）；单次请求还能 `papa.WithHeaders`。
- **代理出口**：框架只提供 `engine.NextProxy()` 这个"随用随取"，**不替业务决定**用哪个 ——
  按站点分流出口是业务自己的事（`papa.WithProxyURL`）。
- **操作日志/访问令牌**：与站点无关（前者记的是后台动作，后者是后台的凭据）。

### 按站点切的进度：刀 B 已切完；`crawler` 那四项**有意**保持全局

- ~~**`error_queue` / `recover_queue` / `repeat_queue`**~~：已按站点拆（各自的键/锁/计数/ticker，
  查询带 `site = ?`），后台每站一行。
- ~~**三个治理队列的配置**~~：已**整段搬进站点声明**（`ErrorQueue` / `RecoverQueue` / `RepeatQueue`；
  每条站点声明都得写全，不跑哪条就写 `Enabled: &false` 一行）。`config.yaml` 里再写 `error_queue:`
  这类段会被「未知配置键」拒绝启动、并指路到声明文件。**后台热更整套取消**（`/api/config` 与
  `runtime.yaml` 都没了：改配置 = 改站点声明 + 重启）。
  **代价**：未归属（任务的 `site` 为空）的任务**不再有治理队列** —— 默认 scope 没有站点声明可挂，
  错误重投 / 启动恢复 / 周期轮询对它一律不跑。老库里 `site = ''` 的行要治理，得先在数据侧给它们归站。
- **`crawler` 的四项有意留在 `config.yaml`（定过的，不是待办）**：`queue_watermark`（`engine/stage.go`
  建池那行）、`archive.*`（`engine/archive.go`，落盘路径按阶段分子目录 —— 阶段名跨站唯一，天然分站）、`trace.*`
  （`engine/trace.go`，步骤记录本来就带站点）、`dedup_cache_size`（`engine.NewEngine`）。
  它们是**进程级资源**那类数值（水位、去重预算、保留期、归档总闸），调的是"这个进程扛不扛得住"，
  而不是"这个站要怎么抓"；目前没有逐站调的真实需求，按站点拆等于把一句话摊成 N 份。
  （原来这里记着"归刀 B2"—— 那一刀**不做了**。真要逐站调，改的是阶段参数 `StageSpec` 与站点声明上那几项。）
- **`dedupCache` 是"按 key 分、预算共享"**：key 是 `stage|url`（阶段唯一 → 天然分站），但只有**一个**
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
