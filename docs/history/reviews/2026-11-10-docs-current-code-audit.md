# 当前代码文档核对记录

<!-- version-scope -->
> **适用版本/阶段：Papa v4 演进过程中的历史工作区快照（具体基线以正文为准；未记录精确发布版的条目不推定版本号）。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 当前使用方式以 [v4 手册导航](../../README.md) 为准。
<!-- /version-scope -->


核对日期：2026-11-10。基准：本地 `1c05e38`（提交说明 v4.4.1）及其工作区代码；没有据此断言远端标签或 `@latest` 的内容。检查覆盖 `docs/` 的全部 16 份顶层手册、原有变更记录、历史清理计划和 `embed.go`。

## 逐份检查

| 文档 | 对照源码 | 处理结果 |
| --- | --- | --- |
| README | cmd/papa/main.go、templates/app_run.go.tmpl | 新布局入口路径、阅读导航；补动态调度与浏览器诊断入口，历史盘点降为按需工具 |
| CLI | cmd/papa/main.go、strategy_migrate.go、templates/ | 修正旧版安装阻断描述、view 路径、模型密钥自动生成说明 |
| CORE_CONFIG | config/config.go、validate.go、internal/database/migrate.go | 核对配置字段和六类框架模型；修正目录、系统管理入口、超时表格和 AutoMigrate 边界 |
| MONITOR | internal/app/app.go、rod_diagnostics*、admin/tokenadmin/api.go、admin/server/static/ | 清除规则发布/模型表单/待审批恢复旧描述；补 rotate API、浏览器诊断入口及启动失败时的限制 |
| DEVELOPMENT | Makefile、internal/app/、admin/server/ui_behavior_test.go、cmd/papa/ | 清理已删除测试引用和策略迁移 P0 计划；补诊断文件、令牌更新和历史盘点命令 |
| FETCHER_WRITING_GUIDE | engine/、internal/app/spec.go、cmd/papa/templates/ | 删除旧“路径 B”标题；示例目录统一至 internal/，修正初始化顺序保证的过度表述 |
| DATA_LANDING_GUIDE | models/crawler_task.go、internal/database/migrate.go、engine/ | 更新业务模型目录；保留结果与调度分离、幂等写入和旧结果列迁移提醒 |
| ERROR_QUEUE | engine/errorqueue.go、config/validate.go | 查询状态、再处理上限、NoRetry 与队列重投边界保持一致，无需改动 |
| RECOVER_QUEUE | engine/recoverqueue.go、persistence.go | 保留启动时间边界、异步恢复和幂等要求；修正已删除 content 列的举例 |
| REPEAT_QUEUE | engine/repeatpoll.go | 修正失败投递说明中“只捞 failed”，实际查询 success/failed |
| TASK_FLOW | engine/taskflow.go、taskflow_policy.go、admin/server/taskflow.go | 保留仍有代码的可选调度能力；清除工作台方案确认和规则发布叙事 |
| OBSERVABILITY | pkg/events/、obs/、cmd/papa/report.go | 保留事件与报告口径；将多副本共享日志建议改为单实例范围 |
| ACCOUNT_RUNTIME | pkg/account/、engine/account.go | 保留真实账号 HTTP/Rod 登录态能力，去除旧模型/发布功能的否定式残留 |
| CRAWL_TOOLKIT | pkg/discovery/、pkg/extract/、pkg/browser/record.go、pkg/egress/ | 保留通用采集工具；明确链接过滤与当前网络出口约束的关系 |
| TEMPORARY_RESOURCES | pkg/temporary/、pkg/testdb/ | 改为通用临时资源与已授权清理说明，去除模型候选和实验平台后续规划 |
| STRATEGY_MIGRATION | pkg/strategymigrate/inspect.go、cmd/papa/strategy_migrate.go | 改为历史表只读盘点，明确标签不是当前模型注册清单或后续路线图 |

`issues/changes.md` 是历史变更事实，不把旧名称机械删除。`superpowers/plans/2026-11-09-removed-features-cleanup.md` 已标为完成的历史记录。`embed.go` 的注释按实际 `*.md` 范围修正：仅顶层手册被分发，两个历史目录不进入业务项目的框架手册。

## 保留项及边界

- HTTP Cookie 会话、账号登录态、User-Agent 和 robots 的 agent 参数均为现行通用能力，不属于已删除的会话 Agent。
- 历史表名仍存在于只读盘点工具中；未改动工具或数据库。新项目无需执行历史盘点，缺表不要求补建。
- `.papa-model.key` 仍在脚手架 `.gitignore` 模板中，本次如实说明它只是历史忽略项；当前代码不自动生成模型主密钥。
- 本次仅更新 Papa 文档及嵌入注释，没有覆盖业务模块现有文档。业务框架副本可使用当前 CLI 的 `papa upgrade <目录> --write` 刷新。

## 验证

- 递归检查 Markdown 本地文件链接：无失效链接。
- 核对文档中显式框架源码路径；生成项目路径与占位路径另按脚手架核对。
- `config/config.go` 的 mapstructure 字段名在 CORE_CONFIG 中均有覆盖；这是字段覆盖检查，不等同于所有组合配置的运行验收。
- `GOWORK=off go test ./cmd/papa ./docs` 通过，包含脚手架生成、构建及 upgrade 保留应用文档的现有测试。
- `git diff --check` 通过。
- 未执行生产采集、数据库迁移、Git 写操作。本次没有运行完整爬虫测试套件，也没有逐个执行手册中的真实站点示例。
