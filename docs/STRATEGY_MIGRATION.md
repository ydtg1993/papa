# 会话、试验与正式策略：迁移预检

本命令用于历史版本的迁移预检：清单中的 `crawler_agent_*` / `crawler_rule_*` 是待盘点的旧表名，`crawler_strategy_*` 是规划目标，不表示当前框架仍提供这些运行模块，也不会重新注册或创建这些表。

当前阶段为**P0设计与只读盘点**。统一策略存储、导入、切换、旧表删除尚未实现；本页不是已经迁移完成的操作说明。

目标是会话组织需求，试验冻结一份方案/代码/配置并定量验证，正式版本保存同一份已通过验证的完整策略包。规则不再单独发布；正式发布和启动仍是两个不同的人工确认动作。

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

当前没有apply/import/drop/--write选项。普通`papa migrate`仍只增不减，不执行旧表清理。该命令不会启动业务应用、Agent、浏览器、任务或发布。

## 后续迁移边界

预期采用：盘点 → 完成替代存储与业务适配 → 维护窗口停止写入 → 数据库/文件一致备份 → 导入和内容/引用核验 → 一次切换 → 验收后精确删除旧表/旧逻辑。不长期双写，也不永久通过旧表fallback读取。

任务、Trace、调用审计、计量、周期状态和质量观测不是独立规则发布实体，不按crawler_rule_前缀一刀切。历史源码与规则只有半次发布的情况，不通过迁移自动补成正式版本。试验数据清理继续使用[TEMPORARY_RESOURCES](TEMPORARY_RESOURCES.md)，不能清理正式版本运行所需内容。
