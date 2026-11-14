# Docs 目录编排实施计划

<!-- version-scope -->
> 适用版本：独立 Papa v4 当前工作区，2026-11-14。用户已授权目录整理；本计划随历史材料移入 `docs/history/plans/`，仅改工作区，不执行 Git 写操作。
<!-- /version-scope -->

**Goal:** 将当前手册按用途编排，集中历史材料，并保证导航、嵌入分发和脚手架引用可用。

**Architecture:** `docs/README.md` 是统一入口；architecture、configuration、guides、accounts、runtime、operations 拥有各自使用手册和导航；`package_structure` 保留包职责说明。history 只保存仓库审查、设计、计划和变更记录，不进入分发。

**Tech Stack:** Markdown、Go embed、现有 CLI 递归复制与升级检查、Go 测试、本地 Python 链接校验。

## 执行清单

- [x] 建立明确的旧路径→新路径清单；迁移前保存原有内容供核对，移动路径必须位于 Papa 仓库内，目标已存在则拒绝覆盖。
- [x] 移动 22 份当前手册及历史材料；包职责目录不移动；按原文件路径重新计算所有本地 Markdown 链接，同步仓内源码注释、模板和测试中的文档路径。
- [x] 为六组手册和历史材料提供导航；根导航按读者目的索引；更新结构说明、CLI 生成目录及历史分发边界。记录 `upgrade` 不删除调用方旧文件的既有契约。
- [x] `docs/embed.go` 使用显式当前目录白名单；保留 `embeddedDocs`/`copyDocs`/`writeDocs` 已有递归职责，不新增路径兼容层或自动删除旧项目文件。
- [x] 扩展 `cmd/papa/docs_structure_test.go`：校验六组目录及架构图完整复制、history 排除、分发内部链接可解析、missing-only 保留修改、upgrade 只刷新当前手册且不删旧/自有文件。
- [x] 运行 `GOWORK=off go test ./cmd/papa . ./docs -count=1`（含真实脚手架 tidy/build）、`go vet ./cmd/papa ./docs`、仓内当前/历史链接与旧路径检查、`git diff --check`；回填实际结果。

完成后由用户自行 review/提交；不额外创建分支、工作树或操作业务项目。

## 实施与验证结果

移动清单共 105 份文件（含本计划）；六组导航补齐后当前分发 37 份 Markdown，历史目录含 86 份 Markdown。根目录仅保留 README.md 和 embed.go，包职责目录不移动。690 个当前及历史相对链接检查通过，移动目标全部存在，源路径不残留重复文件。

新 `TestEmbeddedManualLinksAndNavigation` 在白名单未更新时真实报出 `undistributed or missing link`，更新后通过；它同时检查总导航可达性和链接目标确实被嵌入。完整 CLI 测试发现旧 `TestRunNewScaffoldsCompleteProject` 只允许一个子目录，错误为“docs 下出现了非 markdown 文件：accounts”等；现改为递归验证分发文件数和文件类型，没有删除断言。Engine 的文档载荷测试也已改读新 MONITOR 路径。

- `GOWORK=off go test ./cmd/papa . ./docs -count=1`：通过，含生成项目的 `go mod tidy` / `go build ./...`。
- `GOWORK=off go test ./engine -run 'Test(BreakerStatusPayloadKeys|.*Payload.*)$' -count=1`：通过。
- `GOWORK=off go vet ./cmd/papa ./docs`：通过。
- 本地移动清单、链接、历史适用版本标记检查及 `git diff --check`：通过。

源码非测试变化仅涉及 embed 分发清单、文档指针及错误提示中的手册路径；不改调度、结算、取消或关停逻辑。本轮未重跑全仓 race、MySQL 或真实站点验收；未修改业务数据、未更新其它业务项目副本、未执行 Git 写操作。已有项目升级后可能保留旧路径副本，按原契约交由项目所有者核对处理，不自动删除。
