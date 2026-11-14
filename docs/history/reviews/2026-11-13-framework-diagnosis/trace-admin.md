> 已取消：用户决定仅保留 off/errors/all 三模式配置，不实施主动按钮与待选状态。以下为历史方案，不代表当前实现。

# Trace 后台主动追踪

<!-- version-scope -->
> **适用版本/阶段：v4 文件 Trace 过渡期的后台主动追踪方案（已取消）。**
> 手动按钮、待选状态及兼容空壳均不属于当前能力；当前仅配置 off/errors/all，默认 errors，见 [配置手册](../../../configuration/CORE_CONFIG.md)。 当前使用方式以 [v4 手册导航](../../../README.md) 为准。
<!-- /version-scope -->


2026-11-13，承接纯文件 Trace：已增加任务行登记/取消下一执行追踪及详情状态刷新。流程与边界见 ../../MONITOR.md 的主动追踪章节。

任务动作属于 admin/tasksource，详情属于 admin/tasktrace；App/server 仅装配。Engine.traceService 继续拥有有界选择表和锁；新增快照/取消接口，不改 TaskActions、Options、MonitorConfig 的已有公开布局。选择、取消和消费串行，已捕获执行策略不被之后取消改写。

登记不调度任务、不修改任务行、不创建 Trace 数据表；仍需先开启 Trace。待生效期间重复点击不续期。状态只回答是否待生效，不伪装成已完成或正在追踪。文件容量满、损坏、过期和重启等原边界继续适用。原 SQL 表和业务数据不操作；现有改动和用户暂存图片保留，不提交。

验证结果收尾补录。前端由 Node 执行生产控制器，未进行真实部署或浏览器人工点击验收。
