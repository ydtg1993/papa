# RuleFetcher 可运行示例

在 papa 模块目录运行，无需 MySQL、浏览器或外部网站：

```powershell
$env:GOWORK = 'off'
go run ./examples/rulecrawl
go run ./examples/rulecrawl -mode=json
go test ./examples/rulecrawl
```

示例启动自己的回环 HTTP 服务，先用保存的样本验证并激活 `demo-catalog` 和 `demo-detail` 规则，再执行列表 → 详情任务。输出为稳定排序的 JSON：两页列表、两个去重后的条目、两页详情、两条业务记录，以及每页使用的规则版本、每条详情的响应体 SHA-256。临时服务端口不进入输出，`source_path` 只展示来源路径。

- 默认 `html`：访问 `/list1` 和 `/list2`，CSS 提取字段并跟随 `rel=next`。第二页重复 A，最终只派生 A、B 两条详情任务。
- `-mode=json`：两次请求相同的 `/api/catalog`，使用只读 POST 与 `RuleRequest.State` 携带游标。JSON 响应的 `items` 是业务条目，根级 `next_cursor` 是分页游标；规则声明 `Next: &crawlrule.Next{Mode: "cursor", Pointer: "/next_cursor"}`，fixture 用 `Expect.NextCursor` 验证。框架自动把非空游标传给同一地址的下一任务，`BuildRequest` 根据 `State` 生成请求体，无需额外 `Next` 回调。详情仍使用 HTML 规则。
- 两种模式都主动重放一条成功的详情任务，模拟保存后发生重试。`Save` 按业务 ID 覆盖写入，输出始终只有两条业务记录。

这里调用 `RunHTTP`，使用最多 16 条任务的单线程内存队列。它演示规则执行、请求身份、分页、跨页去重、详情派生和幂等保存；队列和数据在进程退出后消失。`RunHTTP` 不启动框架 worker，也不覆盖数据库恢复、账号管理或浏览器路径。

## 接到实际应用

生产环境用 `papa.NewRuleFetcher` 创建普通 fetcher，放进 `papa.StageSpec`；worker 调用 `FetchHandler`，后续任务走 `Engine.SubmitTask` 落入 `crawler_tasks`，沿用恢复、错误重投和后台治理。应用需要配置 MySQL，预先创建数据库并显式执行迁移。

下面的函数可放进已有应用的启动代码。传入已完成 `Register` + `Activate` 的 registry、固定版本 bindings 和业务保存函数；保存函数应按业务唯一键执行数据库 upsert，并处理并发与事务。

```go
func registerRules(
    app *papa.App,
    registry *crawlrule.Registry,
    bindings map[string]papa.RuleRef,
    save func(context.Context, *papa.Task, papa.RulePage) error,
) error {
    const origin = "https://example.com"
    cfg := papa.RuleFetcherConfig{
        Site: "example", Stage: "catalog", Registry: registry,
        Bindings: bindings, Origins: []string{origin},
        Transport: "http", MaxPages: 20, MaxItems: 1000,
        ItemKey: "id", Save: save,
        Links: []papa.RuleLink{{Field: "href", Stage: "detail"}},
    }
    catalog, err := papa.NewRuleFetcher(cfg)
    if err != nil {
        return err
    }
    cfg.Stage, cfg.Links = "detail", nil
    detail, err := papa.NewRuleFetcher(cfg)
    if err != nil {
        return err
    }
    app.RegisterSites(papa.SiteSpec{
        Key: "example", BaseURL: origin,
        Stages: []papa.StageSpec{
            {Fetcher: catalog, WorkerCount: 2, QueueSize: 100},
            {Fetcher: detail, WorkerCount: 2, QueueSize: 100},
        },
    })
    entry, err := catalog.NewTask(papa.RuleRequest{URL: origin + "/list"}, nil)
    if err != nil {
        return err
    }
    return app.Engine.SubmitTask(entry)
}
```

这段代码显式投递入口，`StageSpec.Entry` 保持默认值。若使用 `Entry: true`，需要在自己的 wrapper 上实现 `SubmitEntries`；`RuleFetcher` 不猜测站点入口地址。`bindings` 至少包含 `catalog` 和 `detail`，版本必须已通过本进程 registry 的 fixture 激活。

`NewTask` 会把版本、请求状态、分页检查点和账号标识写入保留元数据，并生成任务身份 URL。不要自行拼接规则任务、覆盖 `_rule_*` / `rule_id` / `rule_version`，也不要把 `task.URL` 直接作为网络请求地址。真实请求地址由 fetcher 读取；JSON 游标任务即使访问相同 URL，也不会被普通 URL 去重合并。

规则及已验证状态目前只存在进程内存中。重启时须重新注册并验证仍有待执行任务引用的版本；回滚当前版本后，这些已验证版本仍可供旧任务执行。`Save` 与提交后续任务并非一个原子事务，重试可能再次保存成功页面，业务保存必须幂等。认证凭据留在运行期请求构建或账号管理器中，不能塞进游标或任务元数据。仅对可安全重放的读取型 POST 设置 `ReplaySafe: true`。
