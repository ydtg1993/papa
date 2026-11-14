# App ownership and shutdown implementation plan

<!-- version-scope -->
> **适用版本/阶段：Papa v4 演进过程中的历史工作区快照（具体基线以正文为准；未记录精确发布版的条目不推定版本号）。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 本文为设计/计划记录，不能仅凭计划步骤或复选框认定已实现或已验收。 当前使用方式以 [v4 手册导航](../../README.md) 为准。
<!-- /version-scope -->


> Execution: use the approved App review as the design; independent domains follow dispatching-parallel-agents, integration and verification stay in this chat. No Git writes.

**Goal:** Close the confirmed App ownership and shutdown gaps, then consolidate feature assembly without changing public API, routes, page keys, or execution lock ordering.

**Architecture:** Engine owns registered sites; sitedecl owns copied startup declarations. Each service owns admission/cancellation/drain, while App requests all stops and aggregates results before closing shared resources. Admin feature packages own default page descriptors, task presentation and action audit semantics; App owns dependency injection and route protection.

**Tech Stack:** Go, net/http, GORM, embedded JavaScript/CSS, Go behavior tests, Node/Chromium and isolated MySQL where supported.

## 1. Declaration ownership

- [x] Add regression coverage in internal/sitedecl for nested registration snapshots, plan/entry isolation and inherited breaker validation, and App integration for consistent Site snapshots / no partial publication on invalid defaults.
- [x] Clone declaration-owned maps, slices and configuration pointers on ingress/egress. Fetcher is intentionally shared. Overlay clone behavior belongs to its declaring package; Engine and declarations reuse it.
- [x] Remove App.sites; App.Site delegates Engine.Site. Preserve registration validation before any mutation and all public aliases/signatures.
- [x] Run `GOWORK=off go test ./internal/sitedecl ./internal/app ./engine ./pkg/middleware/filedown ./pkg/middleware/m3u8` after integration.

## 2. Component shutdown contract

- [x] Add blocking-provider regressions for runtime failure paths and cancelable error-listener behavior.
- [x] Split component stop initiation from bounded waiting where necessary; existing Shutdown methods remain compatible. All runtime components receive stop before waiting under the same drain deadline.
- [x] App tracks its own message listeners; wait before shared resource close. Initialization failure cleans up created runtime components.
- [x] Add HTTP timeout regression using a real request that stays alive after connection Close. Return a drain result and include it in shared-resource-close decisions.
- [x] Verify component tests and App tests, then run race on all affected packages.

## 3. White-list lifecycle

- [x] Add context-aware Store/cache reads without removing existing public methods. Preserve invalidation correctness and TTL/default read-failure behavior.
- [x] Give admin/access an owned reload component with cancellation and bounded Stop/Wait; do not publish canceled read results.
- [x] App starts it and includes its drain result in the same shared-resource safety decision.
- [x] Test blocked reads, cancellation, no publication after stopping, and SQL context propagation.

## 4. Admin ownership and assembly

- [x] Introduce a neutral page descriptor contract, with each admin feature owning its default metadata and embedded resources; App retains page order and route/auth behavior.
- [x] Keep task table presentation in admin/tasksource while preserving Table's existing public defaults and App's historical displayed defaults through an explicit workspace factory.
- [x] Let breaker/queue/taskflow features construct their action audit events; App only supplies recorder and operator/time adapters. Keep audit timing tied to existing successful/failed action callbacks.
- [x] Bind one handler per feature in an App route assembly file; remove private per-endpoint forwarding shells and script aliases, preserve public RodDiagnostics and App extension API.
- [x] Move purely frontend assertions to owning feature tests; keep App HTTP/browser integration tests and add route/key/resource-order coverage.

## 5. Final verification and documentation

- [x] Add dependency guard for sitedecl and App frontend literals as appropriate without enforcing arbitrary file counts.
- [x] Run gofmt, full test/vet/build with GOWORK=off, affected/full race, available local Chromium and isolated MySQL regressions. Record genuine skipped or unavailable environments.
- [x] Update docs/package_structure/INTERNAL_STRUCTURE.md, DEVELOPMENT.md, ARCHITECTURE.md, README.md and audit status to describe actual final files, owners and tests. Keep the original review as historical evidence.
- [x] Read final diff and independently review failure paths, shared deadlines, snapshot copies, route compatibility and all prior changes. Leave all changes uncommitted.

## Completion evidence

Completed the approved scope, including long-lock drain corrections found during independent review, public TaskFlow resource override compatibility, deterministic MySQL settlement test timing, and bounded Windows diagnostic profile cleanup. Preserved the workspace move to docs/package_structure and verified new/upgrade distribution of nested manuals.

Full ordinary tests: 76 packages, 2817 tests/subtests passed; 6 environment skips. Full race: 76 packages passed; later diagnostic/App changes passed Chrome-enabled affected race (27 tests/subtests), and the final CLI documentation changes passed package ordinary/race tests. Final vet/build, gofmt, diff and documentation checks passed. Detailed evidence and limits: [implementation record](../reviews/2026-11-13-app-ownership-implementation.md). No Git writes.
