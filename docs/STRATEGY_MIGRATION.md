# 历史表只读盘点

> 面向：升级后需要核对历史数据库遗留表的维护者。新项目无需运行此命令。

当前代码保留 `papa strategy-migrate inspect` 只读工具。`pkg/strategymigrate.FrameworkTables()` 中的 `crawler_agent_*`、`crawler_rule_*` 和 `crawler_strategy_*` 是历史盘点清单，不是当前模型清单；命令不会注册、创建或恢复这些表及功能。

本工具只统计表是否存在与行数。历史清单中的 `retain`、`adapt`、`target`、`retire_after_verified_migration` 仅是既有报告标签，不代表当前版本要求保留、改造、创建或删除对应模块，也不是后续开发路线图。

## 只读命令

```text
papa strategy-migrate inspect -c configs/config.yaml --manifest docs/business/strategy-migration-inventory.json --out artifacts/strategy-inventory-new.json
```

`-c`也可写`--config`；省略时沿用CLI配置查找。`--manifest`可省略，只盘点框架表；业务自己提供清单，Papa不内置任何业务表或站点。`--out`可省略，此时JSON输出到stdout；指定文件必须不存在，其父目录应已存在。报告无原始数据、消息、凭据或DSN。

业务manifest示例：

```json
{
  "version": 1,
  "tables": [
    {"name":"example_discussions","disposition":"retire_after_verified_migration"},
    {"name":"example_items","disposition":"retain"}
  ]
}
```

表名仅接受小写SQL标识符，不接受SQL、重复表名或覆盖框架清单。包括框架表最多64项，manifest最多64KiB。disposition只能为retain、adapt、target或retire_after_verified_migration，**全部是规划标签，不会执行相应写操作**。

命令仅连接MySQL开启只读事务，30秒内查询表是否存在与COUNT。`exists=false,rows=null`表示缺表；`exists=true,rows=0`表示空表。报告的migration_ready始终false：行数不能证明包摘要、文件、跨表引用、当前指针、活动作业和备份均已核对。数据库异常不会被当作空库继续。

当前没有apply/import/drop/--write选项。普通`papa migrate`仍只增不减，不执行旧表清理。该命令不会启动业务应用、浏览器或采集任务。

## 如何使用报告

缺表是有效结果，不需要为满足清单而创建表。`migration_ready=false` 不表示当前爬虫不可启动；它表示行数盘点无法证明任何数据迁移已完成。

清理遗留表前，由数据库维护者核对现有业务引用并备份，再执行单独审阅的 SQL。本命令没有删除能力，也不能依据表名前缀批准删除。当前框架建表由 `App.Migrate` 的模型注册与配置开关决定，见 [核心配置](CORE_CONFIG.md)；业务模型由业务模块登记。
