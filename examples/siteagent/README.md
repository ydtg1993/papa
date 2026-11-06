# 受控网站分析示例

本示例启动自己的回环 HTTP 网站，经过真实的受控访问、规则仓库、独立 fixture 验收与 MySQL 审计，生成待审核候选。默认模型是确定性 `Replay`，不需要 Key，也不访问外部网站或模型。它验证接线与安全边界，不代表真实模型的规则生成质量。

在 papa 模块目录执行。先创建专用 MySQL 数据库；示例不会创建数据库，`-migrate` 才会显式创建/补充框架表。样本目录应保留，供后续读取证据和验收。

```powershell
$env:GOWORK = 'off'
$env:PAPA_AGENT_DSN = 'user:password@tcp(127.0.0.1:3306)/papa_agent_example?charset=utf8mb4&parseTime=True&loc=Local'
go run ./examples/siteagent -migrate -sample-dir ./siteagent-samples
# 以后不加 -migrate；普通分析不会自动迁移。
go run ./examples/siteagent -sample-dir ./siteagent-samples
```

默认 `-render=true`，需要已安装的 Chromium/Chrome/Edge；可用 `-browser` 指定可执行文件。示例覆盖四个阶段：

- `list`：静态列表提取标题和详情 URL，声明 list → detail 关联。
- `detail`：静态详情提取标题和摘要。
- `rendered`：页面自身脚本生成 DOM，经独立受控浏览器提取渲染快照。
- `json`：渲染 `/json` 时记录真实发生的匿名 GET JSON，再获取第二页，验证 JSON Pointer 和下一页 URL。

`-render=false` 运行三个 HTTP 阶段，JSON 直接通过匿名 GET 取证；该模式不验证浏览器路径。所有网站请求仅允许本次本地服务的明确 origin/路径，`AllowPrivateNetwork` 只为这个回环 origin 开启。样本正文放行策略也只适用于示例自己编写的公开内容，接实际网站时须换成合适的脱敏策略。

训练与独立验收样本由宿主预先编写，正文、URL 和期望值各自独立。模型不能修改这些期望；独立样本不进入模型上下文。规则 ID/版本由宿主分配。成功结果是 `awaiting_review`、`verified=true`，包含 run ID、候选引用、证据引用、usage 和重新打开后的审计步骤数；`unpublished=true` 表示仓库仍没有当前发布版本。示例从不调用 Publish、Rollback、业务 Save 或任务提交。

## 显式启用真实模型

仅设置环境变量不会启用模型调用，必须传 `-real-model`。模型供应商和模型名称由操作者选择。

```powershell
$env:PAPA_AGENT_MODEL_BASE_URL = 'https://your-service.example/v1'
$env:PAPA_AGENT_MODEL = 'your-selected-model'
$env:PAPA_AGENT_MODEL_KEY = '<set privately in your environment>'
go run ./examples/siteagent -sample-dir ./siteagent-samples -real-model
```

也可用 `-model-base-url`、`-model`、`-api-key-env` 指定端点、模型和密钥环境变量名；不要把密钥写进参数。客户端只在 BaseURL 后追加 `/chat/completions`，不猜测 `/v1`，不自动重试、跟随重定向或继承代理。默认发送 `max_completion_tokens`；旧服务须显式传 `-legacy-max-tokens`。该参数是否受支持需对所选服务确认。

运行预算是最多 20 次模型调用、每次最多 4096 输出 token、180 秒分析时间；输出 token 上限可能包含推理 token。未配置单价，费用标为未知。真实模型可能因候选不合格、预算耗尽或服务错误结束；查看输出中的状态、原因与审计，不应把运行结束等同于验收通过。本次测试只使用回放，没有验证真实模型质量或实际计费。

## 查看记录与显式中断

完成后，示例重新打开仓库/样本目录与审计适配器，确认候选和步骤可读。它另外创建一个明确标记为模拟停止的运行：只有一条预留步骤、没有外部请求；确认执行者已结束后将它改为 `interrupted`，未完成步骤改为 `unknown`。输出中的 `interruption_example_id` 指向这个独立演示记录，不会重发模型请求。

```powershell
# 只读取；不需要样本目录，不执行模型或网站请求。
go run ./examples/siteagent -inspect-run '<run ID from output>'

# 仅在宿主确认旧执行者已结束后使用。
go run ./examples/siteagent -interrupt-run '<unfinished run ID>' -worker-stopped
```

重启后不会自动接管或重放旧运行。检查旧候选和证据后，可创建新分析运行；发布仍由操作者使用既有发布 API 审阅执行。

## 验证

```powershell
go test ./examples/siteagent -count=1
# 数据库名必须包含 agent_test；使用独立、可丢弃的测试数据库。
$env:PAPA_AGENT_EXAMPLE_TEST_DSN = 'user:password@tcp(127.0.0.1:3306)/papa_agent_test_example?charset=utf8mb4&parseTime=True&loc=Local'
go test ./examples/siteagent -count=1 -v
$env:CGO_ENABLED = '1'
go test -race ./examples/siteagent -count=1
```

未提供测试 DSN 时，只运行离线 fixture、回放默认值和参数保护测试，真实 MySQL/Chromium 集成测试会明确跳过。提供 DSN 后会实际运行 HTTP 与受控浏览器两条路径、独立验收、重复只读检查及中断审计；缺少 Chromium 会失败。测试不调用任何真实模型。示例保留不可变数据库记录；请由操作者管理专用数据库和长期样本目录。
