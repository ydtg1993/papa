# 持久规则发布示例

在独立 MySQL 数据库和本地 HTTP 测试站点上运行：草稿 → 样本集 → 验收 → 发布 v1 / v2 → 差异与影子评估 → 重建仓库 → 旧任务继续 v1 → 新任务使用 v2 → 回滚。

PowerShell：

```powershell
$env:GOWORK = 'off'
$env:PAPA_RULE_DSN = 'user:password@tcp(127.0.0.1:3306)/papa_rule_test_example?charset=utf8mb4&parseTime=True&loc=UTC'
go run ./examples/rulepublish -sample-dir ./tmp/rulepublish-samples -migrate
```

数据库必须事先创建。`-migrate` 是显式建表选项；不带时只运行流程，缺表会报错。示例使用唯一 ID 添加自己的记录，不删除已有记录；每次运行都会增加演示数据，请只使用独立测试库。数据库与样本目录需一起备份、保留。

输出应包含：4 个已验收版本、发布代数 `[1,2,3]`、旧任务 `Alpha`、新任务 `Beta`、回滚后 `Alpha`、恢复的 3 条发布历史，以及样本恢复和子任务版本固定成功。Shadow 只比较同一份已取得页面，不触发 Save 或任务提交。

示例中的正文原样保留回调仅针对程序自己生成的公开测试 HTML，不是通用生产脱敏方案。数据库 DSN 从环境变量读取，不写入规则和样本。

真实集成回归可用 `PAPA_RULE_EXAMPLE_TEST_DSN` 指定名称包含 `rule_test` 的独立库，执行 `go test ./examples/rulepublish -count=1`。生产接入、发布冲突和样本保留边界见 [RULE_STORE.md](../../docs/RULE_STORE.md)。
