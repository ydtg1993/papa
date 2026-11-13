# 抓取接入与数据落地

> 适用版本：当前独立 Papa v4。返回 [文档导航](../README.md)。

按 Fetcher → 业务结果 → 可选工具的顺序接入站点。

| 手册 | 职责 |
| --- | --- |
| [Fetcher 编写](FETCHER_WRITING_GUIDE.md) | 站点/阶段、Task 身份、入口提交、HTML/Rod 抓取、错误与子任务 |
| [数据落地](DATA_LANDING_GUIDE.md) | 业务模型、幂等结果、写入辅助及显式建表；区分任务状态与业务数据 |
| [抓取工具](CRAWL_TOOLKIT.md) | 网络录制、发现、分页、增量和数据校验，以及调用方责任 |

真实站点、业务模型与验收材料归独立业务模块。账号登录与浏览器身份见 [账号与身份](../accounts/README.md)，多阶段运行策略见 [运行调度](../runtime/README.md)。
