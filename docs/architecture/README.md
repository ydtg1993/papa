# 架构与框架开发

> 适用版本：当前独立 Papa v4。返回 [文档导航](../README.md)。

先理解组件与任务循环，再查实现归属和修改规则。

| 手册 | 职责 |
| --- | --- |
| [Papa 六图介绍](PAPA_OVERVIEW.md) | 能力、组件、任务循环、恢复、观测与关停的图片介绍，附矢量图和图源 |
| [核心引擎图解与算法](ENGINE_GUIDE.md) | Engine 内部组件、调度背压、执行结算三张专图及当前算法公式 |
| [图册维护说明](DIAGRAM_ASSETS.md) | 图源生成、产物校验、源码责任与 CLI 分发 |
| [架构图解](ARCHITECTURE_DIAGRAMS.md) | 静态组件图、动态任务数据流，重试、取消及 obs 旁路 |
| [架构规则](ARCHITECTURE.md) | 包依赖、状态所有权、装配和关停契约、变更纪律 |
| [开发手册](DEVELOPMENT.md) | 构建、测试、race、平台要求和脚手架开发 |
| [目录说明书](../package_structure/README.md) | 每个包和重要文件的职责，新增代码的归属 |

此目录说明框架如何组织与维护。业务接入用法见 [抓取接入](../guides/README.md)，部署与后台操作见 [运维观测](../operations/README.md)。
