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
core/                           零依赖叶子包：跨包流动的纯值（告警 / 错误分类 / 监控快照 DTO）
config/                          配置结构 + 运行期覆盖层（纯数据，不依赖任何内部包）
crawler/                         引擎：Task · Trace · Fetcher · Engine
  engine.go    结构体 + 生命周期 + 依赖注入 + 结果读写
  stage.go     阶段装配与 worker 循环（ApplyRegisterStage / runAttempt）
  submit.go    提交路径（去重 → 落库 → 入队 → 认领）
  spill.go     高水位溢出与回灌
  batch.go     各治理队列共用的分页扫描（keyset 游标）
  dedup.go     内存去重表 + 活跃任务装载
  stats.go     监控快照
  fetch.go     浏览器池 / 静态 HTML 客户端
  recoverqueue.go · errorqueue.go · repeatpoll.go · delayqueue.go · taskadmin.go · trace.go
internal/                        框架内部件（不对外承诺 API）
  workerpool/ database/ metrics/ msgqueue/ track/
  server/ auth/ tokenadmin/ oplog/ tasksource/ gormsource/ sysinfo/ scheduler/   ← 后台模块
pkg/                             可独立复用的件
  browser/ htmlfetch/ loggers/ notify/ middleware/{proxy,m3u8,filedown}
models/                          框架自带的表结构
cmd/papa/                        脚手架 CLI（new / migrate / token / html / rod / diff / select）
```

**依赖方向**（箭头 = import 方向，不允许反向）：

```
papa 门面 ──► internal/app ──► crawler ──► internal/{workerpool,database,metrics,track}
                 │                │              pkg/{browser,htmlfetch,loggers,middleware}
                 │                └──► core ◄── config
                 └──► internal/{server,auth,tokenadmin,oplog,tasksource,...} ──► core
pkg/notify ──► core
```

两条约束值得记住：

1. **`core` 只能 import 标准库。** 它被 `internal/server`、`pkg/notify` 这些叶子用；一旦它
   import 了 gorm / models，这两处就会被连带着拖上 —— 那正是抽它出来要避免的。
2. **值类型要跨包就走 `core`，引擎自己的类型留在 `crawler`。** `Task` / `Trace` / `Fetcher`
   与引擎强耦合（`Fetcher` 的签名里就带 `*Engine`），搬过去零收益，还得多拖 gorm 和 logrus。

`crawler/alias.go` 给旧路径留了别名（`crawler.StageStats` ≡ `core.StageStats`，是**同一个类型**
而不是可互转的两个），所以从 `crawler` 切到 `core` 对调用方是非破坏的；新代码直接用 `core`。
