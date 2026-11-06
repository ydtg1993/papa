# 完整周期采集示例

这个示例启动本地 HTTP 服务，用真实 MySQL、Engine worker 和 `repeat_queue` 完成两批列表 → 详情 → 分页采集。每批有两页列表、两页详情和四条独立任务；第二批重新访问相同 URL，更新相同业务 ID 的两条记录。

示例不访问外部网站，不需要浏览器、ffmpeg 或模型。数据库须预先创建，库名必须包含 `rule_test`；每次运行使用独立 site 标识，保留框架任务与调度历史，不清空已有表。

## 运行

在 papa 模块目录设置专用测试库连接。不要把真实凭据提交进代码或日志。

```powershell
$env:GOWORK = 'off'
$env:PAPA_RULE_SCHEDULE_DSN = 'root@tcp(127.0.0.1:3306)/papa_rule_test_periodic?charset=utf8mb4&parseTime=True&loc=Local'

# 第一次或框架升级新增表后，显式迁移再运行。
go run ./examples/ruleperiodic -migrate

# 普通运行只检查已存在的表，不建表。
go run ./examples/ruleperiodic
```

正常约 10 秒完成，整个例程设 90 秒上限。第二批完成后，示例调用 `SetTaskRepeatable(false)` 停止控制入口的后续轮询并排空 Engine；采集阶段不启用 `Repeatable`。第二批通过实际 10 秒轮询启动，没有修改数据库时间、伪造成功状态或手动直接调用控制 handler。控制任务的 `control_repeat` 是执行次数，可能包含等待上一批结束的轮询，因此不能用 `RepeatLimit: 2` 表示“两批”。

JSON 输出包含：

- `control_id`：稳定控制任务 ID。
- `batches`：两个不同的 BatchID、序号、固定 root、各四个成功成员 ID，以及当批业务结果。
- `requests`：`/list1`、`/list2`、`/detail/A`、`/detail/B` 各实际请求两次。
- 第一批 `Alpha-v1` / `Beta-v1`；第二批更新为 `Alpha-v2` / `Beta-v2`，仍只有 A、B 两条业务记录。
- `reopened_status` 与 `reopened_equal`：停止并排空原 Engine、关闭原数据库连接，重新连接、重建规则与调度器后，读到相同的第二批持久状态；重开查询不启动采集。

## 测试

```powershell
$env:GOWORK = 'off'
go test ./examples/ruleperiodic

# 明确指定可迁移的专用测试库后，才运行真实 MySQL/Engine 链路。
$env:PAPA_RULE_SCHEDULE_EXAMPLE_TEST_DSN = $env:PAPA_RULE_SCHEDULE_DSN
go test ./examples/ruleperiodic -count=1 -v
```

未设置测试 DSN 时，真实数据库用例会明确跳过；DSN 校验、离线 fixture 验证及业务 Save 幂等性仍会测试。真实用例验证两批 8 条不同采集任务全部成功、四个 URL 各访问两次、结果确实变化，以及重开状态一致；它不以日志或内存计数替代数据库完成状态。

## 接入业务时的关键点

1. 先定义并验证列表、详情规则，再创建对应 `RuleFetcher`。示例用内存 `Registry`，所有固定 fixture 都在启动前离线验收；重启时重新注册和验收。使用持久发布的业务可替换为 `Catalog`，示例不演示发布切换。
2. 用 `NewRuleSchedule(RuleScheduleConfig{Stage: "schedule", Entry: catalog, Interval: 10 * time.Second})` 创建独立控制阶段。周期必须是至少 10 秒的整秒值，控制阶段不能与采集阶段重名。
3. 注册控制、列表和详情三个阶段，启用 `repeat_queue`，通过 `schedule.NewTask` 和普通 `Engine.SubmitTask` 提交控制入口。`rules.enabled` 及显式迁移负责包括 `crawler_rule_schedules` 在内的框架表。配置中的 `rules.sample_dir` 为 `./ruleperiodic-samples`，满足规则配置契约；本例使用内存 Registry，不创建该目录或写入样本文件。
4. 读取 `RulePage.BatchID` 追踪业务来源；保存仍按业务 ID upsert。当前 `businessStore` 是加锁内存 map，只演示并发和重试时覆盖同一业务记录；生产业务需要数据库唯一键与持久事务，不能依靠示例 map 保存重启后的业务数据。
5. 上一批的 root 和所有成员必须成功，后续轮询才能生成新批。失败批次等待既有错误重投或人工修复；不要另给子任务开 `Repeatable`、删除成员或重投已完成的历史批次。
6. 生产重启若要继续未完成成员，须启用 `recover_queue` 并在恢复前注册所有阶段。本示例演示完成后的状态重开；未完成批次的真实启动恢复另由框架 MySQL 回归覆盖，调度状态不能替代原有 worker 恢复。

批次进度通过任务 Meta 的 JSON 查询，历史任务量会影响扫描成本。这里验证单进程轮询与状态重开，未提供分布式租约、cron、失败批次自动跳过或完整业务数据恢复。
