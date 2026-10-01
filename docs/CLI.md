# Papa CLI 命令手册

> 面向：用 `papa` 命令行排查「页面实际抓到了什么」，以及生成新项目。
> 用途：`papa new` 生成项目骨架；`papa html/rod/diff/select` 是**调试命令**——复用生产配置（`config.yaml` 的 headers / proxy / timeout）单独抓一次页面，看真实结果，不用起整个爬虫。

---

## 0. 安装

```bash
# 免克隆安装（推荐）：直接装最新版，得到 papa 命令
go install github.com/ydtg1993/papa/v2/cmd/papa@latest

# 已在 papa 仓库内（贡献/调试）：装当前源码
go install ./cmd/papa

# 不想装：在仓库根目录 go run ./cmd/papa <子命令> ...
```

## 1. 命令总览

| 命令 | 作用 |
| --- | --- |
| `papa new <name> [--replace <path>]` | 生成新爬虫项目骨架 |
| `papa html <url> [flags]` | 静态 HTML 抓取（htmlfetch，不走浏览器） |
| `papa rod <url> [flags]` | 浏览器渲染后抓取（Rod，走完整 JS 渲染） |
| `papa diff <url> [flags]` | 同一 URL 分别用 html 与 rod 抓取并对比 |
| `papa select <css> <url\|文件> [flags]` | 选择器测试（URL 抓取后查，或本地 html 文件离线查） |

## 2. 通用 flags（html / rod / diff 共用）

| Flag | 说明 |
| --- | --- |
| `-c, --config <path>` | 复用项目配置；回退链 `PAPA_CONFIG` → `configs/config.yaml`。**可缺省**：零配置也能跑（用内置默认 UA） |
| `--proxy` | 走代理（读 `config.proxy`）；**默认直连**，避免调试时烧代理配额 |
| `--timeout <duration>` | 覆盖请求/导航超时 |
| `--ua <string>` | 覆盖 User-Agent |
| `--header "k=v"` | 追加/覆盖请求头（可重复） |
| `--json` | 输出结构化 JSON（见第 5 节） |
| `-o, --output <file>` | HTML 落盘到文件 |

> flag 可放在 URL 前后任意位置，如 `papa html https://x --json` 或 `papa html --json https://x` 都行。

## 3. 输出形态（html / rod，选一个，默认输出 HTML）

| Flag | 输出 |
| --- | --- |
| （默认） | HTML 原文 |
| `--links` | 所有 `<a href>`（按最终 URL 绝对化，一行一个） |
| `--text` | `<body>` 可读文本 |
| `--select <css>` | 选择器匹配节点的文本（`序号\t文本`） |

## 4. rod 专属 flags

| Flag | 说明 |
| --- | --- |
| `--screenshot <file>` | 保存 PNG 截图 |
| `--full-page` | 整页截图（配合 `--screenshot`） |
| `--wait <duration>` | 加载完成后额外等待，暴露懒加载/异步渲染内容 |
| `--act <动作>:<参数>` | 页面动作，可重复，按序执行（见下） |
| `--show` | 有头浏览器 + 人工操作，回车后导出结果 |
| `--devtools` | 同 `--show`，并自动打开 DevTools |

> `rod` 的 headless 取自 `config.browser.headless`；**无配置文件时默认 headless**（不弹窗）。
> `--show` / `--devtools` 强制有头。

### `--act` 动作

静态抓取永远只能拿到首屏。涉及下拉加载、点「加载更多」、切 Tab 的页面，用 `--act` 把交互写进命令：

| 动作 | 说明 |
| --- | --- |
| `scroll:bottom` | 滚到文档底部 |
| `scroll:top` | 回到顶部 |
| `scroll:<N>` | 向下滚 N 屏（`scroll:3`） |
| `scroll:<css>` | 滚动到该元素可见 |
| `click:<css>` | 点击元素（自动先滚入视口） |
| `input:<css>=<文本>` | 清空并输入文本 |
| `hover:<css>` | 悬停 |
| `wait:<duration>` | 固定等待（`wait:2s`） |
| `wait:<css>` | 等到元素出现 |
| `eval:<js>` | 在页面上下文执行任意 JS，返回值打到 stderr（`eval => ...`） |

> `wait:` 的参数能解析成时长就是等待，否则按 CSS 选择器处理——合法选择器不可能是合法时长，不会歧义。
>
> `eval:` 收的是**表达式或语句**（`document.title`、`document.title='x'`、`document.querySelectorAll('.item').length` 都行），
> 不是函数——内部会包一层 `eval()` 求值，返回值打到 stderr。页面 CSP 禁止 `eval` 时会报错。
>
> `--timeout`（默认 30s）同时作用于导航与**每个动作**（等元素、点击等），动作卡住不会无限等待。

**每个动作结束后会自动等页面稳定**（DOM 不再变化、网络不再请求，最多等 5 秒，超时只告警不中断），
避免下拉完立刻取值拿到半截数据。有些页面（轮播、时钟）永不静止，这时每个动作会固定耗满 5 秒，
可在动作后补 `--act wait:2s` 并接受告警。

`--act` 顺序执行，任一动作失败立即报错退出（不会静默跳过），并带上是第几个动作失败。

**无限滚动的页面**一次 `scroll:bottom` 往往只加载一屏，重复写多次；`scroll:3` 也行：

```bash
# 下拉三次触发懒加载，再取所有条目标题
papa rod "https://example.com/list" \
  --act scroll:bottom --act wait:1s --act scroll:bottom --act wait:1s --act scroll:bottom \
  --select ".item-title"

# 点「加载更多」按钮两次
papa rod "https://example.com/list" --act click:.load-more --act wait:.item:nth-child(21)

# 搜索框输入后查询
papa rod "https://example.com" --act input:#kw=火影 --act click:.search-btn --act wait:.result

# 页面上滚到某个区块并截图
papa rod "https://example.com" --act scroll:#comments --screenshot comments.png

# 直接取值：看列表实际渲染了多少条
papa rod "https://example.com/list" --act scroll:bottom --act "eval:document.querySelectorAll('.item').length"
```

### `--show` 人工操作

调反爬、验证「到底要点哪里」时，最省事的是让浏览器开着、自己点，点完再导出：

```bash
papa rod "https://example.com/detail/1" --show --select ".episode-list"
papa rod "https://example.com" --devtools --screenshot page.png
```

流程：打开有头浏览器 → 你在窗口里操作（下拉、点击、登录都可以）→ 回终端按回车 →
命令按 `--select` / `--text` / `--links` / `--screenshot` 导出**当前**页面状态。

`--show` 可与 `--act` 组合：先自动跑一遍动作，再交给你手动补充。

> stdin 不是终端（管道、重定向）时不会等待回车，直接导出，避免脚本里卡住。

## 5. JSON 输出结构

**`--json`（html / rod，默认 HTML 形态）**：

```json
{
  "engine": "html",          // 或 "rod"
  "input_url": "https://...",
  "final_url": "https://...", // 重定向后
  "status": 200,
  "content_type": "text/html",
  "title": "Example Domain",
  "html_len": 12732,
  "elapsed_ms": 309
}
```

**`--json` + `--links` / `--text` / `--select`** → 包一层 meta：

```json
{ "meta": { "...上述字段..." }, "items": ["https://...", "..."] }
```

**`diff --json`**：

```json
{
  "input_url": "https://...",
  "html": { "status": 200, "final_url": "...", "html_len": 713 },
  "rod":  { "status": 200, "final_url": "...", "html_len": 12732 },
  "html_equal": false,
  "links": { "html_only": [], "rod_only": [], "common": 1 }
}
```

**`select --json`**：

```json
{ "selector": "a", "count": 2, "items": ["第一项", "第二项"] }
```

## 6. 典型排查场景

```bash
# 看某页静态 HTML 到底拿到了啥
papa html "https://example.com/list" --json

# 看浏览器渲染后的 HTML（JS 动态内容）
papa rod "https://example.com/detail/1" -o rendered.html

# 对比「静态 vs 渲染」——判断这页到底需不需要浏览器
papa diff "https://example.com/list" --json

# 测一个选择器还能不能匹配到内容（本地 html 文件离线反复调）
papa html "https://example.com/list" -o page.html
papa select ".item a" page.html

# 截图看反爬拦截页 / 验证码
papa rod "https://example.com" --screenshot shot.png --full-page --wait 3s

# 复用生产代理 + 自定义 UA
papa rod "https://example.com" --proxy --ua "Mozilla/5.0 ..." --json
```

## 7. 说明

- **零配置**：不指定 `--config` 且无 `configs/config.yaml` 时也能跑，用内置默认请求头；`rod` 默认 headless。
- **退出码**：0 = 成功，1 = 抓取失败或非 2xx，便于脚本串联。
- **`--header` 格式**：`--header "User-Agent=xxx"`（`key=value`），可多次传入。
- **`-o` 与 `--json`**：`-o` 把 HTML 落盘；带 `--json` 时 JSON 走 stdout、HTML 落盘。
