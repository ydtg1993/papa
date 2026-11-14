# Trace 真实 MySQL 迁移与旧数据验收

<!-- version-scope -->
> **适用版本/阶段：v4 工作区切换文件 Trace、完成独立版本清理之前的 SQL Trace 阶段（已替代）。**
> 正文的旧表升级、历史行兼容与测试名仅是当时证据；当前 v4 不提供 SQL Trace 模型、迁移或读取入口，请勿按本文对当前数据库施工。 当前使用方式以 [v4 手册导航](../../../README.md) 为准。
<!-- /version-scope -->


日期：2026-11-13。返回 [诊断](README.md) / [复审记录](rereview.md)。本记录补齐前次未执行的 Trace 真实数据库验证，不改写历史测试批次的跳过结果。

## 隔离范围

使用现有配置提供的连接凭据，通过 PAPA_RUNTIME_TEST_CONFIG 交给仓内 compensationMySQL 测试辅助函数。连接前清空 DSN 数据库名；仅独占创建 papa_compensation_test_<随机 UUID> 数据库，创建失败不会复用或删除任何已有库。所有合成数据、建表和迁移都在该新库内执行，测试结束通过 pkg/testdb 的清理回调删库。没有查询或修改业务表，也没有执行业务迁移入口。

实际服务版本 SELECT VERSION() 返回 **MySQL 8.1.0**。增强用例普通执行一次、race 连续三次，各使用不同测试库；结束后通过 information_schema.schemata 对日志中四个确切库名做只读检查，全部不存在。最初未增强用例亦通过并正常完成清理回调。日志不含连接凭据。

## 用例与结果

`engine/trace_mysql_test.go` 的 `TestTraceMySQLLegacyMigrationAndLatestTail`：

1. 在独立库创建 Trace 表，再移除 execution_id 与三个新索引，构造本次升级前表结构；旧表就绪检查必须报错。
2. 插入 551 条合成历史行，attempt/seq 故意反复归零，含中文信息、JSON、状态、耗时、时间与 ID；保存全部字段快照。
3. 连续两次调用生产 database.Migrate。每次验证历史快照完全相同，execution_id 为空；实际列为 varchar(36)、NOT NULL、空字符串默认值。
4. 从 information_schema 校验三个新增索引及保留的 idx_task_attempt_seq 的实际列顺序。
5. 历史查询按插入 ID 返回末 500 行，身份未知，只在首步标记截断，不根据重复 attempt/seq 猜测轮次。
6. 写入另一个执行的 551 步，跨两个 attempt；返回第 51–550 步共 500 条，保留最后失败步骤，不混入旧行；旧 ListTrace 返回值与详细接口的原字段逐一相同。
7. 再写一条更新执行，结果只包含它且不标记截断；最终持久记录仍有 1103 条，查询不删除历史。

以上均通过，未发现新的生产实现缺陷。本次只增强回归测试和文档，未改变公开 API、锁序、取消或排空实现。

## 验证命令与证据

环境：Windows、GOWORK=off，race 使用 CGO_ENABLED=1；连接配置路径通过环境变量传入，不将凭据写进命令或报告。

- `go test ./engine -run '^TestTraceMySQLLegacyMigrationAndLatestTail$' -count=1 -v -timeout 180s`：通过，未跳过。
- `go test -race ./engine -run '^TestTraceMySQLLegacyMigrationAndLatestTail$' -count=3 -v -timeout 180s`：三次通过，未跳过，无 DATA RACE。
- `go test ./internal/database ./pkg/testdb ./core ./engine -run 'Trace|DisposableDatabase' -count=1`：通过；该额外离线批次未设置连接，真实用例的通过证据以上两批为准。
- `go vet ./engine ./internal/database ./pkg/testdb`：通过。
- `gofmt`、`git diff --check`、报告相对链接检查：通过。

原始日志：Papa 工作区 `.review-tmp/trace-mysql/initial.txt`、`expanded.txt`、`race.txt`、`regression.txt`、`vet.txt`、`cleanup.txt`。前次全仓 test/race/vet/build 记录继续见复审报告；本次未因仅测试和文档变化重复全仓长测。

## 仍需区分的边界

已验证本次缺列旧结构的实际升级、重复迁移和数据兼容；不等于所有历史 schema 漂移均兼容，也未测生产规模 DDL 锁等待、在线迁移、断电中断恢复或其他 MySQL 版本。业务库没有被迁移，实际升级时仍需用户按显式迁移流程操作。

所有既有工作区改动保留，未执行 Git 写操作。
