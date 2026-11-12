# Papa 包职责与变更规则

> 面向：维护框架和接入通用能力的开发者。包与文件索引见 [DEVELOPMENT.md](DEVELOPMENT.md) §6。

## 依赖方向

Papa 是单实例大单体。App 创建组件、注入窄接口并安排启停；后台 handler 解析请求并调用服务；服务拥有状态与原子操作；存储适配器实现持久化。业务站点、固定采集目标和业务模型留在独立业务模块。

- `admin/*` 不导入 `internal/app`；页面脚本与所属 handler 位于同一功能模块。
- `admin/server` 只装配后台组件并保留公开兼容入口；其它 admin 功能包不得反向导入 server。Monitor 不保留完整配置、白名单锁、观测身份或模板缓存副本。
- 后台功能包接收各自窄依赖并拥有 handler、视图片段、控制器及专用样式；`admin/shell` 拥有公共会话、导航、请求及 UI 生命周期。功能控制器只导出语义操作和必要查询，不能把所有私有变量通过 getter/setter 重新变成共享状态。
- `internal/runtimecontrol`、`internal/reportrunner`、`internal/taskstore` 不依赖 HTTP、后台或 Engine 实现。
- `engine/internal/*` 不导入父 engine；类型不适合跨包时先建立同包状态组件，不能制造反向依赖。
- 浏览器诊断中的 `net/http` 用于本地合成 fixture，后台 handler 仍在 `admin/diagnostics`。
- 公开契约与具体实现分开：账号存储使用 `account/contract`，配置与熔断共享 `breakerpolicy`。

## 状态与生命周期

运行策略的 revision、document、effective、uncertain 和互斥锁属于运行策略服务。策略保存和池删除的引用检查必须在同一互斥边界内完成。返回的治理策略快照必须在该发布锁内与 revision 一起捕获，HTTP 只补充独立运行指标，不得解锁后重新拼装策略快照。持久结果未确认时，池删除须先核对持久策略；远程暂停确认须拒绝旧缓存版本，待刷新核对后重试。返回快照不能泄露服务持有的可变 map/指针。

报表组件持有报表专用值配置、ticker、生成串行锁与排空信号，不保留整份应用配置指针；App 在关库前 Stop 并等待正在生成的报告，再产生退出报告。诊断组件持有操作取消、在途计数和合成浏览器资源；所有使用存储的入口（含身份列表 View）都自行进入取消及排空协议，不能依赖某个 HTTP handler 替它登记。

任务存储按 Claim、Complete、Release、Retry、AdvanceRepeat 等语义使用；终态条件包含执行轮次，丢失确认后的核对不能重复追加错误或重放业务 handler。公开 Task 的旧方法保留兼容，执行路径使用可返回错误的入口。执行重试计数是加法写入：失败或确认丢失时只执行一次，记录 unresolved 并停止下一次 handler，保持 processing 等待重启恢复；不盲目重试加法，也不发出已重试成功的事件。

调度组件拥有池和延迟队列，执行持久化组件拥有未确认状态。熔断与暂停、任务持久化之间的协调仍须遵守锁序；独立状态组件不能自行跳过协调器改动其他组件。

## 文件与 API

一个包应能回答“做什么、如何调用、依赖什么、谁负责关闭”。可以在同包按职责分文件，不要求每个文件再建子包。禁止无边界的 utils/service/manager 集合，禁止为减少行数而暴露内部字段。

App 和 Engine 的公开入口是兼容门面，新增功能优先委派到职责明确的组件。已有公开字段和类型别名不能直接移除；`internal` 路径不意味着通过根包别名暴露的字段可以任意修改。

JS/CSS 继续用 go:embed 保持单二进制部署。`custom.js` 的既有顺序、页面 key 和路由维持兼容；跨页面共享脚本必须明确提供者和加载顺序。后台注册接口持有自己的导航元数据快照，不保留调用方可变的 Owns 切片。

`/static/mo.js`、`/static/mo.css` 是兼容资源 URL，不是新增功能的源码落点。资源按明确清单组合，重名资源不能依赖 map 遍历顺序决定归属；缺失模板/样式片段必须在装配时失败。生产资源 FS 只读且不能把共享字节切片暴露给调用方。

## 变更验收

1. 写清状态所有权、接口与允许依赖，复用已有组件。
2. 为独立组件测试行为；App 测试验证装配，不能用删除集成用例换取通过。
3. `GOWORK=off` 下 test/vet/build；并发改动执行 race；SQL 使用隔离 MySQL 验证，页面使用 Node/合成 Chromium。
4. 跑 `TestPackageResponsibilityBoundaries`，更新 DEVELOPMENT 的实际文件索引。
5. 保留工作区原有修改，禁止 Git 写操作；由用户 review 和提交。
