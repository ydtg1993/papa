# papa-crawler-development validation

**Result: PASS — installable.** No skill source was modified.

## Checks

- **SKILL.md frontmatter: PASS.** The document has a bounded YAML frontmatter block with the required `name` and `description`; `name: papa-crawler-development` matches the directory and uses the permitted lowercase alphanumeric/hyphen form. `compatibility` is a supported optional scalar. The frontmatter is well below the 1024-character constraint (about 780 characters including keys/values/newlines).
- **Local Markdown references: PASS.** All four local links found in `SKILL.md` resolve inside the skill: `references/doc-map.md`, `references/project-conventions.md` (linked twice), and `references/review-checklist.md`. No missing local Markdown target was found.
- **`evals/evals.json`: PASS.** The file parses as JSON and matches the skill-creator eval schema: top-level `skill_name` matches frontmatter; `evals` is a nonempty array; all four records have unique integer `id`, nonempty `prompt` and `expected_output`, array `files`, and nonempty string-array `expectations`.
- **Downloader capability claim: PASS against current code.** `papa/engine/deps.go` retains only `NeedsBrowser`; comments and stage tests confirm `NeedsFiledown`/`NeedsM3U8` startup contracts were removed because `InitDownloaders` always creates both base downloaders. `papa/engine/downloads.go` exports the exact task-aware APIs `FiledownForTask` (lowercase `d` after `File`) and `M3U8ForTask`, resolving stage/site overlays before falling back to the base instances.
- **Header/proxy propagation claim: PASS, with the documented scope.** `filedown.OptionsFromRequest` and `m3u8.OptionsFromRequest` explicitly copy context headers and proxy and take a referer; both state downloaders do not read the fetch context implicitly. They propagate site `SiteSpec.Headers` plus request `papa.WithHeaders` and `papa.WithProxyURL`; they do **not** copy global `html.headers`/`browser.headers`. Cookies and User-Agent propagate when represented in those context headers. The skill accurately requires explicit propagation.
- **`ActionRequest.ID` claim: PASS against current code.** `oao.ActionRequest.ID` is the target row primary key passed from the action body; `ActionRequest.Int(name)` reads `Values[name]`, not the row ID. `Table.IDField` determines which row field the frontend sends as `ActionRequest.ID`. Parsing `req.ID` and treating `req.Row` as untrusted snapshot data is therefore correct.

## Minor wording note (non-blocking)

Eval 4 expectation 52 says the legacy downloader contracts are “removed/ineffective.” Current code is more precise: the interfaces/checks are **removed**; a legacy fetcher method with that name may still compile as an unrelated extra method but has no framework effect. This does not prevent installation or invalidate the skill’s operational guidance.
