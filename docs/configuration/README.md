# 配置与资源预算

> 适用版本：当前独立 Papa v4。返回 [文档导航](../README.md)。

| 手册 | 职责 |
| --- | --- |
| [配置字典与建表](CORE_CONFIG.md) | YAML 键、默认值、严格校验、显式迁移、Trace 三模式与文件保留 |
| [单实例资源预算](RESOURCE_BUDGETS.md) | Go API 装配、输入限额、有界调度、下载和报告预算、背压及取消边界 |

配置字典说明启动参数；运行期间的策略覆盖见 [运行治理](../runtime/RUNTIME_CONTROL.md)。资源预算 API 不等同于 YAML 键，也不等同于操作系统内存/磁盘硬配额。
