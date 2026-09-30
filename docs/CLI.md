# Papa CLI 命令手册

> 面向：用 `papa` 命令行排查「页面实际抓到了什么」，以及生成新项目。
> 用途：`papa new` 生成项目骨架；`papa html/rod/diff/select` 是**调试命令**——复用生产配置（`config.yaml` 的 headers / proxy / timeout）单独抓一次页面，看真实结果，不用起整个爬虫。

---

## 0. 安装

```bash
go install ./cmd/papa          # 一次性安装，得到 papa 命令
# 不想装：在 papa 仓库根目录 go run ./cmd/papa <子命令> ...
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

> `rod` 的 headless 取自 `config.browser.headless`；**无配置文件时默认 headless**（不弹窗）。

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
