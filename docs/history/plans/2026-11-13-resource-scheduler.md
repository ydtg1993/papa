# Resource Scheduler Implementation Plan

<!-- version-scope -->
> **适用版本/阶段：Papa v4 演进过程中的历史工作区快照（具体基线以正文为准；未记录精确发布版的条目不推定版本号）。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 本文为设计/计划记录，不能仅凭计划步骤或复选框认定已实现或已验收。 当前使用方式以 [v4 手册导航](../../README.md) 为准。
<!-- /version-scope -->


> **For agentic workers:** Execute sequentially within the assigned engine/taskstore ownership. No Git writes.

**Goal:** Bound scheduler residency and refill durable pending work continuously with cancellation-aware fair batches.

**Architecture:** A scheduler-owned admission ledger counts pool, running, delay, spill and detached tasks. Engine coordinates durable admission and transfer; taskstore owns eligible pending queries. Existing locks never surround SQL or capacity waits.

**Tech Stack:** Go, GORM/MySQL, existing scheduler and workerpool.

- [x] Replace whole spill detach with indexed round-robin batches; restore untouched entries on cancellation and stop a saturated bucket early.
- [x] Add cancellation-aware taskstore pending/due queries and continuous per-stage cursor refill after stage registration, independent of RecoverQueue.
- [x] Add additive limits API and component-owned resident ownership; durable overflow remains discoverable and non-durable admission returns a recognizable error.
- [x] Limit near-term delay residency and deduplicate queued identities without removing different rounds.
- [x] Preserve transient metadata and breaker deadlines through every transfer; verify stale-round CAS and cancellation publication boundaries.
- [x] Run targeted scheduler/engine regressions, isolated MySQL integration and race checks; record actual outcomes and limitations.

Evidence: `.review-tmp/resource-implementation/scheduler-summary.md`; final whole-repository validation is coordinated by the parent task.
