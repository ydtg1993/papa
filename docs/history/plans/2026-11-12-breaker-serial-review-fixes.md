# 串行恢复验证与历史修复实施计划

<!-- version-scope -->
> **适用版本/阶段：Papa v4 演进过程中的历史工作区快照（具体基线以正文为准；未记录精确发布版的条目不推定版本号）。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 本文为设计/计划记录，不能仅凭计划步骤或复选框认定已实现或已验收。 当前使用方式以 [v4 手册导航](../../README.md) 为准。
<!-- /version-scope -->


**Goal:** 同站点一次一个验证样本，选中后最多等待和执行 10 分钟；超时不算健康，未退出不能开始下一个；修复升级门槛和日报截断误报。

**Architecture:** 在现有 Recovery 中保存当前样本身份、截止时间和阶段超时身份，在 smartBreaker 中保留短暂的重投占用标记。所有任务入口通过原 admission gate 仲裁，沿用任务 CAS/认领及原报告写入循环，不新增队列、服务或管理器。整体恢复预算仍有效。

**Tech Stack:** Go、GORM、MySQL、现有 breaker/obs。

用户已批准本方案并要求直接施工。本工作区不执行任何 Git 写操作；在当前会话按步骤实现、自查。

- [x] `internal/breaker/recovery.go`、`engine/breaker_samples.go`：持久化当前样本与截止时间；选择、重投、正常任务认领互斥；超时记录并跳过；未退出仍占串行位置。
- [x] `engine/breaker_runtime.go`、`breaker_recovery.go`、`stage.go`：全站验证并发固定 1；真实任务传递截止 context；过期结果不参与健康证明；小时重试重新授予证明；重启不继承旧成绩或重新延长样本等待。
- [x] `internal/breakerstore/snapshot.go`：读取旧混合事件时迁移验证模式；新控制字段不从报告反向维护。旧格式通过兼容入口保守升级。
- [x] `obs/analyze/breaker_reports.go`：展示截断时只报告状态未知，不再把前缀当截止状态；历史追加事务保持不变。
- [x] 将上一轮 `.review-tmp` 复现转为正式回归，并增加选中后重启、延迟/运行超时、迟到结果、双入口并发、所有阶段验证及未退出阻塞小时复查测试。
- [x] 更新 BREAKER、MONITOR、FETCHER_WRITING_GUIDE 及验收说明。执行相关测试、race（隔离 MySQL）、全量 test/vet/build、diff 检查；检查报告与控制状态无交叉驱动。

验证命令：
```powershell
go test ./engine ./internal/breaker ./internal/breakerstore ./obs/analyze ./obs/incident -run 'Breaker|Detector|Checkpoint' -count=1
$env:CGO_ENABLED='1'
$env:PAPA_RUNTIME_TEST_CONFIG='E:/go-project/test_papa/grab/configs/config.yaml'
go test -race ./engine ./internal/breaker ./internal/breakerstore ./obs/analyze -run 'Breaker|Detector|Checkpoint' -count=1
go test ./...
go vet ./...
go build ./...
git diff --check
```
