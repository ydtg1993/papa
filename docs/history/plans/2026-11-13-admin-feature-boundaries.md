# Admin 功能职责拆分实施计划

<!-- version-scope -->
> **适用版本/阶段：Papa v4 演进过程中的历史工作区快照（具体基线以正文为准；未记录精确发布版的条目不推定版本号）。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 本文为设计/计划记录，不能仅凭计划步骤或复选框认定已实现或已验收。 当前使用方式以 [v4 手册导航](../../README.md) 为准。
<!-- /version-scope -->


用户已确认实施，保留公开 API 与行为、现有工作区和暂存内容，不提交。

**Goal:** 将 admin/server 收敛为兼容门面与后台装配，前后端按功能拥有依赖、状态和生命周期。
**Architecture:** server 通过窄接口组合 access、monitoring、queues、breakers、taskflow、tasktrace、logs、reports；shell 管理页面外壳、会话与模块挂载，功能页面管理自身状态。资源通过功能包嵌入，原 URL 和导出变量保留兼容。
**Tech Stack:** Go HTTP、go:embed、现有无构建 JS/CSS、Node 行为测试、Chromium。

- [x] 保存本轮基线和现有测试覆盖，禁止 Git 写操作。
- [x] 将来源控制与鉴权状态交给 access，Monitor 保留 NewMonitor/Register/Auth/SetWhitelist 等公开入口并委派；功能 handler 不依赖 server 或整个 MonitorConfig。
- [x] 独立 monitoring、queues、breakers、taskflow 的 HTTP 处理、参数验证、审计依赖，保持原有路由与 wire behavior；兼容 TaskFlowScript/CSS 与事件类型。
- [x] 独立 logs、reports、tasktrace 的文件读取、安全路径检查、下载和接口测试，仅注入该功能依赖。
- [x] 前端 shell 提供会话、请求有效性、导航及生命周期；日志、报告、令牌、设置、监控与熔断功能拥有自身控制器和状态，不保留万能 mo.js。已有全局扩展 API 由兼容门面提供。
- [x] 模板按功能片段组合，资源放所属功能包；shell 仅外壳与共享 UI。旧静态 URL 继续可用；首帧主题初始化保留。
- [x] 迁移测试时保持覆盖，新增独立 handler、模块卸载/换凭据/迟到响应及依赖边界回归。
- [x] 更新 DEVELOPMENT/MONITOR/ARCHITECTURE，运行 gofmt、Node、Go 全量测试/race/vet/build、实际 Chrome、隔离 MySQL及 Grab 兼容构建。
- [x] 核对原始改动保留、公开 API、资源与路由兼容，记录真实验证结果。

## 编辑范围

后端装配负责人编辑 server Go 核心与 access/monitoring/queues/breakers/taskflow；文件接口负责人编辑 logs/reports/tasktrace，向装配负责人提供构造参数和迁移清单；前端负责人编辑前端脚本/模板/样式及资源嵌入，和装配负责人协调资源出口。主助手维护文档、边界守卫和整体集成验证。独立范围可并行；任何共享文件先协调。

验收不能仅凭文件数量下降：功能组件不得持有 Monitor 或完整配置，不得通过全局共享可变状态取依赖；回归测试验证功能行为和兼容入口。
