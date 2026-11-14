# Report resource budget implementation plan

<!-- version-scope -->
> **适用版本/阶段：Papa v4 演进过程中的历史工作区快照（具体基线以正文为准；未记录精确发布版的条目不推定版本号）。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 其中基于文件/数据库的报告预算属于当时方案；当前在线信号预算与显式离线扫描边界以 [观测手册](../../operations/OBSERVABILITY.md) 为准。 本文为设计/计划记录，不能仅凭计划步骤或复选框认定已实现或已验收。 当前使用方式以 [v4 手册导航](../../README.md) 为准。
<!-- /version-scope -->


**Goal:** Implement R4/R5 with bounded, cancellable inputs and one-day-at-a-time report consumption.

**Architecture:** `obs/inputbudget` owns stream accounting and typed errors, eventlog/logidx own decoding, filtering and exact metadata, obs owns bounded spool lifetime and daily assembly. Runner owns serial generation and cancel/drain; CLI supplies deadline. Existing public struct layouts and entry points stay compatible.

**Tech Stack:** Go context, buffered readers, gzip, bounded temporary JSONL files.

- [x] Add a shared bounded line reader, file/byte/line/event/dedup/spool limits and exact-boundary tests. Run `go test ./obs/inputbudget`.
- [x] Add context-aware event and log reading APIs and bounded directory discovery, preserving malformed/duplicate/gap semantics. Run `go test ./obs/eventlog ./obs/logidx`.
- [x] Add context source loading and daily consumption with bounded staging: scan events once, preserve global bad-line and per-day dedup semantics, release daily slices, remove spool on success/error/cancel. Validate all source budgets before consumers publish outputs.
- [x] Wire runner deadline/lifecycle cancellation and CLI timeout to additive APIs; keep Stop waiting for generation and consumers.
- [x] Add daily equivalence, cancellation, gzip expansion, over-budget/preserved-output regressions. Run `go test ./obs/... ./internal/reportrunner ./cmd/papa`, race and vet for owned packages. No Git writes per workspace rule.

Defaults are explicit conservative local safeguards, not a measured production sizing claim. Legacy APIs use defaults; callers requiring larger complete input must opt in through additive limits. No approximate dedup or silently successful truncation is introduced.
