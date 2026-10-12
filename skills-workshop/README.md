# skills-workshop

`papa-crawler-development` 那个技能的**工场**：skill-creator 的评测定义、历次评测记录与复核页面。

它**不发给业务项目**，也不属于框架手册 —— 框架手册只有 `docs/` 下那 10 份
（`docs/embed.go` 只嵌顶层的 `*.md`，`copyDocs` 也只拷顶层文件）。放在这里是为了让 `docs/`
只表示一件事：**发给业务项目的手册**。

## 唯一事实源在别处

技能**交付给业务项目的那一份**在 `cmd/papa/templates/skill_*.tmpl`：`papa new` 按它写入
`<项目>/.claude/skills/papa-crawler-development/`，`papa upgrade` 按它刷新通用文件。
本目录**不保留技能源文件的副本** —— 以前两边各有一份，必然漂。

- 改技能内容 → 改 `cmd/papa/templates/skill_*.tmpl`（文件清单在 `cmd/papa/skill.go`），跑 `go test ./cmd/papa/`。
- 要重新评测 → 先用 `papa new <临时项目>` 生成一份，把生成出来的
  `.claude/skills/papa-crawler-development/` 当被评对象（它才是真正交付的形态）。

## 目录里的东西

| 文件 | 是什么 |
| --- | --- |
| `evals/evals.json` | 评测用例定义（三个场景：新站接入 / 文档漂移审计 / 后台表格+动作） |
| `iteration-1/`、`iteration-2/` | 历次评测的原始记录（`with_skill` / `without_skill` 的答案与评分）。**冻结**：里面写的是当时的路径 `papa/docs/skills/...`，技能已搬到 `cmd/papa/templates/`，别按记录里的路径找文件 |
| `benchmark.md` / `benchmark.json` | 汇总：无 skill 15/18 → 有 skill 18/18（增量来自「repeat_queue 必须全局开启」与「`Task.Meta` 仅内存」那条过期注释） |
| `skill-review.md` / `validation.md` | 评审与校验记录 |
| `package-manifest.md` | 打包清单。那份 `.skill` 包**已删** —— 它是旧内容的副本；要重新打包就用 skill-creator 从 `cmd/papa/templates/` 那几份生成 |
| `review.html` / `review-data.{js,json}` | 人工复核页面（打开 `review.html` 即可看） |
