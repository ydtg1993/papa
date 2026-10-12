# 审查结果：`crawler/fetcher/huangguo` 与 `crawler/configs/config.yaml`

> 依据仅使用当前 `papa/docs` 与当前代码；未使用旧版 `crawler/docs`。以下按严重程度排序。

## 1. 严重：`config.yaml` 含已删除的 `scheduler` 配置，严格校验下会直接拒绝启动

- **位置**：`crawler/configs/config.yaml:119-122`
- **现状**：仍声明：
  ```yaml
  scheduler:
    timezone: "Asia/Shanghai"
  ```
  并注释称业务可通过 `app.RegisterCronJob` 注册。
- **依据**：
  - `papa/docs/CORE_CONFIG.md:3-4, 20-24` 明确说框架已移除通用 cron，未知配置键一律拒绝启动。
  - `papa/docs/REPEAT_QUEUE.md:78-84` 明确 `RegisterCronJob` 已移除。
  - 当前 `papa/config/config.go:11-37` 的 `Config` 没有 `Scheduler` 字段；当前 `papa/config/validate.go:14-17, 38-46` 会对未使用键 panic。
- **影响**：加载这份配置时，`scheduler` 会被认定为未知键，程序在启动阶段 panic；这不是“注释过时”而是实际启动阻断。
- **修复建议**：删除整个 `scheduler` 段及关于 `RegisterCronJob` 的注释。周期重抓用 `repeat_queue`；必须按固定时刻执行的业务，由业务自行调度并调用相应引擎入口。

## 2. 高：配置注释声称支持运行期热更和 `runtime.yaml`，与当前实现相反

- **位置**：`crawler/configs/config.yaml:44-46, 64-70`
- **现状**：注释称 OA `/api/config` 可热更 `browser.max_idle_time/headers`、`html.timeout/max_body_size/headers`，重启后还会叠加 `runtime.yaml`。
- **依据**：`papa/docs/CORE_CONFIG.md:3, 10-13` 明确：`config.yaml` 和 Go 站点声明均**没有运行期热更**，改后必须重启；老项目的 `configs/runtime.yaml` 已无人读写。
- **影响**：运维会误以为后台修改已生效或会持久化，实际进程仍使用启动时配置；抓取超时、请求头或浏览器回收参数排障时尤其容易被误导。
- **修复建议**：把两处“运行期热更”改为“改后需重启”，删除 `/api/config` 和 `runtime.yaml` 覆盖层说明。站点相关请求头继续只放 `configs/sites/huangguo.go`，并注明它同样需重启。

## 3. 高：`huangguo` 三个阶段名过于通用，违反多站阶段名应跨程序唯一的约定

- **位置**：
  - `crawler/fetcher/huangguo/fetch_catalog.go:56`：`catalog`
  - `crawler/fetcher/huangguo/fetch_detail.go:23`：`detail`
  - `crawler/fetcher/huangguo/fetch_episode.go:22`：`episode`
- **现状**：当前项目已有多个站点，但 huangguo 使用无站点前缀的通用阶段名。
- **依据**：`papa/docs/FETCHER_WRITING_GUIDE.md:38, 814, 846-857` 要求阶段名在整个程序中唯一；多站约定使用 `hgd_catalog` / `siteb_catalog` 一类站点前缀，复制站点却复用原阶段名会在注册时 panic。
- **影响**：当前若恰好没有重名可能暂时能跑，但下一站采用同样常见的 `catalog/detail/episode` 名称即会启动失败；阶段日志、归档目录和后台筛选也缺少站点辨识度。
- **修复建议**：统一改为如 `huangguo_catalog`、`huangguo_detail`、`huangguo_episode`，同步修改所有任务提交处的 `Stage` 字段、测试断言以及已存在任务的迁移/清理策略。不要只改 `GetStage()`。

## 4. 中高：默认开启全局 breaker，注释却宣称会暂停“所有阶段”，与当前按站点熔断语义不符

- **位置**：`crawler/configs/config.yaml:30-37`
- **现状**：注释称阈值达到后“把所有阶段的 worker 一起闸住”，且实际配置 `crawler.breaker.enabled: true`。
- **依据**：`papa/docs/CORE_CONFIG.md:52-58` 说明这里是**默认 scope**；每个站点可用 `SiteSpec.Breaker` 独立覆盖，熔断按站点分组，站点 A 被墙不应把站点 B 一起暂停。
- **影响**：一方面注释会误导值班人员，以为 huangguo/yeguodj 都会被全局停住；另一方面在多站项目里启用默认 scope 不等于为每个站点配置同一策略，容易造成“以为站点受保护、实际没有按预期覆盖”的运营偏差。
- **修复建议**：将注释改为“默认 scope 的 breaker”；若确实要保护 huangguo，应在 `crawler/configs/sites/huangguo.go` 的 `SiteSpec.Breaker` 显式声明该站策略，并为其他站分别决定是否启用。

## 5. 中：`crawler.stage_idle_timeout` 缺失，和当前多站/长跑推荐配置不一致

- **位置**：`crawler/configs/config.yaml:6-39`（`crawler` 段缺项）
- **现状**：未配置 `stage_idle_timeout`，因此阶段池默认常驻、不回收。
- **依据**：`papa/docs/CORE_CONFIG.md:49`：`stage_idle_timeout` 默认 `0` 表示不回收，多站/长跑建议配置（例如 `10m`）。当前项目已注册多个站点，且 catalog/detail 为周期任务，属于典型长跑场景。
- **影响**：空闲阶段池长期留在活池表并保留相关资源；随着站点/阶段增加，常驻资源会持续累积。它通常不立刻报错，但会形成长期运行问题。
- **修复建议**：在 `crawler` 下加入例如 `stage_idle_timeout: "10m"`，再根据阶段唤醒成本和业务延迟调整；若确实要求常驻，应显式写 `0` 并注释理由，避免让缺省值伪装成有意决策。

## 6. 中：分类页封面下载失败后“下轮会补”的注释过度承诺，实际也可能永久空缺

- **位置**：`crawler/fetcher/huangguo/fetch_catalog.go:174-186`
- **现状**：下载失败被降级为 `Trace.Warn` 并让 catalog 成功，注释称 repeatable 分类任务下次会再补。
- **依据**：
  - `papa/docs/FETCHER_WRITING_GUIDE.md:115-121` 支持用 `Warn` 表示非致命降级，但它不会使任务失败或触发重试。
  - 当前代码只有 catalog 再次遍历该条目时才会调用 `EnsureCover`；而 `parseCatalogPage` 只处理当前分类页上的条目。条目若在下一轮前被挤出分类分页、站点删除或入口分类发生变化，就不会再补。
- **影响**：任务显示成功，但 `cover_path` 可能永久为空；“下次一定补”会误导排障人员，也缺少一个可治理的失败任务。
- **修复建议**：至少把注释改为“后续分类轮询若再次遇到该条目会尝试补”；若封面完整性是业务要求，拆出独立、幂等的 cover 任务并允许正常重试，或建立缺失封面补偿扫描，而不是只依赖 catalog 偶遇。

## 7. 中：无分集的详情任务提前返回，未执行轮询状态同步

- **位置**：`crawler/fetcher/huangguo/fetch_detail.go:61-68`；轮询同步只在 `:102-103`
- **现状**：`/detail/` 页面解析出 `NoEpisodes` 时直接保存并返回，绕过 `syncDetailPolling`。只有进入播放页并解析完 `SerialStatus` 后才同步轮询。
- **依据**：当前代码自身约定见 `crawler/fetcher/huangguo/repeat.go:32-43, 70-96`：未知/未完结状态应继续轮询，且对还没有自身周期的任务补 6–9 小时。`papa/docs/REPEAT_QUEUE.md:43-50` 说明任务轮询状态和周期要显式通过任务字段/API 管理。
- **影响**：对新任务通常首次提交已经播种周期，表面无事；但迁移前老任务、人工构造任务或周期为 0 的行命中“暂无分集”分支时，永远不会执行补周期逻辑，只会退回全局 `repeat_queue.interval`（当前为 2h），造成远高于设计值的请求频率。
- **修复建议**：在该提前返回分支中先调用 `syncDetailPolling(engine, task, models.SerialStatusUnknown)`；或者重构成所有成功出口统一执行轮询同步，避免未来新增早退分支再次绕过。

## 补充说明

- 本审查没有把 `crawler/docs` 作为依据。
- `config.yaml:28` 的注释“负数 = 不永久清理”语义也有笔误；当前手册 `papa/docs/CORE_CONFIG.md:176` 明确负数表示**永久保留、不自动清理**，建议顺手改成“负数 = 永久保留”。
