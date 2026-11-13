# 动态阶段调度

> 面向：需要按阶段组合广度/深度策略的业务开发者。任务仍由原阶段工作池执行；调度器在认领时决定是否放行。

## 启用与升级

配置 `task_flow.enabled: true`，显式执行 `papa migrate` 后重启。新增可选 `crawler_task_flows` 表；`crawler_tasks` 新增 `flow_id/flow_lane/flow_branch/flow_path` 四列及 flow_id 索引。即使未启用调度，新版本也须迁移任务列；大任务表建议低峰期迁移。普通启动不建表。

普通 SubmitTask/SubmitTasks 行为保持不变，不自动把现有积压解释成任务树。新流程通过专用 API 登记。不要在普通 SubmitTask 中自行填写 Flow 字段。

## 创建和投递

阶段先按现有 API 注册，然后创建运行：

```go
policy := papa.FlowPolicy{
    Stages: map[string]string{"catalog": "discover", "detail": "items", "content": "items"},
    Lanes: map[string]papa.FlowLane{
        "discover": {Mode: "breadth", Parallel: 2},
        "items": {Mode: "depth", Parallel: 1},
    },
}
err := e.CreateTaskFlow(ctx, "run-001", "example", policy)
// 检查 err；重复创建不会覆盖旧运行。运行 ID 由业务持久保存。
err = e.SubmitFlowTask(ctx, "run-001", 0, root)
// FetchHandler 内同步登记下游任务；检查每次错误，再返回。
err = e.SubmitFlowTask(ctx, task.FlowID, uint(task.ID), child)
```

自定义 Fetcher 需要显式调用 SubmitFlowTask。不要启动一个异步 goroutine 派生任务后立即返回，父任务终态后新增子任务会被拒绝。重复登记同一运行/父任务下的同 URL 子任务返回既有任务，不重复抓取。

## 策略语义

每个 lane 是一个跨阶段调度范围。目录可独立广度发现，详情与内容共享分支深度范围；两个范围可并行。

| 策略 | 放行规则 | Parallel |
| --- | --- | --- |
| breadth | 已登记任务树中较浅层结束后放行深层；同层不保证完成次序 | 同层在途任务数 |
| depth | 分支按根 ID，分支内按稳定前序；同一分支一次只执行一条任务 | 并行分支数；1 为严格分支优先 |

父任务处于 pending/processing 时，子任务不执行。成功或失败终态都释放后续调度；失败后的人工/错误队列重试重新参与策略。子任务已成功不因父任务重试重复执行。正在执行中的退避重试仍占分支；可通过增大并行分支数推进其它分支。

模式修改影响下一次认领；已运行任务不强停。缩小并发时先等待已有任务退出，不新开超过上限的分支。旧的在途顺序可能暂时与新策略不同，这是正常过渡。加急只能改变排队优先级，不能突破层次/分支限制。

广度是对已登记任务树的层次约束，不保证未来尚未提交的新入口也在当前层次屏障内。目录各分页的树关系由业务决定；需要同层分页时应在同一父任务登记，而不是把每页都当作上一页的子级。

## 动态切换与后台

`e.TaskFlow(ctx,id)` 返回策略 revision、活跃任务及等待原因（最多展示 200 条）。`e.UpdateTaskFlow(ctx,id,revision,policy)` 使用 CAS；过期 revision 返回 ErrFlowConflict，须刷新后重新决定。阶段到 lane 的映射固定，运行中可改各 lane 的 mode/parallel；重新分组须另建流程并由业务显式迁移工作，不能静默改变既有分支身份。

启用后，现有后台「采集任务流 → 运行调度策略」可输入业务保存的运行 ID，查询并人工应用策略。页面不自动轮询覆盖编辑。接口为 GET/POST `/api/task-flow?id=...`；POST body 为 `{revision,policy}`，须具名令牌。变更接入既有操作日志（需开启 operation_log）。确认策略只修改已有运行的调度方式，不创建新任务。

POST 请求体最多 1 MiB，只接受一份 JSON；重复键（包括嵌套对象）、未知字段和超过 64 层的嵌套会返回 400，不调用策略更新。自定义宿主通过 `OnTaskFlowAction func(server.TaskFlowActionEvent)` 接收操作结局。

## 持久化与限制

- 策略更新、任务登记、认领锁定同一运行行，防并发越过策略。任务完成唤醒回灌；漏通知由原周期回灌兜底。恢复仍需启用已有 recover_queue；框架沿用单个活动引擎的恢复约定，不提供多实例任务租约。
- 每运行最多 10000 条持久任务、32 层、32 个阶段；并发上限 1..64。达限拒绝登记。调度读取有界活动集合；大规模无限采集需要后续独立容量设计。
- 任务使用 `(site,stage,identity_hash)` 唯一约束。跨父任务/跨运行的同 URL 不会被偷偷迁移，返回关系冲突；共享链接须在业务发现层选择唯一归属。一个运行内支持重试，尚不支持直接为同批 URL 创建新的周期运行。
- 当前不支持受控任务自身开启 Repeatable/延迟投递；周期应由业务明确创建批次，不能让永久轮询任务占住父层屏障。保留普通任务的原能力。
- 后台禁止删除受控任务和开启轮询，防破坏关系。失败标记/重试/加急仍保留。不要直接改 SQL 关系字段；流程归档清理尚未提供。

框架只提供调度。真实网站的阶段声明、抓取逻辑、字段落地和清洗由独立业务模块接线。
