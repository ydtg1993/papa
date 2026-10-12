# Papa crawler development skill review

Reviewed `skills/papa-crawler-development/SKILL.md`, every file under its `references/` directory, and `evals/evals.json` against the current `papa/`, `crawler/`, and `oao/` code and manuals. The defects below are concrete; stylistic preferences and already-disclosed drift in the framework manuals are omitted.

## High severity

### 1. Removed downloader capability APIs are taught as required startup contracts

**Skill evidence**

- `SKILL.md:95` says a fetcher using downloader resources must implement `NeedsFiledown` or `NeedsM3U8` so startup validates the wiring.
- `references/review-checklist.md:9` asks reviewers to flag disabled global downloader resources.

**Current authority**

- `papa/engine/deps.go:5-18` defines only `NeedsBrowser` and explicitly says the old `NeedsFiledown` / `NeedsM3U8` checks were removed because both downloader bases are always initialized.
- `papa/engine/stage_test.go:576-589` treats methods with those names as legacy methods that have no dependency-checking effect.
- `papa/engine/downloads.go:8-18,109-126` says downloader configuration is site/stage overlays over an always-prepared base; there is no globally enabled/disabled downloader resource.

**Impact**

Following the skill can add dead methods and, more dangerously, gives a false assurance that startup will validate downloader wiring. The checklist can also generate a false positive about a nonexistent disabled global downloader.

**Required correction**

Require only `NeedsBrowser()` for startup dependency validation. For file/M3U8 downloads, instruct callers to use `FiledownForTask` / `M3U8ForTask` and validate site/stage overlay settings; remove the disabled-global-downloader checklist item.

## Medium severity

### 2. Header inheritance is stated too broadly and contradicts current fetch/download behavior

**Skill evidence**

- `SKILL.md:97` says site-level headers are for all requests and downloads and that `papa.WithHeaders` provides request-specific overrides, without explaining the explicit handoff required for downloads.

**Current authority**

- `papa/docs/FETCHER_WRITING_GUIDE.md:914` states downloaders do not implicitly inherit fetch headers; callers must pass `filedown.OptionsFromRequest(ctx, referer)` or `m3u8.OptionsFromRequest(ctx, referer)`.
- `papa/pkg/middleware/filedown/downloader.go:132-155` states the downloader does not read the context itself; `OptionsFromRequest` explicitly copies site/request headers and proxy into download options.

**Impact**

An implementation following the skill can correctly declare cookies, UA, or referer at site/request scope yet omit them on asset or media downloads, causing authorization or anti-hotlink failures.

**Required correction**

State that site-level and `WithHeaders` values affect fetch context; downloads receive them only when the fetcher explicitly builds download options with the relevant package's `OptionsFromRequest` (or copies them manually).

### 3. The monitor action example reads the row ID from the wrong request field

**Skill evidence**

- `references/implementation-patterns.md:91-105` presents a concrete handler whose first operation is `req.Int("id")`.
- The caveat at `:109` says the stable ID may instead be in `req.Row`, but still does not direct readers to the actual canonical field.

**Current authority**

- `oao/action.go:23-29` defines `ActionRequest.ID` as the row primary key and `Values` as action form fields.
- `oao/action.go:38-49` shows `req.Int(name)` reads only `Values[name]`; it does not read `ID` or `Row`.
- Current Papa handlers parse `req.ID`, for example `papa/admin/tasksource/table.go:236`.

**Impact**

For a normal row action with no form field named `id`, the example always returns `400 invalid id`; adding a hidden editable `id` field to make it work would also improperly trust client form data instead of the route-supplied row identity.

**Required correction**

Parse and validate `req.ID` (for example with `strconv.ParseUint`). Use `req.Row` only for an explicitly declared optimistic-lock/version value when needed, never as the primary ID source.

### 4. Site-header advice conflicts internally with the downloader claim

**Skill evidence**

- `SKILL.md:97` says `papa.WithHeaders` is a request-specific override while simultaneously implying all such header layers apply to downloads.
- The skill never mentions the explicit `OptionsFromRequest` bridge anywhere else.

**Current authority**

- `papa/docs/CORE_CONFIG.md:98-101` and both downloader implementations require an explicit conversion from request context to download options.

**Impact**

This is not merely missing detail: it creates an internally inconsistent mental model in which request-specific headers appear automatically available to a separate downloader. Users cannot reliably infer when headers are propagated.

**Required correction**

Separate the concepts: site/request headers are attached to fetch context; downloader calls must explicitly copy them into download options.

## Low severity

### 5. Trigger description is overbroad for generic Oao work

**Skill evidence**

- `SKILL.md:3` lists “oao tables/actions” as a trigger without limiting that phrase to a Papa business crawler repository.

**Why this is defective**

Oao is a framework-agnostic module in this workspace (`references/project-conventions.md:8` itself says so). A request to change `oao/` component internals, its standalone demo, or a non-Papa consumer matches that phrase but falls outside the skill’s declared purpose (`SKILL.md:11-13`) and its instructions to avoid framework internals.

**Impact**

The skill can trigger on component-development tasks where its business-integration boundaries and Papa monitor assumptions are wrong.

**Required correction**

Qualify Oao triggers as Oao tables/actions mounted by a Papa business crawler monitor, or require the repository to import `papa/v3`. Explicitly exclude standalone `oao/` component implementation work.

### 6. Trigger description can miss Papa business work that does not match its crawler-keyword list

**Skill evidence**

- `SKILL.md:3` makes implicit triggering conditional on the repository importing `papa/v3`, but the concrete trigger list centers on crawler-specific nouns and a few Chinese phrases.

**Why this is defective**

Requests such as “add an Episode admin table and approve action in this repository” or “add a GORM business model and migration” may not mention Papa, crawler, monitor, Oao, or any listed Chinese phrase even though inspection would show a Papa business project. Those are supported workflows in the body (`SKILL.md:115-144`) but are weakly represented in the description.

**Impact**

The skill can under-trigger on its own persistence and business-monitor use cases.

**Required correction**

Add repository-context triggers for business models/repositories, migration registration, admin tables/actions, and custom monitor routes in projects importing `papa/v3`, while preserving the standalone-Oao exclusion above.

## Coverage defect in the skill's own evaluations

### 7. Existing evals cannot detect the dangerous API claims above

**Evidence**

- `evals/evals.json` covers a static-HTML site, drift review, and a monitor action.
- None requires file/M3U8 downloading, header propagation into downloader options, or distinguishing the removed downloader capability methods from `NeedsBrowser`.
- The monitor expectation checks distrust of `Row` but does not assert use of `ActionRequest.ID`, so the broken `req.Int("id")` reference example can pass the eval's intended criterion.

**Impact**

The current test set can report the skill as successful while it teaches dead downloader APIs and a nonworking action handler.

**Required correction**

Add one downloader scenario asserting no `NeedsFiledown`/`NeedsM3U8` dependency contract and explicit `OptionsFromRequest`; strengthen the monitor assertion to require parsing `req.ID` and to reject `req.Int("id")` unless `id` is an actual declared form field.
