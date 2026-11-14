# Admin / Obs / Cmd 诊断（2026-11-13）

<!-- version-scope -->
> **适用版本/阶段：Papa v4 演进过程中的历史工作区快照（具体基线以正文为准；未记录精确发布版的条目不推定版本号）。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 当前使用方式以 [v4 手册导航](../../../README.md) 为准。
<!-- /version-scope -->


> 本文保留修复前诊断证据与当次验证。D1–D9 后续修复和最终验证见 [修复记录](remediation.md)。
## 范围与结论

阅读了工作区 AGENTS.md、docs/package_structure 下 ADMIN/OBS/CMD 三分册，检查后台装配、鉴权、文件接口、配置缓存/审计/采样生命周期、账号与运行策略前端、观测输入/产物/清理和 CLI 报告。未修改生产代码，无 Git 写操作。完整 test/race 由主线程统一运行，本分报告不代替其结果。

当前组织已经从集中式 Monitor 转为按功能拥有状态和资源：server/monitor.go 仅构造功能组件；reports/logs 各自处理文件策略；access 拥有白名单及重载器；accounts 等 HTTP 层通过能力接口使用领域服务；obs 不接管 Engine；报告调度归 internal/reportrunner。检查未发现应把功能重新集中到 App/Engine 的理由。现有主要可复现缺陷集中在报告文件边界和历史重生成流程。

## 确认问题

### P2 A1：报告文件接口允许通过 junction / symlink 越过报告根目录

- 位置：`admin/reports/handler.go:164-186`，特别是 180 的 filepath.Rel；实际打开在 `143` 的 os.Stat 和 `152` 的 os.ReadFile。
- 触发：报告根目录里已有一个指向根外的目录链接，客户端用其名称作为 run，file 使用白名单中的 report.md。单段名称合法、字符串 Rel 也在根内，但最终文件系统解析越界。
- 实测：在 `.review-tmp/admin-report-probe/root/linked` 建 Windows junction，目标为相邻 outside；outside/report.md 仅含合成哨兵。调用公开 reports.Handler.File 得到 `status=200 body="OUTSIDE_REPORT_ROOT_SENTINEL\r\n"`。
- 影响：具有后台读取权限的调用者可读取报告根之外的同名文件；如果是文件链接，目标文件名无需是 report.md。它不是未登录任意读，也不证明远程用户能创建该链接：利用前提是目录中存在链接（维护操作或本地可写方）。当前“越界一律拒绝”承诺并未覆盖文件系统真实路径。
- 修复方向：文件策略仍归 reports 包，采用 rooted 文件打开（日志下载已有 os.OpenRoot 风格可对照），从已打开句柄检查普通文件与尺寸，并限制实际读取长度；不要只先 EvalSymlinks 后再次按路径打开。

### P2 A2：CLI 显式重生成过期历史日，成功输出后立即删除刚生成的四份产物

- 位置：`cmd/papa/report.go:224` 写日报，`231` 无条件按当前时间执行 report.Prune；`obs/report/retain.go:74-77` 按目录日期判断过期，而不是本次生成时刻。
- 触发：retention=168h，`report generate --since/--until` 选择 10 天前，正常执行历史诊断。无需坏输入或并发。
- 实测实际 CLI：`go run ./cmd/papa report generate -c ../.review-tmp/admin-report-probe/config.yaml --since 2026-11-03T00:00:00+08:00 --until 2026-11-03T23:59:59+08:00 --no-logs`。exit=0；输出四行“写出 .../2026-11-03/...”，随后“清理过期报告 1 份：2026-11-03”。目标目录不存在。使用无效 DSN 的合法离线降级路径，未访问真实数据库。
- 影响：历史排查最需要的报告无法取走，调用方收到成功但结果已无；默认扫描全部旧事件还会不断重建又删除旧日报，浪费读写和分析时间。
- 修复方向：CLI 明确历史重生成与日常保留清理的关系，例如本次显式历史输出排除于本轮清理或只对自动常驻报告做保留清理。不要直接改 Prune 的日期规则破坏已承诺的日历保留语义。

### P2 A3：run_id 为 `..` 时 CLI 将报告写到 --out 父目录

- 位置：`obs/report/markdown.go:357-380`，sanitizeDirName 允许点且未排除 `.`/`..`；`cmd/papa/report.go:206` 以 DirName 作为标签，`133` 直接 filepath.Join(outDir,label)。
- 触发：输入事件的 run_id 是 `..`，CLI `--run ..` 匹配该 run。eventlog.Read 接受这类有效 JSON 事件，未验证 run_id 格式。
- 实测实际 CLI：把合成事件 run_id 改为 `..`，运行 `go run ./cmd/papa report generate -c ../.review-tmp/admin-report-probe/config.yaml --run .. --out ../.review-tmp/admin-report-probe/escape-box/inside --no-logs`，exit=0；四份产物实际写到 `escape-box/report.md` 等，完全在指定 inside 外。
- 影响：导入、历史或手工事件输入可以把固定名称的四个报告产物写到父目录，并替换同名已有文件；不属于任意路径任意内容写入。正常框架生成的随机 run_id 不触发，优先级低于正常历史重生成缺陷，但安全目录名工具不能仅允许字符而漏掉路径特殊段。
- 修复方向：DirName 对 `.`/`..` 和跨平台特殊名归一为普通目录名，CLI 写入前再确保最终目录在 out 根内；修复属于 report 产物命名和 CLI 输出接线，不进入 App。

## 已知限制和需要补充验证的风险（不冒充新确认故障）

- 报告列表 100 份只是返回截断：`admin/reports/handler.go:68-119` 先枚举所有目录、逐份 Stat 并读摘要，再排序截断。历史量很大时单次 HTTP 的 I/O 与内存不受 100 限制。文件数/并发/延迟压力测试未执行。
- 下载的 16 MiB 限制是先 Stat 再 ReadFile，文件替换或增长存在窗口；未做并发替换复现，不把它单独列为已实测漏洞。与 A1 可由同一文件句柄方案处理。
- `obs/eventlog/read.go:67,99` ReadString 无单行上限，窗口内事件和去重键全量存内存；`obs/daily.go:82` 逐日重读并把每天 Sources 全部积存在返回切片。文档已经明确这不是有界内存；大日志下的峰值 RSS、报告耗时和关停预算仍应实测。单实例内资源预算问题在范围内，不涉及多实例。
- 报告四份产物是单文件原子替换，整体不是事务；读页面可能跨代读取。OBS 分册已准确披露，不应作为“文档声称事务但实现缺失”的新问题。
- 取消链覆盖有限：obs 文件读取、Pattern、Write/Prune 不带 context；Generate 主要把 ctx 交给 Section。现有文档已披露。是否关停仍安全由 reportrunner/App 对未退出依赖保留策略决定，本分未据此推断资源提前关闭。
- `admin/auth/token_counter.go` 每次验证都查数据库，避免计数缓存绕过权限；但 Verify 的 SQL 不从 HTTP 请求传 context。DB 卡顿时鉴权不能随客户端取消退出的风险存在，尚未做真实 DB 延迟实验。
- `admin/oplog/oplog.go` 首次 Close 真正等待排空，timeout 只是告警；重复 Close 提前返回。文档明确规定宿主只能把首次 Close 完成当屏障。未发现这里需要按“超时即退出”重写的依据。
- `admin/sysinfo` 系统采样失败有 available 与成功时间，快照深拷贝 map/时间指针；Start 无排空接口且目录遍历无 context，属于现有生命周期限制。Windows 多盘部署采样 `/` 是否对应期望存储盘，需要产品定义/环境验收，未列确认缺陷。
- 来源白名单读错放宽但令牌鉴权仍强制，是已写明的既有策略；不能误报成匿名管理权限。认证关闭默认与路由装配已检查。
- CLI new/upgrade 的文件写入非整批事务、migrate/token/report 没有逐命令 DB Close 是 CMD 分册明确披露的边界；短命令退出后资源由进程回收，不能直接等同长驻泄漏。
- 账号、账号池等前端有 dead/generation、写入冻结和控制租约清理；本轮只读源码，未实际运行浏览器、账号登录、代理或远程交互，不声称这些路径全部通过。

## 复现材料与输出

材料保留于工作区 `.review-tmp/admin-report-probe/`（按主线程要求保留）：main.go、config.yaml、logs/events.jsonl、root/linked junction、outside/report.md、escape-box 中父目录写入的四份报告。事件最后一次运行已改为 run_id=`..`；历史重生成仍可按时间窗口重跑，不需要恢复 r-old。

公开 API 探针输出：

```text
junction_download status=200 body="OUTSIDE_REPORT_ROOT_SENTINEL\r\n"
run_dotdot_dir=".." joined="E:\\go-project\\test_papa\\.review-tmp\\admin-report-probe"
historical_write paths=4 err=<nil>
historical_prune removed=[2026-11-03] err=<nil> file_exists=false
```

实跑 CLI 输出摘要：

```text
[2026-11-03] 判定：未判定（unknown）
事件：2 条 · 缺口 0 · 坏行 0 · 日志签名 0
写出: ...\history\2026-11-03\report.md （及另外三份）
清理过期报告 1 份：2026-11-03
exit=0

[..] 判定：未判定（unknown）
写出: ..\.review-tmp\admin-report-probe\escape-box\report.md （及另外三份）
exit=0
```

这里 unknown 来自刻意最小化的离线合成事件/无 DB，仅用来重现产物处理；不是用该 fixture 验证真实业务健康。

当前收尾状态见 [闭环清单](closure.md)；本文的修复前发现和当次未验证项作为历史证据保留，不能代替最新验证结论。
