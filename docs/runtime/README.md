# 任务运行、恢复与治理

> 适用版本：当前独立 Papa v4。返回 [文档导航](../README.md)。

| 手册 | 职责 |
| --- | --- |
| [任务时间](TASK_TIMING.md) | NotBefore/Delay、开始结束记录、UTC 与计时边界 |
| [任务流](TASK_FLOW.md) | 多阶段广度/深度、并发屏障、父子关系和策略更新 |
| [错误队列](ERROR_QUEUE.md) | 失败补偿、人工重投、版本条件与次数上限 |
| [启动恢复](RECOVER_QUEUE.md) | processing 孤儿恢复及其与 pending 持续补充的区别 |
| [周期轮询](REPEAT_QUEUE.md) | repeatable、任务周期、轮次和到期扫描 |
| [分类熔断](BREAKER.md) | 可靠保护、失败样本、串行验证、持久状态和恢复 |
| [运行治理](RUNTIME_CONTROL.md) | 请求预算、AutoThrottle、资源准入、运行策略与账号池选择 |
| [临时资源](TEMPORARY_RESOURCES.md) | 资源归属、排空后清理、删除意图恢复和独立测试库 |

治理队列仍是全局配置；多站点/多阶段在单实例内协作。输入和驻留限额见 [资源预算](../configuration/RESOURCE_BUDGETS.md)，组件所有权见 [目录说明书](../package_structure/README.md)。
