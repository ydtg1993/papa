# Download resource budgets implementation plan

<!-- version-scope -->
> **适用版本/阶段：Papa v4 演进过程中的历史工作区快照（具体基线以正文为准；未记录精确发布版的条目不推定版本号）。**
> 审查结论、路径、接口和测试结果仅适用于当次代码与环境；旧兼容约定不延续为当前契约。 本文为设计/计划记录，不能仅凭计划步骤或复选框认定已实现或已验收。 当前使用方式以 [v4 手册导航](../../README.md) 为准。
<!-- /version-scope -->


**Goal:** Bound downloader inputs, active memory and temporary disk; preserve old resume and atomic output publication.

**Architecture:** `pkg/middleware/resources.go` owns immutable limits and shared admission counters. Downloader-specific resource helpers own format validation and response roles. `pkg/middleware/checkpoint.go` owns one coalescing writer per active download; workers only signal after updating state, and completion drains it before cleanup. No App/Engine dependencies or public config layout changes.

**Tech stack:** Go standard library, existing HTTP and output locks.

- [x] Add finite limits and recognizable resource error; immutable `WithResources` clones preserve shared budget through `With`.
- [x] Bound old JSON bytes before decode and counts before building indexes. Save compact versioned snapshots, retaining backward JSON imports.
- [x] Move checkpoint serialization/I/O outside resume locks; coalesce signals and drain exactly once before publication/cleanup.
- [x] Bound playlist/media/key/init reads, reject oversized declarations, stream plain media with rate limiting during reads.
- [x] Bound file chunk counts and direct response bytes; preserve old output on every rejection.
- [x] Validate synthetic exact/plus-one inputs, shared admission, old resume and checkpoint cancellation using package tests, race and vet.

No Git writes. Retained resume data is not garbage-collected by age. Shared disk admission scans retained data and reserves active worst-case output before writing; external writers require filesystem quotas for a hard physical-volume guarantee.

