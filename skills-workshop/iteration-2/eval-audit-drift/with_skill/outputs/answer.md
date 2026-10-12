# Eval 2：`huangguo` 与 `config.yaml` 漂移审查

## 审查口径

本审查以当前 `papa/` 的导出代码、测试和 `papa/docs/` 为权威，不使用旧的 `crawler/docs/` 作结论依据。重点依据包括：

- `papa/docs/README.md:3,17-20,47`：框架已移除通用 cron；阶段名在整个进程内唯一，多站点应带站点前缀。
- `papa/docs/CORE_CONFIG.md:3-13,17-24`：配置改动全部需重启，`runtime.yaml` 已废弃；未知 YAML 键会拒绝启动。
- `papa/docs/FETCHER_WRITING_GUIDE.md:38`：`GetStage()` 返回值必须全局唯一，多站点示例为 `hgd_catalog`。
- `papa/docs/SCHEDULER_REDESIGN.md:30-32,154-155`：`admin/scheduler`、`RegisterCronJob` 和 `scheduler` 配置段均已移除，旧段会触发未知键校验。
- 当前代码 `papa/config/config.go:11-36,309-344` 与 `papa/config/validate.go:14-17,38-46`：`Config` 不含 `scheduler` 字段，解码后的 unused key 会导致 panic。
- 当前代码 `papa/models/crawler_task.go:40-43`、`papa/engine/task.go:105-128`：`Task.Meta` 写入 `crawler_tasks.meta`，并在恢复、轮询、错误队列及后台重投时还原。

## 按严重程度排序的发现

### 1. 严重：遗留 `scheduler` 配置会导致启动时 panic

**位置**

- `crawler/configs/config.yaml:119-122`
  - 注释称业务定时任务通过 `app.RegisterCronJob` 注册。
  - 实际仍声明了 `scheduler.timezone`。

**依据**

- `papa/docs/README.md:3` 明确说明框架不再内置通用 cron 调度器。
- `papa/docs/SCHEDULER_REDESIGN.md:30-32` 明确删除 `admin/scheduler`、`RegisterCronJob` 和 `scheduler` 配置段。
- `papa/config/config.go:11-36` 的当前 `Config` 没有 `Scheduler` 字段；`Load` 会把无法解码的键记录为 unused，并在 `Validate` 中拒绝。
- `papa/config/validate.go:14-17,38-46` 明确未知键一律拒绝启动并 panic。
- 当前 `papa/**/*.go` 中也不存在 `RegisterCronJob` 导出符号。

**影响**

这不是无效注释而已：加载该 YAML 时，顶层 `scheduler` 是未知键，应用可能在完成站点注册和启动 worker 前直接 panic。注释还会诱导后续代码调用已经删除的 API。

**修复建议**

- 删除 `crawler/configs/config.yaml:119-122` 整个 `scheduler` 段及 `RegisterCronJob` 注释。
- “每隔 N 时间重抓”改用 `Task.Repeatable` / `Task.RepeatInterval` 加全局 `repeat_queue`；本项目已经配置了 `repeat_queue.enabled: true`。
- 若业务确实需要“每天 08:00”之类墙钟语义，应由业务层自有调度器实现，而不是恢复 Papa 已删除的 cron API。
- 删除后运行 `papa check <crawler-path>`；数据库启动校验有条件时再运行 `papa check <crawler-path> --boot`。

### 2. 高：`huangguo` 三个阶段名未加站点前缀，存在进程级冲突

**位置**

- `crawler/fetcher/huangguo/fetch_catalog.go:56`：`GetStage() == "catalog"`
- `crawler/fetcher/huangguo/fetch_detail.go:23`：`GetStage() == "detail"`
- `crawler/fetcher/huangguo/fetch_episode.go:22`：`GetStage() == "episode"`
- 同一组裸字符串也出现在任务提交处：
  - `fetch_catalog.go:70` 提交 `catalog`
  - `fetch_catalog.go:196` 提交 `detail`
  - `fetch_detail.go:132` 提交 `episode`
- `crawler/configs/sites/huangguo.go:15-16` 自己的注释已经要求阶段带站点前缀，但实现未遵守。

**依据**

- `papa/docs/FETCHER_WRITING_GUIDE.md:38`：阶段名在整个程序里必须唯一；多站点必须带前缀，例如 `hgd_catalog`。
- `papa/docs/README.md:47` 同样要求多站点阶段名带站点前缀。
- `papa/docs/SCHEDULER_REDESIGN.md:58-60` 说明引擎阶段表以 stage 名为 key，阶段名是进程级唯一标识。

**影响**

新增另一个使用 `catalog`、`detail` 或 `episode` 的站点时，`RegisterSites` 会因重复阶段名拒绝启动。更隐蔽的是，只改 `GetStage()` 而漏改任务的 `Stage` 字符串，会让任务提交到不存在的旧阶段。

**修复建议**

- 在 `fetcher/huangguo` 中集中声明常量，例如：

  ```go
  const (
      StageCatalog = "hgd_catalog"
      StageDetail  = "hgd_detail"
      StageEpisode = "hgd_episode"
  )
  ```

- `GetStage()`、`SubmitEntries`、所有子任务提交、相关测试和日志标签统一引用这些常量，避免裸字符串漂移。
- **迁移注意**：`crawler_tasks.stage` 持久化了旧值。不能只部署新代码后让旧行继续存在，否则恢复、错误重投、周期轮询或后台重投会尝试投递到已不存在的阶段。部署前应备份数据库，并设计一次性数据迁移，将该站点的旧 `catalog/detail/episode` 行改为新阶段名；若数据库中存在其他站点共用裸阶段名，必须用 `site = 'huangguo'` 等条件限定，不能全表替换。还应检查与阶段相关的归档目录、监控筛选和运维脚本。

### 3. 中高：配置注释宣称支持 OA / `runtime.yaml` 热更，与当前配置契约相反

**位置**

- `crawler/configs/config.yaml:44-46`：声称浏览器 `max_idle_time / headers` 可在 OA `/api/config` 热更，并在重启后叠加 `runtime.yaml`。
- `crawler/configs/config.yaml:64`：声称 HTML 的 `timeout / max_body_size / headers` 可运行期热更。
- `crawler/configs/config.yaml:70`：再次区分站点声明和“这一层 headers 可热更”。

**依据**

- `papa/docs/CORE_CONFIG.md:3-4`：全部配置修改后都需要重启，没有热更。
- `papa/docs/CORE_CONFIG.md:10-13`：`config.yaml` 和 Go 站点声明均需重启；旧 `configs/runtime.yaml` 已无人读写，可以删除。
- 当前 `papa/engine/runtime_test.go` 的测试注释也明确运行期覆盖层已经随热更取消。

**影响**

运维人员可能在 OA 中寻找并不存在或不再持久化的配置入口，或者编辑废弃的 `runtime.yaml` 后误以为 timeout、body 大小、headers 已生效。实际进程仍使用启动时配置，容易造成限速、超时、请求头和故障排查判断错误。底层包存在 `SetConfig`/`SetHeaders` 方法不等于业务配置文件仍有公开热更契约；应以当前 App 接线和手册为准。

**修复建议**

- 将上述注释统一改为：`config.yaml` 中所有字段修改后均需重启。
- 删除所有 `/api/config` 和 `runtime.yaml` 覆盖层描述；若仓库仍有 `configs/runtime.yaml`，确认无业务自定义读取后删除。
- 可以保留“哪些字段属于浏览器/HTML 配置”的说明，但不要承诺动态生效。

### 4. 中：repository 注释错误地宣称 `Task.Meta` 是内存态、不落库

**位置**

- `crawler/models/repository.go:62-66`：`FindSeriesBySourceURL` 上方注释称 `Task.Meta` 不落库，`crawler_tasks` 没有 meta 列，恢复/轮询/错误队列重建任务时天然没有 Meta。
- `crawler/models/repository.go:75` 又让 episode URL 回查沿用同一错误理由。

**依据**

- `papa/docs/FETCHER_WRITING_GUIDE.md:81-82`：`Meta` 会写入 `crawler_tasks.meta`。
- `papa/models/crawler_task.go:40-43` 明确声明 JSON `Meta` 列。
- `papa/engine/task.go:105-128` 的统一 `taskFromRecord` 会在恢复、轮询、错误队列和后台动作路径中还原 `Meta`。
- 同一业务包中的 `crawler/fetcher/huangguo/fetch_detail.go:149-156` 和 `fetch_episode.go:87-93` 已经写出了正确语义，因此 repository 注释与当前代码内部也互相矛盾。

**影响**

该注释会误导维护者把业务身份继续编码进 URL，或错误判断恢复/轮询任务必然丢失 `series_id`/`episode_id`，从而设计多余且脆弱的查找逻辑。它也可能掩盖真正的异常：若新任务重建后没有 Meta，应检查迁移、提交字段或坏数据，而不是归因于框架设计。

**修复建议**

- 保留 `FindSeriesBySourceURL` / `FindEpisodeBySourceURL` 作为兼容兜底，但更新注释：Meta 当前会持久化并恢复；URL 回查只用于迁移前 `meta` 为 NULL 的旧行、损坏数据或上游原本未写 Meta 的任务。
- 确认数据库已执行当前 `papa migrate`，使 `crawler_tasks.meta` 列存在。
- 新提交链路继续优先写稳定业务键到 `Task.Meta`，URL 回查不要成为主身份机制。

### 5. 低：分类封面注释把全局 `repeat_queue.interval` 错当成每条分类任务的实际周期

**位置**

- `crawler/fetcher/huangguo/fetch_catalog.go:174-176`：注释说分类任务由 repeat queue “每 2h 重投”。
- 但同文件 `fetch_catalog.go:74-77` 实际为每条分类任务设置随机 `12-20h` 的 `RepeatInterval`；`repeat.go:17` 也定义为 12–20 小时。

**依据**

- `papa/docs/REPEAT_QUEUE.md:10,26-28,41-45`：每条任务自己的 `repeat_interval` 决定实际周期；全局 `repeat_queue.interval` 是最粗扫描兜底，任务周期为 0 时才作为有效周期。
- `crawler/configs/config.yaml:136-140` 的 `2h` 是全局队列 interval，不会覆盖已设置 12–20 小时周期的任务。

**影响**

这通常不会直接改变运行结果，但会让维护者高估封面失败后的补偿频率，并可能错误调节全局扫描周期来解决单任务周期问题。

**修复建议**

- 把注释改为“分类任务按各自 12–20 小时周期由 repeat queue 重投；全局 2h 是扫描兜底，不是该任务的实际轮询周期”。
- 若业务希望更快补封面，应显式调整每条任务周期或设计独立补偿机制，不要依赖修改全局扫描间隔。

## 建议的修复顺序与验证

1. 先删除 `scheduler` 段，恢复配置可加载性。
2. 规划并执行持久化阶段名迁移，再把 `huangguo` 的阶段常量与所有任务提交点统一改为带前缀的名称；两者需要作为同一次受控发布处理。
3. 清理热更/`runtime.yaml` 注释，避免继续误导运维。
4. 更正 repository 的 Meta 注释和分类轮询注释。
5. 在业务模块执行：

   ```text
   go test ./fetcher/huangguo/...
   go test ./...
   go vet ./...
   papa check <crawler-path>
   ```

6. 若阶段迁移或 Meta 列涉及数据库，先备份并审查迁移 SQL，再运行显式迁移/校验；不要依赖普通启动自动建表。

以上仅为审查结论；未修改 `crawler/` 或 `papa/` 项目源码。
