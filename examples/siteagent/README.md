# 受控网站分析示例

本示例启动自己的回环 HTTP 网站，经过真实的受控访问、规则仓库、独立 fixture 验收与 MySQL 审计，生成待审核候选。默认模型是确定性 `Replay`，不需要 Key，也不访问外部网站或模型。它验证接线与安全边界，不代表真实模型的规则生成质量。

即使启用 `-real-model`，页面仍是这个自建测试站。真实网站的站点逻辑、独立预期、供应商接线和验收记录应放在独立业务项目，通过 Papa 公开 API 接入；不能用本地回放代替真实站点验收，也不能将真实站点业务放入框架示例目录。

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

每个阶段的训练集、独立验收集现在各有一个正样本和一个 `Kind: "empty"` 负样本；空 HTML/JSON/DOM 预期由宿主预先定义，正文标记互不相同，避免训练与隐藏样本重复。模型不能修改类型或预期。原 HTTP 14 次、浏览器 18 次回放决策和 180 秒总预算不变；这只是本地合成验收示例，不是 D3 固定模型质量评测。

浏览器场景在 800ms 后创建 `section.card > h2`，回放动作使用 `wait: {selector: "section.card > h2", state: "visible", timeout_ms: 3000}` 等待后再取证。等待仍受原 180 秒总预算约束，不增加模型调用次数；HTTP 和 GET JSON 录制动作保持原协议。详细条件、错误与审计口径见 [SITE_AGENT](../../docs/SITE_AGENT.md#同次渲染中的有界等待)。示例只包含本地合成站点，真实网站策略留在业务模块。

```powershell
go test ./examples/siteagent -count=1
# 数据库名必须包含 agent_test；使用独立、可丢弃的测试数据库。
$env:PAPA_AGENT_EXAMPLE_TEST_DSN = 'user:password@tcp(127.0.0.1:3306)/papa_agent_test_example?charset=utf8mb4&parseTime=True&loc=Local'
go test ./examples/siteagent -count=1 -v
$env:CGO_ENABLED = '1'
go test -race ./examples/siteagent -count=1
```

未提供测试 DSN 时，只运行离线 fixture、回放默认值和参数保护测试，真实 MySQL/Chromium 集成测试会明确跳过。提供 DSN 后会实际运行 HTTP 与受控浏览器两条路径、独立验收、重复只读检查及中断审计；缺少 Chromium 会失败。测试不调用任何真实模型。示例保留不可变数据库记录；请由操作者管理专用数据库和长期样本目录。

受控渲染专项验证（在 Papa 模块目录）：

```powershell
go test ./pkg/webaccess -run '^TestRealChromium' -count=1 -v
$env:CGO_ENABLED = '1'
go test -race ./pkg/webaccess -count=1
# 下项使用另一套专用库：PAPA_AGENT_MYSQL_TEST_DSN 的数据库名必须是 papa_agent_test。
# 在本机安全设置 DSN 后运行，不把密码写进文档或命令历史。
go test -race ./pkg/crawlagent -run '^TestRunnerMySQL' -count=1 -v
```

真实浏览器回归包含空正文页面/脚本/重定向、请求/DOM/总字节/页面/重定向预算，以及实际请求开始后的取消、会话关闭/期限到期和全局名额释放。Runner 真库回归另验证渲染超限/取消后的审计收尾、训练证据保留、没有伪成功渲染证据、没有自动发布。

D1 另覆盖延迟出现、隐藏后显示、首个匹配语义、隔离世界抗网页函数覆盖、等待超时、等待中取消/会话关闭/期限、网络策略和预算退出、排队取消与名额复用。`TestRunnerMySQLChromiumWaitReopen` 在真实浏览器和专用 MySQL 上验证条件成功后的实际 DOM、候选与审计重开，以及等待超时不新增成功页面证据、不自动发布。

Windows 启动失败时，先保留系统默认 `TEMP/TMP/GOTMPDIR` 做环境对照；本机将三者或仅 GOTMPDIR 指向工作区目录时均无法取得 Chrome 调试连接，具体宿主限制尚未确定。不要以关闭沙箱或降低出口控制来让测试变绿。成功的回放验收证明框架接线与约束，不能替代真实模型生成质量评测。

## D3：固定评测与基线

这是本示例的 `-evaluate` 模式，不是新增的 Papa CLI 子命令。静态 HTML、延迟渲染 DOM、JSON 两页各有 6 个固定合成样本：训练 2 正＋1 空负例，独立隐藏集同数、不同正文，共 18 个。每场景运行 3 次，保留全部 9 次。DOM 实际通过沙箱 Chromium 等待 800ms 延迟内容；JSON 检查第一页 next 与第二页结束。HTTP 服务只提供训练页，不提供隐藏页。

先在本机设置专用库 `PAPA_AGENT_DSN`（含 parseTime=True），沿用上文显式迁移；不要使用生产库。以下在 Papa 模块目录执行，`$env:PAPA_EVAL_BROWSER` 是本机已验证的 Chromium 可执行文件路径：

```powershell
# 第一次使用既有 rules/agent 表时加 -migrate；已有表不用重复迁移。
go run ./examples/siteagent -evaluate -browser $env:PAPA_EVAL_BROWSER -sample-dir ./evaluation-samples > ./evaluation-baseline.json
# 相同协议、模型、预算、价格再次跑全部 9 次，输出逐次结果及前后汇总。
go run ./examples/siteagent -evaluate -browser $env:PAPA_EVAL_BROWSER -sample-dir ./evaluation-samples -eval-compare ./evaluation-baseline.json > ./evaluation-current.json
```

默认使用 Replay，不读取模型 Key；`-render=false` 在固定评测中拒绝，不能跳过 DOM 场景后声称完成三类验收。默认的非 evaluate 示例行为不变。可选 `-browser` 留空走宿主自动发现；启动失败应保留失败报告并排查环境，不能仅凭离线候选通过把渲染失败算作成功。

每次分析上限 12 次模型调用、180 秒、3 个候选，原网络/字节/输入输出预算保持；整轮顺序运行至多 9 次、108 次调用、30 分钟，不追加重试批次。评测统计见 [SITE_AGENT §12](../../docs/SITE_AGENT.md#12-固定评测与基线d3)。报告包含每次 run ID、样本集 ID/摘要、候选 ID/版本/摘要、验收 ID、预算与实际用量；没有原始正文、隐藏答案、完整端点或 Key。时间包括样本安装、分析与评分，不是模型独立性能基准。

真实模型须显式加 `-real-model`，同时提供既有 `-model-base-url`、`-model`、`-api-key-env` 与以下参数；不设置时拒绝付费评测：

| 参数 | 含义 |
|---|---|
| `-price-version` | 宿主核实的价格表版本/日期标签 |
| `-currency` | 价格币种，如 CNY 或 USD，不能混币比较 |
| `-input-price-micros` | 每百万输入 token 的微货币单位单价（1 货币单位 = 1,000,000 微单位） |
| `-output-price-micros` | 每百万输出 token 的微货币单位单价 |
| `-max-run-micros` | 每次分析的保守估算费用上限；整轮至多预留其 9 倍 |

供应商未返回用量，或任一次调用用量不完整时，`tokens` 与 `estimated_micros` 为 null，不能解释为零成本。保守预留照常限制后续调用；框架估算不替代供应商账单/余额限额。真实模型不接收 Replay oracle，隐藏预期也不会发给模型。首次真实模型报告独立建立基线，不能拿 Replay 当质量对照。

`-eval-compare` 限制输入报告不超过 4MiB；协议、样本、模型、端点摘要、提示、预算、价格和 token 参数必须相同，否则在新模型调用前停止。中途失败仍输出已收集记录，全部重复完成才能汇总/比较；失败重复不删除、不挑最好值。基础设施失败使该次 `ready_for_review=false`，即使固定样本验收恰巧通过也一样。报告本身没有发布能力，正式发布仍由人工从原规则流程执行。

回归：

```powershell
go test -race ./pkg/agenteval ./pkg/crawlrule ./pkg/crawlagent ./examples/siteagent -count=1
# PAPA_AGENT_EXAMPLE_TEST_DSN 指向专用 agent_test 数据库，有实际 Chromium：
go test -race ./examples/siteagent -run '^TestFixedEvaluationMySQLAndChromium$' -count=1 -v
```

固定样本与宿主目标的摘要由 `evaluation_fixtures.go` 生成并被测试固定；正文、预期、目标或渲染延迟变动时须升级样本协议与基线，不能只修改旧结果。存档 Replay 基线见 `testdata/d3-replay-baseline.json`；它证明框架接线，不证明模型生成质量。基线引用的本机专用库和样本目录不随仓库分发；其它机器用固定语料重跑建立自己的审计记录。

## D4：无历史／已发布历史对照

新增可选 `-eval-history none|published`，只供 `-evaluate` 使用。**省略此参数仍走原 D3 路径，不安装任何历史发布**。显式指定后，两组均由示例宿主安装一份同样的合成已发布规则，且只用训练样本验收；none 组只作候选差异/影子比较，published 组另以 `HistoryPolicy` 批准提供最小规则结构给模型。这个合成发布只是专用测试库的前提，Agent 没有发布工具，生成的候选不得更改测试发布指针。

在相同代码、模型、预算、价格、语料和运行环境下分别执行两组：

```powershell
go run ./examples/siteagent -evaluate -eval-history none -browser $env:PAPA_EVAL_BROWSER -sample-dir ./evaluation-samples > ./history-control.json
go run ./examples/siteagent -evaluate -eval-history published -browser $env:PAPA_EVAL_BROWSER -sample-dir ./evaluation-samples -eval-compare ./history-control.json > ./history-treatment.json
```

每条命令各运行三场景×三次，单组仍最多 108 调用/30 分钟；不会自动启动第二组或失败补跑。真实模型沿用 D3 的显式参数与费用上限，两组须分别授权执行并预留两份预算。历史每阶段最多一个版本，每次最多 4 个，单视图 8KiB/合计 16KiB，计入原模型预算。规则 ID/版本、验证 Pattern、资源上限不进入模型视图，完整规则仍用于后续比较；样本、报告、凭据和隐藏答案不被当作历史导入。

报告新增历史模式、配方摘要、每次审计的引用/视图摘要/字节；汇总展示实际调用数、累计输入字节和单次历史视图字节。`CompareHistory` 只放行 none→published，普通基线比较仍完全一致。D4 系统提示摘要与旧 D3 存档不同，必须重跑当前版本 control，不能直接拿旧 D3 报告充当无历史组。历史视图在各次消息请求中重复计入输入，故累计输入增量不等于单份视图大小。

同一场景各次历史视图的摘要/字节必须一致；两个 published 报告也逐项核对视图，防止配方声明相同却实际上下文改变。历史准备失败的重复继续保留；只有没有模型调用时才能没有成功 history 记录。披露意图、准备结果和实际调用分别由 Plan.HistoryEnabled、history 与 model 审计说明，不混为同一状态。2026-11-08 review 未改变提示或投影视图，原 D4 存档仍可比较；真库测试另检查新运行快照中的披露标记。

存档：`testdata/d4-replay-control.json` 与 `testdata/d4-replay-history.json`。每组隐藏样本 27/27、必填字段 36/36、负样本 9/9，均每次 7 次调用、0 次修正、无工具错误；历史组单次视图 267/267/303 字节，累计输入分别增加 2737/2744/3087 字节。token/费用未知为 null。Replay 输出固定，**这不是历史提高真实模型质量的证据**，因此暂不扩展自动历史检索、候选/失败报告导入或独立记忆库。真实站点的历史选择和效果验收留在业务模块。

专项验证：

```powershell
go test -race ./pkg/crawlagent ./pkg/agenteval ./examples/siteagent -count=1
# 已安全配置 PAPA_AGENT_EXAMPLE_TEST_DSN 的专用 agent_test 库，并安装支持沙箱的 Chromium：
go test -race ./examples/siteagent -run '^TestHistoryEvaluationMySQLAndChromium$' -count=1 -v
```

本机 Windows 曾观察到 go run 复用工作区 GOCACHE 中 exe 后 Chromium 无法启动；同源码构建到系统默认 TEMP 的独立命令双组通过。可用 `go build -o "$env:TEMP\papa-siteagent.exe" ./examples/siteagent` 后运行该程序排查，保持 TEMP/TMP/GOTMPDIR 默认，不关闭沙箱或修改 ACL。失败报告应完整保留并标记环境差异，不能删除失败重复制造提升。审计数据库和正文目录不随存档分发；其它机器重跑建立自己的记录。
