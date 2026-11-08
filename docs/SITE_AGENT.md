# 受控的网站分析 Agent

> 面向：希望从网站证据生成采集规则、在固定样本上验收后再人工发布的业务。
> 配套：[规则仓库](./RULE_STORE.md)、[规则执行器](./RULE_FETCHER.md)、[离线规则](./CRAWL_RULES.md)、[运行质量反馈](./RULE_QUALITY.md)。

`pkg/crawlagent` 在宿主给定的访问范围和预算内取证、生成不可变候选、有限修正并运行独立验收。正式采集继续由 RuleFetcher 执行，Agent 不调用 Publish/Rollback、业务 Save 或 SubmitTask。

### 样本通过与实时可用是两项检查

`Result.Verified` 只表示独立固定样本验收通过，不代表本次访问网站成功，更不代表完整采集或发布成功。离线样本可以正常通过，而某次实时工具调用失败；不要把 verified 映射成「接站完成」。

每个 `Evidence.origin` 由宿主写入：`host_sample` 表示本次加载的训练或批准观测样本（即使样本以前由浏览器产生），`live_page` 表示本次成功取证的页面，`live_api` 表示本次捕获的 API 响应。正式结果及步骤审计保留来源；旧审计没有 origin 时必须按未知处理，不推断为实时成功。不增加表或迁移。

需要实时接站验收的业务应列出必需的 URL、页面/API 类型与渲染方式，检查匹配的本次实时证据、成功状态、非空正文与摘要，并确认调用没有返回错误。审计结果不保存 URL，重启后可按 evidence ID 从批准的样本库恢复元数据，不能据空 URL 猜测验收通过。通过这些检查仅表示可以进入完整采集测试；字段、分页、落库和人工发布仍需各自验证。

Agent 对公开错误使用稳定分类：`renderer_unavailable`、`web_transport`、`render_wait_timeout`、`access_denied`、`robots_denied` 等，不将浏览器或网络异常中的原始地址、正文或秘密写入审计。基础设施问题应按错误类别排查，不以独立样本通过掩盖。

## 1. 先运行完整示例

源码和参数见框架仓库 [examples/siteagent](../examples/siteagent/README.md)。默认使用确定性模型回放，不需要 API Key；网站访问、Chromium、MySQL、样本验收和审计都实际运行。这个例程始终使用自建本地测试站。真实站点的访问策略、固定预期、模型/数据库接线及验收记录必须放在独立业务项目，通过公开 API 使用框架，不能将站点业务代码加入 Papa。

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

这里的 model 包路径是 `github.com/ydtg1993/papa/v4/pkg/crawlagent/model`。密钥由环境变量读取，不把密钥放在 YAML、命令行参数或模型上下文。BaseURL 后只追加 `/chat/completions`，不自动插入 `/v1`；拒绝 userinfo/query/fragment，不跟随服务重定向、不自动重试、不继承环境代理。

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
- 宿主可以在训练/独立集混入显式 `empty`、`reject` 负样本；每组仍必须有正样本。空/全负/非法样本集在模型调用前拒绝，训练报告提供类型、实际拒绝分类与正负总数，隐藏集的正文和预期仍不反馈给生成循环。负样本不会把解析、超时、资源或持久化失败变成通过，具体契约见 [CRAWL_RULES](CRAWL_RULES.md#样本验收激活与回滚)。
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
| fetch_page | 获取 HTTP 页面/JSON，或渲染 DOM；render 可选有界 CSS 等待 | URL 在宿主允许范围；render 另需 `Web.Renderer.Enabled` |
| read_evidence | 按 ID/片段读批准证据 | 只能读取本运行的公开证据；有片段和上下文预算 |
| inspect_selector | CSS / JSON Pointer 试验 | 只读离线证据，不联网 |
| record_api | 观察受控渲染中真实发生的 GET JSON | 同时要求 `Web.Renderer.Enabled` 与 `AllowRecordAPI=true`，后者默认 false |
| submit_candidate | 严格解析、编译并保存新候选 | 满足宿主字段/阶段/传输/链接契约与预算 |
| validate_candidate | 固定训练验收或结构试跑 | 不创建/修改正确预期；结构试跑不等于验收通过 |
| finish | 请求程序执行独立验收 | 所有目标阶段有合法候选；结果始终待人工发布 |

模型一次仅返回一个 JSON 动作；未知字段、未知工具、伪造证据、坏 JSON 和越权参数均拒绝并计入调用预算。网页中的提示不会增加工具权限。没有点击、输入、任意 JS、Shell、SQL、POST、登录或验证码工具。

Agent 的 CSS 子集比通用 crawlrule 更窄：支持标签/class/id/属性、`>` 和 `+`，每分支最多一个后代组合；不支持 `~` 组合器、伪类/伪元素、注释或转义（属性匹配 `[attr~=token]` 仍支持）。单选择器最多 512 字节、4 分支、每分支 8 个复合选择器；JSON Pointer 最多 1024 字节、32 段。

### 同次渲染中的有界等待

动态内容可以使用以下模型动作；只允许显式 `fetch_page` + `mode=render`，HTTP 和 `record_api` 不接受 `wait`：

```json
{"tool":"fetch_page","url":"seed:0","mode":"render","wait":{"selector":"section.card > h2","state":"visible","timeout_ms":3000}}
```

`selector` 必填、非空，沿用上述保守 CSS 子集；`state` 必须为 `exists` 或 `visible`，`timeout_ms` 为 1–30000 的整数。未知字段、null、非法选择器或超范围时限在外部访问前拒绝，仍消耗本次模型调用预算。`exists` 检查主文档的首个匹配元素；`visible` 进一步要求布局框宽高非零、display 非 none、visibility 非 hidden/collapse，不判断透明度、视口或遮挡，也不进入 iframe / shadow DOM。多个匹配时不会跳过隐藏的首个元素寻找可见元素。

等待从本次页面 load 完成后开始，每 100ms 检查一次；继承会话、调用者和 Agent 的剩余总时限，导航、等待、取证不会各自重置总预算。等待期间的网络请求仍经过原策略和记账；条件成立后取最终 DOM，不代表页面所有异步工作完成。条件由隔离世界中的固定代码检查，网页覆盖查询函数不能伪造结果；模型没有脚本、点击或跨调用会话权限。

宿主直接调用通用 API 时：

```go
page, records, err := session.Render(ctx, targetURL, webaccess.RenderOptions{
    Wait: &webaccess.RenderWait{
        Selector: "section.card > h2",
        State:    "visible",
        Timeout:  3 * time.Second,
    },
})
```

`Wait` 替代固定观察期，与非零 `ObserveDuration` 同时指定会返回 `webaccess.ErrWaitInvalid`。不传 Wait 维持旧默认 500ms 观察或宿主明确设置的 ObserveDuration。新字段不影响具名结构体字面量；使用无字段名的 `RenderOptions{...}` 初始化需改为具名写法。

等待超时返回 `webaccess.ErrWaitTimeout`，不返回成功 DOM 或捕获响应，不保存新的成功页面证据；已批准训练证据仍保留。外层取消/期限到期仍为 `ErrCanceled`，网络策略/预算失败保持原错误分类。Agent 工具审计对应 `render_wait_timeout` / `invalid_render_wait`；超时可向模型反馈并有限修正，但仍受原运行预算限制，不自动重试。页面关闭或渲染上下文失效同样明确失败，不返回假成功。

审计输入只追加选择器 SHA-256、state、timeout_ms，输出追加 `render_wait.outcome` 与 `tool_elapsed_ms`。耗时覆盖整个工具调用（包括启动、导航、等待、取证与保存），不是纯等待时长；审计不存原选择器或第二份正文。成功仅继续候选生成/验收，正式发布仍由人工执行。自定义 SourceFactory 必须自行履行这些等待、预算及错误约定。

正文最多 8 MiB，解析树深度最多 64，节点/属性保守计数最多 20,000。HTML 还把解码后文本和属性值的字节数乘以实际节点深度累加，最多 2 MiB，用于约束多个字段反复提取祖先文本的工作量；这不是进程 RSS 保证。JSON 正文被 HTML 规则/检查工具处理时也执行 HTML 限制。候选的训练/独立验收、Shadow 和结构试跑均按实际评估格式预检。通用规则引擎原有 API 保持原行为，复杂站点可交给人工编写规则。

## 6. 网站访问边界

用 `webaccess.Policy.Origins` 声明顶层允许 origin；`ResourceOrigins` 只增加资源访问范围，不能变成新的顶层页面。PathPrefixes 按路径边界匹配，`/a` 不匹配 `/ab`。

HTTP 每次重定向重新检查目标；DNS 检查全部地址并使用已校验 IP 建立连接，保留 Host/TLS 校验。默认拒绝私网、环回及其它特殊地址；测试或已授权内网站点只能按单个明确 origin 开启 `AllowPrivateNetwork`，模型不能调整。普通环境代理不会被隐式使用。

固定 UA 为 `PapaSiteAgent/1.0`；执行本次运行内的 robots 判定、按 origin 调度、CrawlDelay 与 Retry-After。robots 异常/认证拒绝/不可用不会静默当允许。该调度不是跨进程站点配额或分布式限流。

浏览器使用独立临时 profile，页面 HTTP(S) 请求转交同一受控获取器；原生出口经拒绝代理兜底，并限制 DNS/QUIC/WebRTC 等旁路。实际测试覆盖导航、重定向、iframe、资源、worker、popup、WebSocket 和 POST。未支持的流量明确阻断；被拒绝或真实失败的已拦截资源会使渲染明确失败。达到观察期或等待条件后，取快照主动取消仍在途的资源请求属于正常收尾，不将它误记为网络失败；已发生用量仍计入预算，不返回未完成的 API 正文。DOM 快照不承诺页面全部异步工作已完成。

DOM 快照 `Rendered=true, StatusCode=0`；捕获的 JSON 响应保留真实 HTTP 状态。不给模型暴露生产浏览器池、Cookie、LocalStorage 或任意 Rod 对象。以上为框架及已测 Chromium 路径的控制，不是对浏览器漏洞或操作系统网络隔离的认证。

空正文的成功页面、脚本和重定向是合法响应；交给 Chromium 时按 CDP 空字节串编码，不把 `null` 当正文。这个编码适配不修改原始证据或字节计数。请求数、单响应/DOM 字节、总字节、页面数和重定向数超限时返回预算错误，不将失败现场当作成功页面。调用取消、Session.Close 和会话期限到期会结束渲染并释放 profile 与并发名额；会话期限到期沿用 canceled/deadline 分类。

### 受控 Chromium 验收与启动排查

在框架仓库执行 `go test ./pkg/webaccess -run '^TestRealChromium' -count=1 -v`；并发验证加 `-race`（Windows 需 CGO 与 C 编译器）。没有浏览器时的 skip 不算验收通过。完整候选/报告/审计链路另按 `examples/siteagent/README.md` 配置专用 MySQL 测试库运行，默认使用确定性模型回放，不产生真实模型调用费用。

受控渲染要求保留 Chromium 沙箱。遇到 `webaccess: controlled renderer unavailable`，先核对实际可执行文件、临时目录和运行账户，再定位启动或 CDP 错误；不要直接加 `--no-sandbox` 或关闭网络防护。Windows 本次验收中，将 `TEMP/TMP/GOTMPDIR` 指向工作区临时目录时 Chrome 启动失败，仅覆盖 `GOTMPDIR` 也能复现；保留系统默认临时环境后同一套受控参数通过。尚未确定具体宿主限制，不能单独归因为 profile 权限；这不代表所有自定义目录都不可用，也没有据此修改目录 ACL 或全局环境。

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

模型输出仍要求正文中的单个严格 JSON 动作；DSML/XML 原生函数调用标记、Markdown 和混合解释正文不会被自动剥离或执行。对明确支持 JSON 输出的兼容供应商，宿主可在 `model.Config` 设置 `JSONOutput: true`，发送 `response_format={"type":"json_object"}`。默认关闭，不影响已有端点；供应商拒绝参数时明确失败，不自动回退再发一次付费请求。输出选项计入请求大小预算，也不替代严格动作校验。

`submit_candidate` 成功只表示草稿保存，观察结果明确包含 `validation_status=not_run` 和下一步 `validate_candidate`。`training_passed/holdout_passed=false` 不能在报告尚未生成时解释为失败。训练通过后返回 `training_passed_holdout_not_run`；全部目标阶段候选和训练检查就绪才建议 `finish`。独立验收仍只在结束步骤执行，模型不获得隐藏预期。动作协议和状态语义的通用修复及回归见 [框架修改说明](issues/2026-11-07-agent-protocol-release-summary.md)。

通过独立验收的 Result 只表示候选待发布。将候选阶段引用、训练/独立报告、证据、CompareRules/Shadow 结果交给操作者，再显式调用第三批 Publish；传实际 Current generation 和新的 operationID。

Agent 不会修改站点 Current，也不改变已有任务固定的 release/版本。已有线上规则可通过 StageGoal.Existing 提供用于只读对比，Shadow 使用同一份已批准证据，无额外请求或业务写入。

OA 已提供运行与候选审阅中心（见下文 §11）；仍没有通用 `papa agent` CLI。真实站点接入和生成质量验收由独立业务层承担，单站结果不构成跨站点质量基准；也没有自动漂移修复、自动发布、账号接管或跨进程调度。

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

导入 `github.com/ydtg1993/papa/v4/pkg/agentadmin`。`agentConfig` 沿用 §2 的模型/Repository/FileBlobs/当前脱敏策略与 webaccess/budgets 配置；审计 Store 和操作人由 App/具名 HTTP 身份绑定，不接受浏览器覆盖。Profile ID 和 Revision 不含冒号；改变目标生成、披露策略、模型或权限时升级 Revision。Goal 回调只能根据固化报告及可信业务配置构造目标，须可重入、确定性、有界并遵守 context；不得在回调中调用模型或改变发布。

`Agent.Plan(goal)` 返回实际生效默认预算与经过披露策略处理的种子，返回值与内部配置分离。中心预览额外隐藏 Context 和 EvidenceSamples；摘要仍绑定完整宿主输入、配置版本、操作人及证据选择。模型 API Key 不进入 Plan。`ForActor` 克隆固定能力并绑定具名操作者。

`Options.WorkerStopped` 默认不配置；只有宿主能证明旧执行者在所有实例均停止时才注册。它不是租约或自动恢复。守卫不在中心全局锁内运行，可以查询中心状态；仍须遵守 context，且只有真实停止证明才能返回 true。App 的关闭等待同时覆盖后台分析与在途恢复检查，超时保留依赖；App.Run 返回后可再次调用 `App.CloseAgentCenter(ctx)` 确认。中心总运行时限从受理开始，包含导入证据的准备阶段，不会在 Agent.Run 开始时重新获得完整时长。完整操作、错误与恢复边界见 [MONITOR §15](MONITOR.md#15-agent-运行与候选审阅中心)。

`examples/rulequality` 使用真实 MySQL、实际失败现场与确定性回放，现经中心完成预览、重复准备、后台执行、固定候选审阅与完整发布清单生成，并验证 Current 不变。真实供应商质量/账单仍需宿主另行显式验收。


### 供应商显式模式与宿主凭据管理

`model.Config.ThinkingMode` 为空时不发送 thinking 参数，保持既有请求行为；`enabled` / `disabled` 显式发送 `thinking.type`，其它值拒绝。仅为支持该参数的兼容接口启用。参数计入输入字节预算，供应商不支持时返回错误，不自动降级、重试或修改模型响应。

`model.New(config)` 继续只读取一次 APIKeyEnv。宿主已有加密凭据仓库时可调用 `model.NewWithKey(config, key)`，不读取或改写环境变量，也不发起网络请求。两者均冻结 endpoint、模型、思考模式和凭据；不要把 key 放进 Config、日志或前端。新配置应构造新 provider，在途操作保留原 provider。

空白 assistant content 使用 `model.ErrEmptyResponse`，同时满足 `errors.Is(err, model.ErrInvalidResponse)`。Runner 仅记录已知安全分类（例如 model_empty_response、model_invalid_response、model_rate_limit、model_unauthorized、model_transport、model_truncated）；不把供应商错误正文、凭据或推理文本写入审计。未知错误仍记 operation_failed。分类有助于人工诊断，不构成重放付费请求的授权。

## 12. 固定评测与基线（D3）

在框架的 `examples/siteagent` 显式使用 `-evaluate`：静态 HTML、浏览器渲染、JSON 分页各固定 6 个合成样本（训练 2 正＋1 空负例，隐藏集同数且正文不同），每场景独立运行 3 次。默认 Replay；真实站点样本、业务预期和模型接线放在独立业务模块。评测不由 OA 轮询自动触发。

每次分析仍使用 12 次模型调用、180 秒、每阶段最多 3 个候选和既有网络预算。整轮最多 9 次分析、108 次调用、30 分钟，顺序执行；不会因失败自动补一轮。`-real-model` 显式开启付费路径时，还必须给出价格版本、币种、输入/输出百万 token 单价（微货币单位）及单次估算费用上限；整轮保守预留费用上限是单次的 9 倍。供应商账单可能包含其它项目，宿主仍需配合供应商限额。

| 指标 | 固定口径 |
|---|---|
| 隐藏样本通过率 | 分母为该场景 3 个隐藏样本，缺候选也不缩小；每个样本须精确满足完整预期 |
| 必填字段准确率 | 分母为宿主正样本的固定字段断言数（本合成集每次 4 项），缺失/错误计零；部分正确字段的诊断分数不替代整条记录校验 |
| 负样本正确率 | 分母为固定负例数（每次 1 个），类型/预期拒绝类别必须吻合 |
| 工具错误率 | 非成功工具步骤 / 全部已预留工具步骤，pending/unknown 计非成功；没有工具步骤时未定义，不视为零错误 |
| 修正次数 | 本次单阶段提交候选尝试次数减一，下限 0；包括失败提交，不含未进入工具的格式错误回复 |
| 耗时 | 样本安装、Agent 执行、审计读取与离线评分的总毫秒数，不是纯模型延迟 |
| 模型用量 | 所有模型调用均有有效 token 用量才汇总，否则为 null；输入字节、网络请求/页面/字节另记 |
| 估算费用 | 价格版本与完整已知用量都存在才记录，否则为 null，不能用未知 token 的零值推算免费 |

`pkg/agenteval.Score` 只作离线诊断，无候选保持零分和固定分母。字段参数由宿主固定为必填字段断言。`MeasureAudit` 计算尝试、工具错误、修正与用量；操作/证据/资源/执行失败单列。即使之后修正成功，发生基础设施错误的该次评测也不能标为 ready_for_review。格式/候选错误与基础设施失败分开记录；任何评分都不自动发布。

报告包含协议、实际系统提示摘要（`crawlagent.PromptDigest()`）、工具协议版本、固定样本摘要、模型/无凭据端点摘要、实际生效预算、候选与验收引用。模型只能读训练证据，隐藏正文与预期不进入反馈；报告不记录 Key、原始模型回复或供应商错误正文。各 run ID 可由原 `-inspect-run` 重开查看，不重放请求。

`-eval-compare <旧报告.json>` 只接受相同身份的完整报告；模型、端点、提示、协议、样本、预算、价格或 token 参数变化均拒绝，在新模型调用前停止。不以本地随机端口、run ID 或数据库生成 ID 判断可比性。全部三次结果都保留，汇总 min/max/mean；中途取消产生的部分报告不能成为完整基线。真实模型与 Replay 不能混合比较。报告是宿主诊断材料，不是防篡改的发布凭据；正式发布仍走原候选独立验收和人工发布路径。

运行命令、价格单位和记录基线见框架仓库 `examples/siteagent/README.md`。本功能不加表/列，不需额外迁移；首次使用既有规则/Agent 表仍须显式迁移。显式历史对照见下节。

## 13. 受限历史上下文（D4）

复用 `StageGoal.Existing` 选择规则版本。`Config.HistoryPolicy` 默认 nil：保持旧行为，仅在候选提交后比较旧规则，不把旧规则发给模型。宿主显式提供 `func(context.Context, HistoryRequest) error` 后，Runner 在首次模型调用前读取、检查并冻结所选版本。首批只允许 `EverPublished=true` 且 `State=published`，不导入未发布候选、失败规则或报告正文，不自动搜索历史。

`HistoryRequest` 包含本轮 Site/Stage、脱离内部快照的 Version 与投影后的 ViewJSON。回调必须核实**所选版本与该站点/阶段的授权绑定**，并批准视图中选择器、JSON Pointer 等字面量披露给模型；返回非 nil 即拒绝整次分析，不能静默降级。版本本身没有站点所有权，EverPublished 也不是访问授权；可核对宿主所选 Current/历史 release 的 Bindings 和固定摘要，不能从规则 ID 猜测所属站点。回调是可信宿主代码，应有界、遵守 context，不调用模型或改变发布。通过后只使用冻结版本，之后发布切换、仓库读取结果变化和回调修改入参都不改变本次历史视图与影子比较。

`Plan.HistoryEnabled` 明确标记宿主是否启用历史披露，参与中心的预览摘要并保存在运行快照；默认 false，旧快照缺少字段仍可读取。后台预览显示该状态，启用后仍须逐项授权成功才能调用模型；它不表示批准或调用已经发生。改变回调的授权语义仍须升级 Profile.Revision，布尔标记不替代宿主配置版本。

| 约束 | 行为 |
|---|---|
| 数量 | 每阶段一个 Existing，每次最多 4 个历史规则 |
| 视图大小 | 单个最多 8KiB，总计最多 16KiB；超限拒绝，不截断 |
| 原模型预算 | 视图与包装 JSON 进入每次输入/累计输入、估算 token/费用和运行时限；不新增额度 |
| 投影内容 | 仅声明式提取结构与已发布标记；去掉规则 ID/版本、验证正则 Pattern 和资源上限；不带 Actor、样本、报告、正文、Cookie |
| 隐藏集 | 模型仍不可读取隐藏样本或预期；投影 JSON 字符串中的直接 URL 及常见 HTTP(S) URL 字面量按取证的 URL 身份与披露策略检查，覆盖主机大小写、默认端口和查询顺序等价写法。该检查不解释 CSS 转义，也不是通用秘密或间接答案检测，宿主仍须审阅字面量 |
| 历史可信度 | 仅作不可信建议；不得覆盖当前目标、网络权限、预算、隐藏验收或人工发布 |
| 审计 | 模型前完成 `history` 步骤，保存 stage/ref、规则与视图 SHA-256、状态和视图字节数；不重复保存规则正文。审计失败不得调用模型 |

成功的 history 步骤说明已准备并批准上下文，是否实际尝试披露还须看后续 model 步骤；不能单凭 history 成功宣称模型已经收到。重启只读原 run/steps 和不可变规则引用，不自动重放请求。训练样本继续用 `TrainingSet/EvidenceSamples`，沿用原披露审批，不另建历史样本通道。候选仍重新训练/独立验收，发布由人工执行。

评测示例提供 `-eval-history none|published`，必须显式指定。两组均安装相同、仅由训练样本验收的宿主合成历史规则，只有 published 组披露；生成候选不能替换该测试发布指针。每组仍是 D3 三场景各三次，单组预算不变，用户分别执行两组才增加总调用预算。`agenteval.CompareHistory` 仅允许 none→published 的处理差异，要求相同历史配方、提示、模型、样本、预算、价格和完整重复次数；普通 Compare 不放宽。新增汇总模型调用数、累计输入字节和单次历史视图字节，未知 token/费用仍为 null。

历史组各重复的视图摘要及字节数须一致；比较两个 published 报告也检查对应视图，不能只凭相同配方声明接受实际变动的历史输入。若历史准备前就失败，允许完整保留该次失败且没有 history 记录，但必须零模型调用；一旦有模型调用就必须能追溯到成功的历史准备。比较仍是诊断，不能证明手工修改过的外部报告真实可信。

存档 Replay 两组均满分且调用/修正次数相同，仅输入字节增加；这证明流程与隔离，**尚未证明真实模型质量提升**。提示增加历史约束后摘要变化，原 D3 存档保留作历史记录，需用当前版本重新生成无/有历史对照。真实站点历史选择、授权 UI 和模型对照留在独立业务模块；本批不接入自动检索或长期记忆。

兼容性：未配置 HistoryPolicy 的业务无需启用历史披露。`Config/Plan` 新字段使无字段名结构体字面量需改为具名字段；`agenteval.Identity/AuditMetrics/Summary` 同理。无 DDL 或 YAML 配置项，无需额外 migrate。Agent 中心宿主改变 HistoryPolicy 或选择规则的逻辑时必须升级 Profile.Revision，保留在途快照。
