# 受控的网站分析 Agent

> 面向：希望从网站证据生成采集规则、在固定样本上验收后再人工发布的业务。
> 配套：[规则仓库](./RULE_STORE.md)、[规则执行器](./RULE_FETCHER.md)、[离线规则](./CRAWL_RULES.md)、[运行质量反馈](./RULE_QUALITY.md)。

`pkg/crawlagent` 在宿主给定的访问范围和预算内取证、生成不可变候选、有限修正并运行独立验收。正式采集继续由 RuleFetcher 执行，Agent 不调用 Publish/Rollback、业务 Save 或 SubmitTask。

## 1. 先运行完整示例

源码和参数见框架仓库 [examples/siteagent](../examples/siteagent/README.md)。默认使用确定性模型回放，不需要 API Key；网站访问、Chromium、MySQL、样本验收和审计都实际运行。

```powershell
$env:GOWORK = 'off'
$env:PAPA_AGENT_DSN = '<已创建的专用 MySQL 示例库 DSN，包含 parseTime=True>'
go run ./examples/siteagent -migrate -sample-dir ./siteagent-samples
```

默认需要本机 Chromium/Chrome/Edge，或用 `-browser` 指定路径。示例覆盖静态列表/详情、脚本生成 DOM、实际 GET JSON 接口和第二页；`-render=false` 仅运行 HTTP 三阶段。示例预先定义独立训练/验收样本，不用候选输出反过来生成正确答案。

成功结果为 `status=awaiting_review`、`verified=true`，含候选、证据、用量与审计运行 ID，仓库 Current 仍未发布。回放证明程序接线和控制边界；不能当作真实模型生成质量或费用验收。

## 2. 启用与构造

```yaml
rules:
  enabled: true
  sample_dir: ./rule-samples
agent:
  enabled: true
```

`agent.enabled` 默认关闭，开启要求 `rules.enabled`。显式执行 `papa migrate` / `App.Migrate()` 后登记两张审计表 `crawler_agent_runs`、`crawler_agent_steps`；普通启动不建表。开关不自动创建 Agent、调用模型、运行分析或发布规则。

宿主构造顺序：

1. 用普通 MySQL 连接与长期样本目录构造 `rulestore.Repository`、`FileBlobs`。
2. `agentstore.New(db)` 构造持久审计；拒绝 DryRun 与外层事务连接。`NewMemoryStore()` 仅适合不要求跨进程恢复的测试/内嵌用法。
3. 构造模型 Provider，传入明确的模型名和密钥环境变量名。
4. `crawlagent.New(Config)` 传入 Model、ModelName、Audit、Repository、Blobs、Actor、SamplePolicy、Web 与 Budgets。
5. `Agent.Run(ctx, goal)` 创建一次新分析。Run 不是幂等重试接口，重复调用会创建新运行并可能再次计费。

宿主已有持久请求身份时可调用 `Agent.RunWithID(ctx, runID, goal)`。ID 由宿主指定；原子审计 Start 成功后才允许调用模型，已有 ID 或提交回执不明均停止，不能换个新 ID 重试规避检查。该入口不会恢复旧运行；质量复分析会先持久固定 RunID，再使用此入口。

Agent 的仓库接口只含样本、草稿、查询与 Verify，无法经工具调用生产发布；SourceFactory 是受信任的宿主扩展/测试入口，宿主实现须执行相同策略和记账回调，不能把无约束客户端作为替代品。

## 3. 模型配置

```go
provider, err := model.New(model.Config{
    BaseURL:     "https://your-service.example/v1",
    Model:       "your-selected-model",
    APIKeyEnv:   "PAPA_AGENT_MODEL_KEY",
    MaxInputBytes: 256 << 10, // 包含 HTTP JSON 封装，给 runner 输入留余量
})
```

这里的 model 包路径是 `github.com/ydtg1993/papa/v3/pkg/crawlagent/model`。密钥由环境变量读取，不把密钥放在 YAML、命令行参数或模型上下文。BaseURL 后只追加 `/chat/completions`，不自动插入 `/v1`；拒绝 userinfo/query/fragment，不跟随服务重定向、不自动重试、不继承环境代理。

默认发送 `max_completion_tokens`；旧兼容服务可显式设置 `OutputTokenField: model.MaxTokens`。接口参数是否支持须按所选供应商验证，不通过第二次真实请求自动试探。使用单次、非流式、一个 assistant 文本结果承载严格 JSON 动作；不要求兼容服务支持原生 function calling。拒绝截断、拒答、意外 tool call、多个结果、重复 JSON 键、非文本或超限响应。

例程启用真实模型必须加 `-real-model`，只设置环境变量不会产生模型调用：

```powershell
$env:PAPA_AGENT_MODEL_BASE_URL = 'https://your-service.example/v1'
$env:PAPA_AGENT_MODEL = 'your-selected-model'
# 在私有环境中设置 PAPA_AGENT_MODEL_KEY，不把密钥贴进日志或文档。
go run ./examples/siteagent -sample-dir ./siteagent-samples -real-model
```

## 4. 宿主目标与独立样本

`Goal` 指定 Site、Seeds 与 Stages。每个 `StageGoal` 包含 Stage、RuleID、Fields（名称、含义、类型、是否必须存在）、Transport（http/browser）、可选 ItemKey/Links，以及 TrainingSet/HoldoutSet ID。字段类型沿用 crawlrule/extract；“必须存在”与具体非空值正确性由规则校验和固定样本预期共同验证。

- 训练集的批准正文可以进入模型，失败报告可用于有限修正；模型没有修改样本预期的工具。
- 独立验收集的正文、预期、样本/集合 ID 不进入模型。finish 后统一验收，不把隐藏失败的期望值反馈给同一生成循环反复试答案。
- 训练和独立样本不能重叠；隐藏 URL 使用与实际请求一致的 host、默认端口、路径规范化，并按本次 SamplePolicy 过滤/规范化查询参数。URL 别名和动态正文不能借此绕过隔离。
- 独立样本缺失时可以产生候选与结构试跑，但最终为 `needs_fixtures`，`verified=false`。
- 每次分析的训练/独立 fixture 与附加 `EvidenceSamples` 合计最多 64 个引用，避免对整库进行隐式扫描。

旧样本进入模型前重新应用本次 SamplePolicy，并校验原始正文长度与摘要；历史“允许保留”不等于本次“允许披露给模型”。正文和 URL 的脱敏策略须由宿主提供，默认不存在自动全量放行。种子使用 `seed:0` 等别名，私有查询参数留在宿主请求侧；审计快照和模型只看到经过策略处理的 URL。

`Goal.EvidenceSamples` 可显式加入宿主选定的观测样本 ID，按本次 SamplePolicy、正文完整性检查和隐藏样本隔离后供分析读取。它们是证据，不会自动成为带正确预期的训练/验收 fixture。`Goal.Context` 用于宿主批准的调查事实，例如质量报告的稳定类别与计数；它会进入分析上下文，宿主应先去除秘密和原始业务值，也不能通过这段文字扩大工具或访问权限。

候选的规则 ID、版本由宿主控制，模型不能换阶段、放宽字段契约、扩大规则预算或开放外部下一页。版本格式关联分析 run ID 和该阶段候选序号，规则从第一次持久写入起不可变。

## 5. 工具与授权

| 工具 | 功能 | 前提 |
| --- | --- | --- |
| fetch_page | 获取 HTTP 页面/JSON，或渲染 DOM | URL 在宿主允许范围；render 另需 `Web.Renderer.Enabled` |
| read_evidence | 按 ID/片段读批准证据 | 只能读取本运行的公开证据；有片段和上下文预算 |
| inspect_selector | CSS / JSON Pointer 试验 | 只读离线证据，不联网 |
| record_api | 观察受控渲染中真实发生的 GET JSON | 同时要求 `Web.Renderer.Enabled` 与 `AllowRecordAPI=true`，后者默认 false |
| submit_candidate | 严格解析、编译并保存新候选 | 满足宿主字段/阶段/传输/链接契约与预算 |
| validate_candidate | 固定训练验收或结构试跑 | 不创建/修改正确预期；结构试跑不等于验收通过 |
| finish | 请求程序执行独立验收 | 所有目标阶段有合法候选；结果始终待人工发布 |

模型一次仅返回一个 JSON 动作；未知字段、未知工具、伪造证据、坏 JSON 和越权参数均拒绝并计入调用预算。网页中的提示不会增加工具权限。没有点击、输入、任意 JS、Shell、SQL、POST、登录或验证码工具。

Agent 的 CSS 子集比通用 crawlrule 更窄：支持标签/class/id/属性、`>` 和 `+`，每分支最多一个后代组合；不支持 `~` 组合器、伪类/伪元素、注释或转义（属性匹配 `[attr~=token]` 仍支持）。单选择器最多 512 字节、4 分支、每分支 8 个复合选择器；JSON Pointer 最多 1024 字节、32 段。

正文最多 8 MiB，解析树深度最多 64，节点/属性保守计数最多 20,000。HTML 还把解码后文本和属性值的字节数乘以实际节点深度累加，最多 2 MiB，用于约束多个字段反复提取祖先文本的工作量；这不是进程 RSS 保证。JSON 正文被 HTML 规则/检查工具处理时也执行 HTML 限制。候选的训练/独立验收、Shadow 和结构试跑均按实际评估格式预检。通用规则引擎原有 API 保持原行为，复杂站点可交给人工编写规则。

## 6. 网站访问边界

用 `webaccess.Policy.Origins` 声明顶层允许 origin；`ResourceOrigins` 只增加资源访问范围，不能变成新的顶层页面。PathPrefixes 按路径边界匹配，`/a` 不匹配 `/ab`。

HTTP 每次重定向重新检查目标；DNS 检查全部地址并使用已校验 IP 建立连接，保留 Host/TLS 校验。默认拒绝私网、环回及其它特殊地址；测试或已授权内网站点只能按单个明确 origin 开启 `AllowPrivateNetwork`，模型不能调整。普通环境代理不会被隐式使用。

固定 UA 为 `PapaSiteAgent/1.0`；执行本次运行内的 robots 判定、按 origin 调度、CrawlDelay 与 Retry-After。robots 异常/认证拒绝/不可用不会静默当允许。该调度不是跨进程站点配额或分布式限流。

浏览器使用独立临时 profile，页面 HTTP(S) 请求转交同一受控获取器；原生出口经拒绝代理兜底，并限制 DNS/QUIC/WebRTC 等旁路。实际测试覆盖导航、重定向、iframe、资源、worker、popup、WebSocket 和 POST。未支持的流量明确阻断；被拒绝或失败的已拦截资源会使渲染明确失败，不返回假装完整的成功页面。

DOM 快照 `Rendered=true, StatusCode=0`；捕获的 JSON 响应保留真实 HTTP 状态。不给模型暴露生产浏览器池、Cookie、LocalStorage 或任意 Rod 对象。以上为框架及已测 Chromium 路径的控制，不是对浏览器漏洞或操作系统网络隔离的认证。

## 7. 预算与错误

| 限制 | 默认 | 可配置硬上限 |
| --- | --- | --- |
| 顶层页面 / 实际网站请求 | 8 / 128 | 32 / 512 |
| 单响应 / 累计正文 | 2 MiB / 16 MiB | 8 MiB / 64 MiB |
| 总时间 | 180 秒 | 600 秒 |
| 单运行 / 进程共享浏览器并发 | 1 / 1 | 1 / 4 |
| 模型调用 / 每阶段候选 | 12 / 3 | 64 / 8 |
| 阶段数 | 1 | 8 |
| 模型输出 token | 4096 | 16384 |
| 单次 / 累计模型输入序列化字节 | 128 KiB / 1 MiB | 256 KiB / 4 MiB |
| 动作 JSON / 证据读取片段 | 64 KiB / 16 KiB | 64 KiB / 32 KiB |

网站请求计数包含 robots、重定向和子资源；正文预算保守计入压缩传输、解压结果、DOM 副本及失败读取，达到上限后不会继续外发。模型调用前预留次数、输入及费用额度，失败请求也占预算；供应商报告输出超过本次上限时停止后续动作。

Prices 使用宿主给定的每百万 token 单价和整数微货币单位，调用前按输入字节保守上界和输出上限预留，收到 usage 后校正。缺单价或 usage 时费用标为未知/保留预留值，调用数和字节上限仍生效。这里的估算不是供应商最终账单保证。

返回状态：`awaiting_review`、`needs_fixtures`、`exhausted`、`failed`、`canceled`；审计提交结果不明确时返回 `interrupted` 提示及可查询 RunID，数据库里的真实状态须再读取。模型和网络错误不回显密钥、完整提示或供应商响应正文。

## 8. 审计、取消和重启

每次模型/工具调用前先持久化 pending 步骤；每次实际网站请求前先记录用量。写入失败就不执行外部动作。已完成工具步骤保存证据 ID/摘要、候选 ID/版本和报告 ID，运行未到 Finish 就停止也能追溯；不在步骤表重复保存正文或完整模型回复。

运行快照记录 Actor、目标、实际生效的模型/网站预算及渲染/录制权限；审计修改使用 revision CAS 与数据库事务。提交回执丢失不能被当作允许继续调用模型的凭据。取消用独立短期限收尾审计并释放资源，不让已取消模型回复再触发工具。

加载已批准观察样本、训练集或隐藏验收集期间收到 context 取消，也记录为 canceled，不归类成普通证据不可用。质量复分析在确认旧执行者结束后中断审计，仍从固定快照保留导入样本引用；中断回执丢失时继续只读对账，不重放模型调用。

进程重启后先 `Get` / `Steps` 查看。只有宿主确认旧执行者已结束，才可按 ID 和当前 revision 显式 `Interrupt`，将未确认 pending 标为 unknown。没有自动接管、租约或重放。随后可以人工审阅原候选，或用所选已有样本创建新的分析；不要直接把 Run 当重试接口反复调用。

例程提供 `-inspect-run` 只读检查与 `-interrupt-run ... -worker-stopped` 演示。关闭开关或再次迁移不会删除记录；备份仍须覆盖审计/规则 MySQL 元数据与长期样本目录。

## 9. 交给人工发布

通过独立验收的 Result 只表示候选待发布。将候选阶段引用、训练/独立报告、证据、CompareRules/Shadow 结果交给操作者，再显式调用第三批 Publish；传实际 Current generation 和新的 operationID。

Agent 不会修改站点 Current，也不改变已有任务固定的 release/版本。已有线上规则可通过 StageGoal.Existing 提供用于只读对比，Shadow 使用同一份已批准证据，无额外请求或业务写入。

OA 已提供运行与候选审阅中心（见下文 §11）；仍没有通用 `papa agent` CLI。也没有真实模型质量基准、自动漂移修复、自动发布、账号接管或跨进程调度。

## 10. 从运行质量报告显式复分析

宿主可先用 `rulequality.BuildReport` 固定已记录观测，再对 `suspected_rule_drift` 报告显式调用 `Reanalyzer.Prepare`、`Execute`。创建请求不会调用模型；执行前校验当前发布版本、所选保留证据、目标和预算，成功持久化请求状态后才运行 Agent。缺失或过期正文会返回需要证据，不为补齐现场偷偷重发生产请求。训练/独立验收集仍由宿主提供，不能从异常线上输出反推正确预期。

`quality.enabled` 独立控制三张质量表的显式迁移与启动检查；仅开这个开关不会调用 Agent。复分析沿用本章的 rules/agent 接线、当前脱敏策略和人工发布流程，重复 operationID 关联原请求与 RunID；未知提交与重启先查询对账，不自动重放模型请求。完整使用顺序与能力缺口见 [RULE_QUALITY.md](./RULE_QUALITY.md)。

## 11. 接入 OA 运行与候选审阅中心

仅启用 `agent.enabled` 会为标准 App 接通历史审计查询，不赋予网页执行任意分析的能力。开启 rules、agent、quality 并完成显式迁移后，在 `Run` 前注册：

```go
// 以下 app、agentConfig、goalForReport 均由宿主初始化。
// agentConfig.Repository 与 App 必须使用同一套权威规则/质量库。
err := app.UseAgentCenter(agentadmin.Options{
    MaxConcurrent: 1,
    Profiles: []agentadmin.Profile{{
        ID: "catalog", Revision: "1", Label: "目录规则分析",
        Agent: agentConfig,
        ArchiveDir: archiveDir,
        Goal: goalForReport, // func(context.Context, rulequality.Report) (crawlagent.Goal, error)
    }},
})
if err != nil { return err }
```

导入 `github.com/ydtg1993/papa/v3/pkg/agentadmin`。`agentConfig` 沿用 §2 的模型/Repository/FileBlobs/当前脱敏策略与 webaccess/budgets 配置；审计 Store 和操作人由 App/具名 HTTP 身份绑定，不接受浏览器覆盖。Profile ID 和 Revision 不含冒号；改变目标生成、披露策略、模型或权限时升级 Revision。Goal 回调只能根据固化报告及可信业务配置构造目标，须可重入、确定性、有界并遵守 context；不得在回调中调用模型或改变发布。

`Agent.Plan(goal)` 返回实际生效默认预算与经过披露策略处理的种子，返回值与内部配置分离。中心预览额外隐藏 Context 和 EvidenceSamples；摘要仍绑定完整宿主输入、配置版本、操作人及证据选择。模型 API Key 不进入 Plan。`ForActor` 克隆固定能力并绑定具名操作者。

`Options.WorkerStopped` 默认不配置；只有宿主能证明旧执行者在所有实例均停止时才注册。它不是租约或自动恢复。守卫不在中心全局锁内运行，可以查询中心状态；仍须遵守 context，且只有真实停止证明才能返回 true。App 的关闭等待同时覆盖后台分析与在途恢复检查，超时保留依赖；App.Run 返回后可再次调用 `App.CloseAgentCenter(ctx)` 确认。中心总运行时限从受理开始，包含导入证据的准备阶段，不会在 Agent.Run 开始时重新获得完整时长。完整操作、错误与恢复边界见 [MONITOR §15](MONITOR.md#15-agent-运行与候选审阅中心)。

`examples/rulequality` 使用真实 MySQL、实际失败现场与确定性回放，现经中心完成预览、重复准备、后台执行、固定候选审阅与完整发布清单生成，并验证 Current 不变。真实供应商质量/账单仍需宿主另行显式验收。
