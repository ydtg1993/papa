# 全仓 124 项跳过测试梳理与本地补验

<!-- version-scope -->
> **适用版本/阶段：独立 Papa v4 的 2026-11-14 工作区验证快照。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 当前使用方式以 [v4 手册导航](../../README.md) 为准。
<!-- /version-scope -->


日期：2026-11-14。承接 [最终代码复审](2026-11-14-final-code-rereview.md)。完整原始测试名、功能归属、上一轮直接证据及本轮日志见 [124 项逐项清单](2026-11-14-skipped-tests-inventory.md)。

**最终结果：原清单 124 项均有实际验证路径，未覆盖 ID 为 0。121 个独立场景全部通过（119 项 Windows race、2 项 Linux 实际权限断言）；3 个子进程专用入口由父测试驱动并验证，不计为独立通过。** 原先有直接实库证据的 41 项本轮也重跑；其余 80 个独立场景此次取得直接执行证据。补验发现并修复 FFmpeg 参数顺序问题。修后普通全仓、m3u8 整包 race、全部上述补验以及全仓 vet/build 完成；本轮未再次运行全仓 race，上一轮全仓 race 证据只代表当时版本。

## 计数与分类

从上一轮最终普通全仓 JSON 提取 124 条含测试名的 skip 记录；普通全仓与全仓 race 的 skip ID 集合完全一致。这里的“项”包含叶子子测试，不等于 124 个顶层函数。没有为了降低跳过数删除环境门控；父测试专用入口也不能直接设环境变量伪装成独立通过。

| 环境类别 | 原始项数 | 功能范围 | 上一轮直接补验证据 |
| --- | ---: | --- | --- |
| MySQL | 94 | 后台鉴权/策略 CAS/表读取，任务身份/计时/重复提交/流策略，熔断恢复与结算，调度背压，App 生命周期/obs 隔离，v4 迁移及百万行校验，账号池/接入/浏览器身份持久化 | 41 项精确测试名已通过；53 项未出现在上一轮两份选定日志中 |
| 本地 Chrome | 23 | 后台 UI、身份诊断、账号桥接、远程接管、网络治理、profile 及共享目录清理 | 无上述 MySQL 日志中的直接覆盖 |
| 本地 FFmpeg | 2 | 本地媒体合并/禁止外部网络输入、含单引号路径 | 无直接覆盖 |
| POSIX 文件权限 | 2 | 报告文件模式、下载合并产物权限 | Windows 下不适用，原平台不可能执行到断言 |
| 父测试子进程入口 | 3 | Engine 强杀/失败清理子进程、App 负载子进程、App 混合生命周期子进程 | App 两个入口已有父测试驱动证据；Engine 入口需本轮对账 |

“上一轮”严格指 `final-recheck-mysql-final.log` 和 `final-recheck-settlement-verified.log`。历史报告可能记载更早验证，但此处不把较早批次或同类测试当成最终代码的逐项证据。

## 环境与执行方式

- MySQL：只从本地配置读取连接信息，连接前移除数据库名。各测试通过独占 CREATE 获得随机测试库所有权；后台鉴权和框架表读取由启动器另建两个独占库。框架表夹具显式建全 13 张表并写入合成任务，读取测试处于只读事务，结果为 `available=13 missing=0`。没有打开、迁移或查询业务数据表。
- Chrome：本机 Chrome 157.0.8103.51，仅本地合成 HTTP 服务、内存 store 和独立 profile。使用 `PAPA_EGRESS_BROWSER_BIN`、`PAPA_TEST_CHROME`；两条 live pool 测试另外通过 Rod 的 `-rod=bin=<本机绝对路径>` 指定浏览器，没有下载浏览器或复用用户 profile。
- FFmpeg：本机 `E:\ffmpeg\bin\ffmpeg.exe`，以 `PAPA_EGRESS_FFMPEG_BIN` 显式启用，两条真实进程用例均执行。
- POSIX：Windows Go 交叉编译 Linux amd64 测试二进制，然后在 WSL Ubuntu 的 `/tmp`（Linux tmpfs）真实运行两项断言。不是仅交叉编译，也不是在 Windows 挂载目录上检查权限；`CGO_ENABLED=0`，未执行 Linux race。
- Windows 补验均 `GOWORK=off`、`CGO_ENABLED=1`、`-race -count=1`。MySQL 和浏览器包串行运行。完整普通全仓另行无缓存运行，门控仍保持原有行为。

启动器 `.review-tmp/skip-mysql/run.go` 清除继承的 `PAPA_*` 变量，只给测试传入受控值；不会误用预先设置的固定 repeat/task-flow 库。运行前后仅查询测试库名元数据作差异核对，发现残留会报错，不自动删除任何未获所有权的库。临时配置退出时删除，日志不打印 DSN。

## 发现的问题与修正

### FFmpeg 输出选项放错位置

两条真实 FFmpeg 测试均曾失败，报错包含：

```text
Option fs (set the limit file size in bytes) cannot be applied to input url
```

`pkg/middleware/m3u8/downloader.go` 把输出选项 `-fs` 放在 `-i` 前，导致 FFmpeg 将其解释到输入侧。按下载器职责把它移到输入之后、目标输出之前，保留输入协议白名单、流复制及输出发布边界。

真实测试新增 `output_budget_preserves_previous_artifact`：用合成媒体触发输出预算，断言 `ErrResourceLimit`、旧目标内容不变、`.papa-publish-*` staging 无残留。m3u8 整包 race 通过（73 个顶层测试，0 失败/跳过）；没有用参数字符串断言代替外部进程执行。结构说明已同步。

### 首批测试启动配置不完整

首批 MySQL 的策略 CAS 与账号池两项在读取临时配置时因 `config: log.dir` 缺失失败，尚未进入对应 DB 事务断言。补齐启动器的日志目录和连接池必填项，并在启动测试前调用配置校验；这不是生产代码缺陷。两项随后独立无缓存 race 重跑通过。保留首批失败记录，不把该批整体退出 1 描述为全绿。

## 执行证据

所有日志在 `.review-tmp/`；逐项覆盖由 `skips-coverage-final.json` 及附表对账，不能把下列批次的测试数量直接相加。

| 批次 | 证据 |
| --- | --- |
| 原始 124 项 | `skips-inventory.json`、`skips-classified.json` |
| MySQL 首批 | `skip-mysql-race.jsonl`，两个启动配置失败外，其余选定场景通过；独占库清理后差异为空 |
| MySQL 补跑与百万行校验 | `skip-mysql-supplement.jsonl`，受影响两项和规模测试均通过，0 失败/跳过，退出 0；独占库清理后差异为空 |
| 浏览器 7 包、23 项 | `skip-browser-*.jsonl`、`skip-browser-results.json`，23 项及额外两个子测试 race 通过，无新增 skip |
| 媒体修复红/绿证据 | `skip-media-ffmpeg.jsonl`、`skip-media-m3u8-race.jsonl` |
| Linux 实际权限断言 | `skip-media-linux-report.jsonl`、`skip-media-linux-filedown.jsonl`、`skip-media-linux-environment.txt` |
| 修后完整普通测试 | `skip-followup-all.jsonl`：77 个有测试包、2,925 项通过、124 项门控/平台/入口跳过、0 失败，退出 0 |
| 静态检查与构建 | `skip-followup-vet.log`、`skip-followup-build.log`，全仓 vet/build 退出 0 |

Engine 持续负载显式运行 60 秒：1,280 条任务及尝试逐项一致，初始 pending 64、驻留峰值 16。App 饱和用例采用默认 10 秒窗口（本次实际耗时约 14.4 秒），最低每组 pending 51，驻留峰值 4，四组均有进展；obs 故障子场景另有各自的短窗口。它们是受控短时负载回归，不是 24/72 小时生产长稳。

百万行校验实际构造 10,000 / 100,000 / 1,000,000 行，每级执行三次启动校验；百万行三次为 2.8096s、2.9284s、1.9918s，并验证首尾非法身份被拒绝及数据未被校验过程改写。规模测试 131.75s 通过，清理独占库后再次核对不存在。该时间是当前本机合成夹具实测，不是性能门槛或生产承诺。

## 清理与证明边界

浏览器前后 Chrome 进程数均为 14，没有新增残留 PID；途中采样的 8 个测试进程均退出，采样 profile 已删除，两条 live pool 测试还验证共享根目录及借用 profile 不被误删。未逐个采样所有短生命周期 profile。Linux 自有 `/tmp/papa-skip-media.*` 目录及 Windows 临时 Linux 二进制清理已确认。MySQL 父测试断言随机库和夹具目录清理，启动器另检查测试库名差异。

子进程入口只有父测试给出的库、令牌、目录和协议才有意义；部分子进程故意强杀或断言失败，以父测试的退出、恢复、数据一致性和资源清理断言为结果。不能把这三项本身改成无条件 PASS。

仍未验证：生产真实站点/账号 Provider/凭据/代理链，其他浏览器或 FFmpeg 版本组合，完整 Linux/macOS 生命周期及 Linux race，24/72 小时长稳、断电/文件系统故障/MySQL 服务崩溃。百万行夹具不等于千万级、宽业务行、严格冷缓存或生产负载 SLA。框架表读取只针对合成完整 v4 schema，未验收业务现存表。

本轮未改公开 API、任务结算/锁序/关停协议。所有业务数据保持不动，已有工作区改动保留，未执行 Git 写操作。
