# 数据库执行策略

> 面向：初始化、调整及审计整站熔断和公共浏览器池；后台入口为「系统设置 → 熔断与浏览器」。公开 Go 契约位于 `pkg/executionpolicy`。

## 初始化与升级

`crawler_config.execution_policy` 是这些字段的唯一运行来源，使用独立 revision。普通启动只读取、校验和应用；文档缺失、站点未知、schema 错误或数据库不可用时停止启动。不会回退 YAML、Go 默认或站点声明，也不会自动创建第一版。

1. 配置数据库连接，显式执行 `go run . -migrate`（标准业务启动入口；或 `papa migrate`）。
2. 审核 `configs/execution-policy.json` 的完整初始化文档，站点键必须已注册。
3. 执行 `go run . -init-policy configs/execution-policy.json` 写入第一版；再次初始化不会覆盖已有配置。
4. 正常执行 `go run .`。初始化文件只供显式导入，后续运行不再从文件取值。

已有项目先导出升级前真正有效的全局与站点策略，转换为完整文档。删除 YAML 中的 `crawler.breaker`、`browser.pool_size`、`browser.direct_pool_size`、`browser.max_idle_time`；这些旧键会拒绝启动。删除 Go 的 `SiteSpec.Breaker` / `BreakerSpec`；站点覆盖改存数据库。旧活动事件缺少策略快照时，必须先显式绑定升级前策略；不能结束事件、重置计时或猜测阈值来绕过迁移。完成 schema 迁移后、普通启动前，执行 `go run . -migrate-breaker-policy path/to/confirmed-old-policy.json -confirmed-policy-revision N`。N 必须是经确认的正版本号；输入保留旧有效策略及已退役站点的覆盖，不能拿新初始化文档冒充旧事件策略。

`browser.enable`、二进制、网络授权、worker/队列物理容量及数据库连接仍由启动配置决定。账号专用池不属于此公共池配置。

## 文档与校验

完整结构为 `schema_version: 1`、`breaker.defaults`、`breaker.sites`、`browser`。`breaker.sites` 每个条目是完整站点策略；删除条目表示继承数据库默认。不是逐字段 Patch，也不继承文件值。

| 区域 | 字段 | 约束 |
| --- | --- | --- |
| breaker.defaults / sites.<site> | enabled、window、threshold | window 为正且不超过 24h，threshold 为 1..100000；enabled=false 只禁止新事件 |
| 上述对象的 policy | short_window、fast_threshold、mixed_threshold、observe_for、cooldown、probe_timeout、max_probes、probe_interval、max_recovery、ramp_for、interval、capacity | 必须显式填写正值；校验容量覆盖阈值、恢复预算覆盖冷却/探测/限量恢复 |
| browser | pool_size、direct_pool_size、max_idle_time | >0、≥0、≥0；所有时长为带单位字符串，如 `30s`、`5m`、`0s` |

初始文档不是运行 fallback；合法示例不能替代对实际站点阈值的审核。未知 JSON 字段拒绝接受。

## 生效与状态

保存使用完整文档 CAS。保存版本、应用版本及运行状态分开展示；`degraded` 表示持久化目标尚未完全应用，拒绝继续叠加写入；`stopping` 拒绝新写。数据库故障不改变最后已应用策略。

浏览器扩容按需求创建；缩容先清理空闲实例，借出实例归还后回收，不杀在途页面。`direct_pool_size=0` 拒绝新直连借用并唤醒等待者；`max_idle_time=0s` 停止超时回收。两池显示 target/creating/borrowed/idle/closing/waiting/converging；这些是受管实例数，不是 Chromium 子进程数。browser.enable=false 时保存不会在线启动组件。

活动熔断事件冻结完整策略及版本，包括窗口、容量与恢复预算。保存、人工重试、小时复查、重启恢复均不得改写该事件的冻结值。事件结束后使用最新目标策略；保存不解除自动保护、人工暂停或 Retry-After。窗口扩大不能补回已过期的证据。

## HTTP API

使用既有 OA Bearer 凭据鉴权，操作人来自服务端认证上下文，body 不接受 operator。宿主通过 `papa.WithExecutionPolicyAuthorization`（底层 `MonitorConfig.AuthorizeExecutionPolicy`）实现 read/write 授权；独立 Agent 应使用可撤销的独立凭据。写请求遵循同源限制，不把凭据放 URL。

| 方法 / 路径 | 请求 / 返回 |
| --- | --- |
| GET /api/config/execution-policy | document、revision、applied_revision、status、runtime、sites |
| POST /api/config/execution-policy/validate | body 为完整 Document；返回候选 document、current_document、runtime、current_revision 与 warnings，不写库 |
| PUT /api/config/execution-policy | expected_revision、operation_id、document、reason |
| GET /api/config/execution-policy/history?limit=50 | 最近历史；limit 为 1..100 |
| POST /api/config/execution-policy/rollback | target_revision、expected_revision、operation_id、reason；写成新版本 |
| GET /api/config/operations/{operation_id} | 操作身份、保存版本及 applied/saved/superseded 状态；superseded 表示该保存版本已由更新版本替代，用于响应丢失后的核对 |
| GET /api/config/capabilities | 配置域、schema、可写字段、约束及生效语义 |

`400 invalid` 为非法请求，`409 conflict` 携带 latest，`404 not_found` 表示目标/操作未知，`503 unavailable/stopping` 不表示一定未提交。超时先按原 operation_id 查询，不盲目生成新 ID 重放。同一 ID 同一内容返回原操作；同一 ID 不同内容冲突。未知或过期操作需要人工核对，不能推断提交失败。

OA 冲突保留草稿，手动比较最新版本后决定是否更新 CAS 基线。保存期间禁重复提交；结果不明时锁定写入并显示操作 ID。页面退出停止轮询，旧请求不会覆盖新页面草稿。

## Go 调用

调用方依赖公开 `executionpolicy.Service`；标准宿主启动初始化后可通过 `App.ExecutionPolicy()` 获取，不导入 internal 或直接操作数据库：

```go
func updatePool(ctx context.Context, service executionpolicy.Service, operationID, actor string) error {
    current, err := service.Read(ctx)
    if err != nil { return err }
    candidate := current.Document.Clone()
    candidate.Browser.PoolSize = 4
    if _, err = service.Validate(ctx, candidate); err != nil { return err }
    _, err = service.Update(ctx, executionpolicy.Update{
        ExpectedRevision: current.Revision, OperationID: operationID,
        Document: candidate, Reason: "调整公共池容量",
    }, actor)
    return err
}
```

actor 必须来自调用侧可信认证上下文。配置动作与暂停、恢复、强制解除是独立接口。


## 并发与关停边界

浏览器池只在启动阶段初始化一次；再次初始化会报错并保留原池。运行期调整使用版本化配置服务，避免更换池后应用器仍引用旧池。创建失败的部分实例会关闭；关闭与许可交错也必须准确释放，不将失败实例放回空闲池。

框架拥有浏览器启动进程及 Leakless 守护连接，启动和健康检查支持取消；成功连接后不再受单次借用 context 取消影响。缩容不会强杀已借出的实例。失败启动的进程清理完成前保留物理占位；若取消发生在守护进程返回子进程 PID 之前，等待守护进程清理并退出，不提前释放容量或杀掉守护进程而遗留 Chrome。

人工接管自动熔断事件后，普通重试恢复仍使用原事件策略。尚未结算的失败日志，以及终态尚未持久化时重新触发的同一事件，也保持冻结策略；新目标继续显示为 deferred，直到保护与结算条件允许切换。保存配置不重投任务、不重置重试或认领代次。

公开 Go 配置写入与 HTTP 写入使用同一个关停排空屏障。宿主先禁止新写，再等待在途操作；超时不关闭仍被使用的数据库和浏览器资源。组件同版本不同内容会拒绝，不能用同一个版本号替换全局默认或站点覆盖。

OA 查询到 saved 只代表已持久化，仍保留草稿与写入锁定。可显式重试原操作（同 operation_id、同 expected_revision、同正文），由服务核对并应用；只有确认 applied/superseded 且成功读取当前已应用配置后才结束核对。404 或读取失败不能视为未提交。操作 ID 按精确字节核对；数据库大小写折叠产生的唯一键冲突不会被当成旧请求成功。
