# 审查结果：`crawler/fetcher/huangguo` 与 `crawler/configs/config.yaml`

## 审查口径

以当前框架源码及 `papa/docs/` 为准，未使用旧版 `crawler/docs/` 下结论。重点依据包括：

- `papa/docs/CORE_CONFIG.md`
- `papa/docs/FETCHER_WRITING_GUIDE.md`
- `papa/docs/REPEAT_QUEUE.md`
- `papa/docs/DATA_LANDING_GUIDE.md`
- `papa/docs/SCHEDULER_REDESIGN.md`
- 当前 `papa/engine`、`papa/config` 代码

以下按严重程度排序。

## 1. 严重：保留了已删除的 `scheduler` 配置，严格校验会直接阻止启动

**位置**

- `crawler/configs/config.yaml:119-122`

**现状**

配置仍声明：

```yaml
# 定时任务调度器：业务定时任务通过 app.RegisterCronJob 注册（框架不再内置 job）
scheduler:
  timezone: "Asia/Shanghai"
```

**问题与影响**

当前 Papa 已移除通用 cron、`RegisterCronJob` 和 `scheduler` 配置段。配置加载对未知键执行严格校验，因此这不是“无效但可忽略”的旧配置，而是可能在 `papa.New()` 读取配置时直接 panic、令进程无法启动。

**依据**

- `papa/docs/CORE_CONFIG.md:3-4,17-24`：框架已移除通用 cron；未知键一律拒绝启动。
- `papa/docs/SCHEDULER_REDESIGN.md:30-32,105-110,154-155`：明确删除 `RegisterCronJob`、`SchedulerConfig` 与 `scheduler` 段，并特别要求检查业务侧 YAML，否则启动 panic。
- `papa/docs/REPEAT_QUEUE.md:78-84`：间隔轮询应使用 repeat queue；指定时刻任务由业务自行调度。

**修复建议**

删除 `config.yaml:119-122` 整段。若需求是“每隔 N 小时重抓”，继续使用当前已有的 `repeat_queue` 和任务级 `Repeatable`/`RepeatInterval`；若确实需要每天固定时刻等 wall-clock 语义，应在业务层自行实现调度器，不再向 Papa YAML 添加 `scheduler`。

---

## 2. 高：三个阶段仍用进程全局的通用名称，存在跨站冲突，且修复涉及任务数据迁移

**位置**

- `crawler/fetcher/huangguo/fetch_catalog.go:56,70,196`
- `crawler/fetcher/huangguo/fetch_detail.go:23,132`
- `crawler/fetcher/huangguo/fetch_episode.go:22`
- 同类诊断标签及辅助字符串还散见 `fetch_catalog.go:122,130`、`fetch_detail.go:46,48`、`fetch_episode.go:42,44`、`repeat.go:66,95`

**现状**

`GetStage()` 与子任务均硬编码为 `catalog`、`detail`、`episode`。

**问题与影响**

阶段名在整个进程中全局唯一。当前项目已经按多站结构组织；另一站一旦复用任一通用名称，`RegisterSites` 会因重复阶段名在启动时 panic。大量散落字符串还容易在重命名时漏改，造成提交到不存在/错误阶段。

同时，不能只改 `GetStage()`：`crawler_tasks.stage` 保存旧名称，任务唯一约束也包含 stage。直接改代码而不迁移历史行，会令恢复、轮询、错误重投或后台重投找不到旧阶段，历史任务也可能与新阶段形成重复行。

**依据**

- `papa/docs/FETCHER_WRITING_GUIDE.md:38`：阶段名在整个程序必须唯一，多站应加前缀。
- 同文档 `810-815,846-857`：推荐 `hgd_catalog` 等可区分名称，重名启动即报错。
- `papa/docs/DATA_LANDING_GUIDE.md:54-63`：数据库唯一约束为 `(url_hash, stage)`，stage 是持久化任务身份的一部分。

**修复建议**

1. 在包内集中定义常量，例如 `StageCatalog = "hgd_catalog"`、`StageDetail = "hgd_detail"`、`StageEpisode = "hgd_episode"`（也可用完整 `huangguo_` 前缀）。
2. `GetStage()`、所有 `Task.Stage`、测试、诊断标签中统一复用这些常量，不再散落裸字符串。
3. 制定迁移：在停机或受控窗口内更新历史 `crawler_tasks.stage`，并评估 `(url_hash, stage)` 唯一索引冲突；确认 error/recover/repeat/manual-requeue 所有旧行已迁移后再部署新名称。
4. 迁移前运行 `papa check` 验证声明；迁移后抽查旧任务能被正确重投。

---

## 3. 高：配置注释宣称存在运行期热更与 `runtime.yaml` 覆盖，实际当前版本完全不支持

**位置**

- `crawler/configs/config.yaml:44-46`
- `crawler/configs/config.yaml:64`
- `crawler/configs/config.yaml:70`

**现状**

注释声称浏览器的 `max_idle_time / headers` 和 HTML 的 `timeout / max_body_size / headers` 可通过 OA `/api/config` 热更，重启后由 `runtime.yaml` 叠加。

**问题与影响**

当前手册明确所有 `config.yaml` 修改均需重启，`configs/runtime.yaml` 已无人读写。该注释会误导运维人员以为更改已即时生效，导致请求头、超时、响应体上限等关键参数实际仍使用旧值；排查反爬、超时和大页面错误时尤其危险。

**依据**

- `papa/docs/CORE_CONFIG.md:3,8-13`：没有运行期热更；修改 YAML 或 Go 站点声明均需重启；旧 `runtime.yaml` 可以删除。
- 同文档 `141-157,199-229` 也说明治理队列参数启动时读取，修改后需重启。

**修复建议**

将相关注释统一改成“所有配置修改后均需重启”；删除 `/api/config` 与 `runtime.yaml` 覆盖层的描述。若仓库里仍有遗留 `configs/runtime.yaml`，另行确认无业务自读逻辑后删除，但本次审查不应修改源码或其他文件。

---

## 4. 中高：分类封面重试说明与实际任务周期不一致，可能造成错误运维预期

**位置**

- `crawler/fetcher/huangguo/fetch_catalog.go:174-176`
- 对照实际周期：`crawler/fetcher/huangguo/repeat.go:17`、`fetch_catalog.go:75-77`
- 对照全局 fallback：`crawler/configs/config.yaml:136-140`

**现状**

封面下载失败后的注释写道：“分类任务是 repeatable 的（repeat_queue 每 2h 重投），下次抓取会再补一次。”但入口任务明确播种了每条任务随机 `12-20h` 的 `RepeatInterval`；全局 `2h` 只在任务周期为 0 时作为 fallback。

**问题与影响**

这会让维护者错误估计非致命封面失败的恢复时效，以为约 2 小时会补抓，实际通常要等待 12–20 小时。若运营据此设置告警或 SLA，会长期看到 cover_path 为空却误以为轮询机制失效。

**依据**

- `papa/docs/REPEAT_QUEUE.md:8-10,26-30,41-46`：周期是每任务的 `repeat_interval`；全局 `interval` 是扫描兜底/任务周期为 0 时的 fallback。
- `papa/docs/CORE_CONFIG.md:224-229`：任务级 `RepeatInterval` 优先，0 才用全局 interval。

**修复建议**

把注释改为“下次任务级轮询（当前首次播种为随机 12–20 小时）会再尝试；只有 `repeat_interval=0` 才使用全局 2h”。若业务要求封面更快恢复，不应仅改注释，应把封面下载拆成独立、可重试的任务，或将失败按普通错误处理；需明确权衡对分类主任务成功率的影响。

---

## 5. 中：`siteOf` 的注释把缺站点错误错误归因于“阶段声明漏站”，会误导排障

**位置**

- `crawler/fetcher/huangguo/site.go:9,13,17-18`

**现状**

注释/错误提示称，`engine.Site(task.Site)` 查不到可能是“阶段声明里漏了这个站”或 `configs/sites/` 少声明。

**问题与影响**

当前站点归属不是 fetcher 在 handler 内推断的；任务提交时持久化 `Site`，引擎按已注册 `SiteSpec.Key` 查声明。若当前 huangguo 阶段已经成功注册，但这里查不到，更常见原因是历史任务的 `site` 为空/错误、手工构造任务漏填、或站点 key 改名后未迁移，而不是“阶段声明里漏了站”。错误提示可能把排障方向带到错误文件。

**依据**

- `papa/docs/FETCHER_WRITING_GUIDE.md:860-868`：handler 通过 `engine.Site(task.Site)` 读取声明；任务表持久化 site，跨站派发应显式携带站点。
- 当前代码也显示入口及子任务均显式设置 `Site`（`fetch_catalog.go:71,197`、`fetch_detail.go:133`）。

**修复建议**

保留不可重试的 configuration 分类，但将错误信息改为同时给出真实排查项，例如：“任务 site 为空/未知；检查历史 `crawler_tasks.site`、提交点是否填写 Site、SiteSpec.Key 是否改名以及站点是否注册”。注释也应去掉“阶段声明里漏了这个站”的单一归因。

---

## 6. 中：对“所有阶段池已建好”的注释已与当前懒建工作池实现不符

**位置**

- `crawler/fetcher/huangguo/fetch_catalog.go:58-61`

**现状**

`SubmitEntries` 注释称框架调用它时“所有阶段的池子都已建好”。

**问题与影响**

当前 Papa 的阶段池已经改为首次提交时懒建，并可按空闲时间回收。该说法虽不一定立即导致功能故障，但会误导后续开发者依赖常驻池、错误解读监控中尚未出现的阶段统计，或设计不必要的启动顺序约束。

**依据**

- `papa/docs/SCHEDULER_REDESIGN.md:112-120,124-130,135-143`：阶段池已完成“首次 submit 懒建 + 空闲回收”；首次提交前某阶段可没有活池。
- `papa/docs/FETCHER_WRITING_GUIDE.md:803-808` 只保证入口回调可以向任意已注册阶段提交，并不要求所有池预先创建。

**修复建议**

改为：“框架在阶段声明完成后调用入口回调，可向任意已注册阶段提交；目标阶段池会在首次提交时按需创建。”不要让业务代码依赖池是否已经存在。

---

## 7. 中低：无法识别连载状态时使用普通日志，而不是已约定的结构化 Trace Warn

**位置**

- `crawler/fetcher/huangguo/fetch_detail.go:103-109`

**现状**

注释称“留个 Warn”，实现却调用 `engine.GetLoggerSet().Engine.Warnf`，没有把 `series_id`、原始状态等信息挂到任务追踪。

**问题与影响**

当前约定把可恢复、非致命的业务异常放入 `task.Trace.Warn`，使其与具体任务尝试关联并在后台追踪中可查询。普通引擎日志容易被轮询任务反复刷屏，且缺少任务级结构化上下文；维护者从任务页看不到“状态无法识别”的降级原因。

**依据**

- `papa/docs/FETCHER_WRITING_GUIDE.md:91-124`：非致命异常用 `Trace.Warn`；Warn 的 data 在成功尝试中也会保留。
- 同文档 `59-60,121`：日志由框架兜底，trace 用于业务步骤和可查询上下文；不应以泛化日志替代任务追踪。

**修复建议**

改用类似：

```go
task.Trace.Warn("无法识别连载状态", nil, map[string]string{
    "series_id": fmt.Sprint(seriesID),
    "raw_status": playback.SerialStatusRaw,
    "fallback": "continue_polling",
})
```

若仍希望系统日志有聚合告警，应通过明确的指标/通知机制做限频聚合，而不是每次轮询直接 `Warnf`。

## 建议修复顺序

1. 立即删除 YAML 的 `scheduler` 段，先恢复可启动性。
2. 修正热更新/`runtime.yaml` 注释，避免继续误操作。
3. 设计并执行阶段名前缀化的数据迁移，再统一常量替换代码中的 stage 字符串。
4. 修正 repeat 周期、站点错误和懒建池相关注释。
5. 将无法识别状态的普通日志改为任务级 `Trace.Warn`。

本审查仅生成报告，未修改任何项目源文件。