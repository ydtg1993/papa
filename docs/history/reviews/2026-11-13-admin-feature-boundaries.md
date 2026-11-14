# 后台前后端功能职责拆分

<!-- version-scope -->
> **适用版本/阶段：Papa v4 演进过程中的历史工作区快照（具体基线以正文为准；未记录精确发布版的条目不推定版本号）。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 当前使用方式以 [v4 手册导航](../../README.md) 为准。
<!-- /version-scope -->


## 范围与兼容约束

按已确认方案调整 admin/server，保留现有工作区和暂存内容，不执行 Git 写操作。原有 MonitorConfig 字段、NewMonitor/Register/Auth/SetWhitelist、事件类型、TaskFlowScript/CSS、扩展注册 API、HTTP 路由与 JSON 行为保持；业务继续使用已有 Papa 页面/设置与公共请求、弹窗工具。

本轮基线保存在 `.review-tmp/admin-boundaries-baseline`，原有 91 个后台测试声明均保留。旧前端源码在迁移后归档至 `.review-tmp`，未覆盖原始改动。

## 实际职责

| 组件 | 拥有的职责与状态 |
| --- | --- |
| server | 公开兼容门面、组件构造和统一路由鉴权；不持有整个配置副本、白名单锁、实例标识或模板缓存 |
| access / settings | access 管理白名单和鉴权、保存后发布；settings 管理设置展示和显式关停，通过快照接口读取白名单 |
| monitoring / queues / breakers | 监控响应及实例身份、队列操作、熔断控制和分析；各自只接收所需回调，前端控制器管理相应视图 |
| logs / reports / tasktrace | 日志、报告、追踪与归档的参数校验和文件边界；前端拥有选择、请求代次与清理逻辑 |
| taskflow / tokenadmin | 各自后台接口及编辑器/令牌页面资源；策略及凭据持久化留在既有实现 |
| shell | 外壳模板、公共会话/导航/请求/UI、控制器注册和生命周期、只读资源组合 |

功能包不导入 server，不接收 Monitor 或完整 MonitorConfig。前端工厂接收明确依赖；报告状态、日志选择、请求计数等是实例私有变量，不通过全局 getter/setter 回写。跨模块使用语义查询/操作接口；既有公共函数通过兼容桥继续可用。

## 资源和行为兼容

- JS/CSS/HTML 源码已从 server 移到所属功能包。外壳组合功能 views，保留首帧主题初始化。
- `/static/mo.js` 为确定顺序组合的控制器工厂；`/static/mo.css` 为共享样式与功能样式的兼容组合，输出内容与本轮原始 CSS 完全一致。
- 原有静态 URL、字体范围读取、缓存头和鉴权保持；功能资源另有明确 owner 路径，不发布含糊的 `/static/controller.js`。
- 资源 FS 不使用生产 testing/fstest，构造后只读；读取返回独立字节副本，缺失片段和重复归属会在装配时报错。

## 回归与验证

- 新增全功能路由的 GET/POST 鉴权覆盖、旧静态 URL/MIME/缓存验证、FS 契约/字体 range/缺失资源验证。
- 迁移 Node 测试执行真实控制器工厂，保留导航、草稿保护、账号切换、轮询、表格兼容、图表、日志、报告和熔断场景。
- 新增两个报告/日志实例隔离，以及卸载/reset 后迟到 HTTP、正文、正文错误的 12 种组合，验证生命周期由控制器自身维护。
- 新增完整页面 Chrome 场景发现依赖注入使原生 fetch 的接收者变为 deps，报 `Failed to execute 'fetch' on 'Window': Illegal invocation`。已让平台 fetch 绑定正确浏览器环境；普通功能依赖仍通过所属控制器解析。

最终验证全部通过：

- `GOWORK=off go test -p 2 ./... -count=1 -json`：75 个包通过，2632 个测试/子测试通过；默认环境跳过的外部依赖用例在下面的启用依赖运行中补齐。
- 启用 MySQL、Chrome、FFmpeg 与 live Rod 环境执行 `go test -race -p 2 ./... -count=1 -json`：75 个包通过，2711 个测试/子测试通过，无失败或数据竞争。日志分别为 `.review-tmp/admin-final-tests.jsonl` 与 `.review-tmp/admin-final-race.jsonl`。
- race 中跳过的 `TestAuthenticationMySQLEmptyCreateDisableDelete` 和 `TestFrameworkExistingMySQLReadOnly` 已通过独立临时数据库补跑 race，结束后清理本轮数据库，结果见 `.review-tmp/admin-final-optional-mysql.log`。`TestWriteKeepsFileMode` 因 Windows 不支持 Unix chmod 语义跳过。4 个无测试包不计入通过包数。
- `go vet ./...`、`go build -mod=readonly ./...`、Grab 兼容构建、52 个 JS 文件语法检查、Node 行为测试及完整页面 Chrome 回归通过；236 个本轮涉及的 Go 文件格式检查无遗漏。
- 原有 91 个后台测试声明保留，公开配置字段与既有路由/资源兼容检查通过，CSS 组合结果与基线一致；职责边界守卫通过。

保留原有工作区与暂存改动，未执行 Git 写操作。未执行真实站点业务验收或部署。
