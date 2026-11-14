# Browser Identity Implementation Plan

<!-- version-scope -->
> **适用版本/阶段：Papa v4 演进过程中的历史工作区快照（具体基线以正文为准；未记录精确发布版的条目不推定版本号）。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 本文为设计/计划记录，不能仅凭计划步骤或复选框认定已实现或已验收。 当前使用方式以 [v4 手册导航](../../README.md) 为准。
<!-- /version-scope -->


> **For agentic workers:** Use executing-plans task by task; dispatching-parallel-agents applies to the separate browser, binding and challenge domains below. All changes stay in this worktree. No Git writes or production crawling.

**Goal:** Implement the approved browser identity design, including stable bindings, safe activation, challenge classification, OA management and local diagnostics.

**Architecture:** `pkg/browser/identity` owns immutable profile declarations, CDP application and observations without GORM. A separate identity runtime/store owns append-only profile revisions, CAS subject bindings and in-process use references. Account and Engine scopes pin identity independently of authentication versions. Existing browser creation and diagnostics consume these contracts; transport-neutral challenge errors reuse the current retry contract.

**Tech Stack:** Go 1.25, Rod v0.116.2, existing GORM/MySQL, embedded OA JavaScript, local synthetic HTTP/Chromium fixtures.

Approved spec: [browser-identity-design](../designs/2026-11-10-browser-identity-design.md).

## File ownership and contracts

- Browser domain: `pkg/browser/identity/*.go`, `pkg/browser/identity.go`, `pkg/browser/rod.go`, `pkg/browser/navigation.go`, browser identity tests. Defines Profile/Reference/ResolvedProfile/Observation and native-compatible CDP behavior. Installs protected-page identity tracking and applies profile before navigation; challenge hook changes are coordinated here.
- Binding/account domain: separate `pkg/browser/identity/runtime/` package, identity models/store, `pkg/account/identity*.go` and necessary account changes, `engine/identity.go` and `engine/governance.go`. Exposes profile store, manager snapshots, CAS staging/cancel/reverify and bounded identity scopes; no code in browser-domain files. Owns migration registration and account/engine regressions.
- Challenge domain: `core/challenge.go`, `pkg/htmlfetch/challenge.go`, HTTP response path, `pkg/browser/challenge.go`, capture/response/state/record challenge hooks and tests. Does not edit account files or browser creation/navigation files; sends exact integration requirements to their owner.
- Root: application initialization/lifecycle, authenticated OA APIs, diagnosis and UI, config/public aliases, docs/templates, schema count tests, final integration and verification.

Profile references identify site + profile ID + immutable version. Subjects identify site + kind (`account`/`session`) + explicit key. Empty session keys are errors; htmlfetch.Session stays an ID-free CookieJar. All exposed maps/slices are copied, writes are CAS, reads do not authenticate or launch a browser.

## Task 1: Profile declaration and browser application

- [x] Add typed Profile, Reference, ResolvedProfile and Observation; validate compatible Chromium/native-platform UA policy, locale/timezone, finite bounded dimensions/DPR, touch and supported adapter versions.
- [x] Add behavior regressions before implementation, including this invariant:

```go
// A managed identity rejects conflicting legacy device/header overrides.
err := ValidateOptions(profile, headersWithConflictingUA, legacyDevice)
if err == nil { t.Fatal("conflicting identity accepted") }
```

- [x] Derive network UA and Client Hints from one native runtime snapshot; apply UA override, locale, timezone, metrics and touch before navigation. Add a bounded optional adapter that declares its changed fields and does not overwrite protected identity/observation fields.
- [x] Connect the shared page factory and ConfigurePage; preserve unrelated auth headers, cookies, egress and governance. Refuse identity change on an existing managed page. Register/remove observation lifecycle without leaking page references.
- [x] Verify the first inline script and first server request, two isolated accounts, navigation/reload, state restore coexistence, adapter execution, conflicts and runtime mismatch using local Chromium fixtures.

Run: `$env:GOWORK='off'; go test ./pkg/browser/identity ./pkg/browser -run 'TestIdentity' -count=1`.

## Task 2: Durable profiles and binding state machine

- [x] Add the two approved framework models and a GORM store with no implicit DDL. Profile versions append; stale profile expected versions conflict. Bindings maintain active/pending references and an independent revision.
- [x] Add memory-store and real-SQL shape tests for first insert, update conflicts, missing/corrupt records, immutable versions and reference protection. Concurrent writes must satisfy:

```go
if successes != 1 || conflicts != writers-1 { t.Fatal("CAS admitted multiple writers") }
```

- [x] Implement cancellable use scopes that freeze identity; staged binding changes stop new use, drain existing references, verify under a bounded context, persist and publish. Failures remain pending and observable; cancel restores intake. No waits under engine locks or DB transactions.
- [x] Make restart recovery inspect active/pending and credential identity metadata. A mismatched state is never silently installed under another profile. Explicitly disabled/unbound identity retains legacy behavior.
- [x] Connect account Manager.Do/WithBrowser, pool operations and authentication callbacks to the same account subject. Pin a scope for the entire Engine execution before claim/resource admission; all exit/retry/panic paths release it. Do not change ManagementVersion for profile writes.
- [x] Expose a bounded anonymous scope using an explicit business session key; a direct Profile operation does not create a binding. Reconstructing CookieJar does not alter binding identity.

Run: `$env:GOWORK='off'; go test ./pkg/browser/identity/runtime ./pkg/account ./engine -run 'TestIdentity' -count=1`.

## Task 3: Independent site challenge errors

- [x] Add core typed sentinel/classification with Kind `site_challenge` and Retryable=false; preserve errors.Is through wrappers.
- [x] Classify explicit `cf-mitigated: challenge` before account status classifiers and successful HTTP result processing; reuse existing heuristic restricted-page tools without turning text guesses into automatic account failures.
- [x] Observe primary browser document and selected response before navigation, preserve challenge evidence on load/wait failure, ignore unrelated third-party challenges. Keep CaptureRequest early-return semantics.
- [x] Assert site challenges leave account selection/management/credentials unchanged, execute once within a task, and do not poison proxy health. Test ordinary 401/403, account Challenge, 429/503 and failed proxies separately.

```go
if core.Retryable(err) || !errors.Is(err, core.ErrSiteChallenge) { t.Fatal("wrong challenge contract") }
if before.SelectionVersion != after.SelectionVersion { t.Fatal("site challenge rotated account") }
```

Run: `$env:GOWORK='off'; go test ./core ./pkg/htmlfetch ./pkg/browser ./pkg/account ./engine -run 'Test.*Challenge' -count=1`.

## Task 4: App lifecycle, OA and local diagnosis

- [x] Initialize the identity store/runtime without implicit migration, wire verification to account/anonymous paths, and stop its workers before closing DB. Reuse bounded startup errors and existing authentication/audit.
- [x] Add profile list/revision creation and binding list/stage/cancel/reverify APIs. Strict bounded JSON, independent revisions, source-specific validation, uncertain write reconciliation; GET is read-only.
- [x] Extend browser diagnostics with a separate identity mode using the same managed page factory, selected account pool's reproducible binary/settings, isolated temporary profile and a loopback-only synthetic server. Never import account credentials or use target-site probes.
- [x] Compare requested/resolved/first-script/first-request values, report unsupported observations and runtime changes honestly. Whitelist echo headers, bound output, clean resources on every path and retain existing diagnosis serialization/timeouts.
- [x] Extend the current browser diagnosis page with profile revisions and binding controls; link member identity from account pools. Preserve drafts, reject stale saves, support keyboard/narrow layout and distinguish staged/active/failed verification. No arbitrary JavaScript editor.
- [x] Add API and real-browser UI regressions covering CAS, failed verification, no GET side effects, credential redaction and stale response handling.

Run: `$env:GOWORK='off'; go test ./internal/app ./internal/database ./admin/frameworkdata -count=1` plus opt-in local Chromium UI cases.

## Task 5: Documentation and final acceptance

- [x] Sync ACCOUNT_RUNTIME, CORE_CONFIG, MONITOR, RUNTIME_CONTROL, FETCHER_WRITING_GUIDE, DATA_LANDING_GUIDE, DEVELOPMENT and scaffold defaults. Explain session_key ownership, migration, activation, HTTP limitations and browser target coverage.
- [x] Run gofmt, targeted tests and race for modified concurrency paths; run `go test ./...`, `go test -race ./...`, `go vet ./...`, `go build ./...` with GOWORK off (CGO_ENABLED=1 for race).
- [x] Run actual scaffold generation/tidy/build and isolated MySQL migration/CAS tests using the existing owned-test-database helper; never migrate business tables. Verify temporary DB cleanup.
- [x] Review exact modified integration paths, documentation links and `git diff --check`; preserve the user's other files. Record tested boundaries and observed failures/fixes in a verification document; update spec/plan status only when acceptance is complete.

No implementation step includes a Git write. User already authorized execution of the reviewed design; no extra planning/branch approval is needed.

## Completion

Implemented and verified on 2026-11-10. See [verification record](../reviews/2026-11-10-browser-identity-verification.md) for final tests, diagnostic startup limitations and explicit business migration requirements. All changes remain in the working tree; no Git writes.
