# Adaptive runtime implementation plan

<!-- version-scope -->
> **适用版本/阶段：Papa v4 演进过程中的历史工作区快照（具体基线以正文为准；未记录精确发布版的条目不推定版本号）。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 本文为设计/计划记录，不能仅凭计划步骤或复选框认定已实现或已验收。 当前使用方式以 [v4 手册导航](../../README.md) 为准。
<!-- /version-scope -->


Approved design: ../specs/2026-11-10-adaptive-runtime-design.md. Worktree files only; no Git writes. Subsystems execute in independent file ownership domains, integration follows their verified APIs.

**Goal:** persistent runtime policies, sticky site account pools with asynchronous failover, adaptive request/resource admission, proxy health and OA controls.

**Architecture:** immutable validated policy snapshots; independent account and proxy state machines; transport-level admission and engine task admission; authenticated OA APIs backed by versioned stores.

**Tech Stack:** Go, existing MySQL/GORM, Rod, embedded Papa.page JavaScript.

## Ownership and ordered acceptance

- [x] Account subsystem: pkg/account pool selection, management state store and tests. Same site+pool retains selected account; duplicate failure signals for a generation switch once; no failed request is replayed under another identity. Existing explicit account APIs remain compatible.
- [x] Governance subsystem: pkg/governance policy validation, cancellable request and resource admission, runtime snapshots, transport instrumentation and tests. Disabled defaults retain legacy behavior. Per-site rate, inflight, Retry-After, smoothing, resources and fairness tested with fixtures.
- [x] Proxy subsystem: pkg/middleware/proxy health reporting, cooldown and bounded probes with tests. Fixed proxies never silently change and configured proxy exhaustion never becomes direct access.
- [x] Versioned config: models.Config.Revision; admin/configstore LoadVersioned/SaveCAS; revision 0 creates exclusively, updates are conditional, reset preserves revision. Existing keys retain Save behavior; policy key rejects blind Save. SQL and concurrent first-write regressions.
- [x] Application integration: config structures/default validation, app initialization/lifecycle, Engine task/HTTP/browser admission, accounts and proxy feedback. Queue waits cancellable; no network waits while holding DB/engine locks. Account expiry ends attempt and skips immediate stage retry, existing later compensation remains available.
- [x] OA: authenticated runtime policy/status and account pool endpoints; data/account pools and monitor/crawl control pages. Stale response and revision handling, explicit save, no credentials in snapshots, no authentication caused by GET.
- [x] Documentation/templates: configuration dictionary, account API, monitor, developer structure, migration and scaffold examples.
- [x] Integration verification: gofmt, targeted tests and race, go test ./..., go vet ./..., go build ./..., scaffold tests and local synthetic HTTP/browser scenarios. Isolated real-MySQL migration/CAS checks passed and the temporary database was removed. No production crawling.

## Working commands

```powershell
$env:GOWORK='off'
go test ./pkg/account ./pkg/governance ./pkg/middleware/proxy ./admin/configstore
go test ./engine ./internal/app ./admin/server ./cmd/papa
$env:CGO_ENABLED='1'
go test -race ./pkg/account ./pkg/governance ./pkg/middleware/proxy
go test ./...
go vet ./...
go build ./...
git diff --check
```

Failures must be resolved or identified with actual evidence. A passing unit suite does not establish live database migration, resource availability on every platform or external site correctness.

## Completion

Completed in the worktree on 2026-11-10; no Git writes. See [verification record](../reviews/2026-11-10-adaptive-runtime-verification.md) for commands, regression cases and verified boundaries.
