# 图册资源与维护

> 返回 [Papa 图册](PAPA_OVERVIEW.md) · [架构导航](README.md)。适用当前独立 v4。

图册资源在 `docs/assets/diagrams/`：每个主题一份 `.mmd` 源码、一份 SVG 和一份 PNG。文件在 [图册正文](PAPA_OVERVIEW.md) 中逐一引用；[manifest.json](../assets/diagrams/manifest.json) 记录工具版本、尺寸及源码/两种产物的 SHA-256，用于发现忘记重生成或只更新一种格式的情况。哈希证明文件配套，不证明图中行为正确。

目录内 `.gitattributes` 固定图源、SVG 与 manifest 使用 LF，PNG 为二进制，避免 Windows 检出时行尾转换破坏源码/产物哈希。该仓库规则不随手册副本分发。

## 源码仓库生成

源码仓库的 `tools/docs/package.json` 固定 Mermaid/Playwright 直接依赖版本，`tools/docs/render-diagrams.cjs` 读取 `.mmd` 并统一渲染标题、注记和主题色。它不属于爬虫运行路径；分发副本保留图源和图片，不分发 Node 工具目录。

```sh
npm install --prefix tools/docs
cd tools/docs
npx playwright install chromium
npm run render
```

也可用环境变量 `PAPA_DOCS_BROWSER` 指定已有 Chromium/Chrome 可执行文件，无需下载浏览器。需要中文字体（Microsoft YaHei 或 Noto Sans CJK SC）；字体、浏览器及传递依赖变化可能改变排版和哈希，不能承诺跨机器字节完全一致。渲染本身不访问外网、业务数据库或站点。

## 更新检查

1. 对照所属组件源码修改 `.mmd`，核对箭头是调用、条件转换还是观测；运行关系不等于 Go import。
2. 重新生成 SVG、PNG 与 manifest，逐张检查中文、箭头、换行、图例和裁切；PNG 预览与 SVG 下载须表达同一份内容。
3. 同步图册正文、详细 Mermaid 图与对应职责分册。新增资源显式纳入 `docs/embed.go` 白名单。
4. 运行 `GOWORK=off go test ./cmd/papa`，检查源码/产物哈希、导航与图片链接、新建/升级完整复制及 PNG 字节一致性。

`papa new/upgrade` 复制当前手册及这组图片/图源/manifest，保留相对层级。history、工具脚本和 Node 依赖不分发。`--missing-only` 保留已有图片，显式 `upgrade --write` 更新框架拥有的资源；项目自身目录、技能约定和旧路径副本仍保留。
