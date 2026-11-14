# obs 辅助报告实施计划

<!-- version-scope -->
> **适用版本/阶段：独立 Papa v4 的 obs 辅助报告设计/实施阶段（2026-11-14 修订定位）。**
> 在线信号旁路与显式离线分析的当前契约见 [观测手册](../../operations/OBSERVABILITY.md)；正文测试与实施步骤保留其当次适用范围。 本文为设计/计划记录，不能仅凭计划步骤或复选框认定已实现或已验收。 当前使用方式以 [v4 手册导航](../../README.md) 为准。
<!-- /version-scope -->


已获用户授权，沿用修订设计；不操作 Git、不动业务库。

- [x] pkg/events 与 Engine 事件边界隔离、载荷预算与 race 回归。
- [x] offline obs.Generate 局部故障保护；可靠熔断事实与报告处理职责核对。
- [x] internal/reportrunner 替换为信号增量聚合与独立有界写入；移除 DB/在线任意回调。
- [x] App 接线、2 秒共享退出预算、旧在线报告测试迁移与故障测试。
- [x] 独立库 App 负载/恢复、全仓、race、vet/build，结构说明及报告同步。

结果及限制见 [实施验证记录](../reviews/2026-11-14-obs-sidecar-implementation.md)。
