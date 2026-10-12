# papa-crawler-development benchmark

## Final quantitative result

| Eval | Without skill | Final with skill | Delta |
| --- | ---: | ---: | ---: |
| New site integration | 4/6 (66.67%) | 6/6 (100%) | +33.33 pp |
| Documentation drift audit | 5/6 (83.33%) | 6/6 (100%) | +16.67 pp |
| Monitor table/action | 6/6 (100%) | 6/6 (100%) | parity |
| **Total** | **15/18 (83.33%)** | **18/18 (100%)** | **+16.67 pp** |

## Interpretation

- The new-site improvement came from making global `repeat_queue` enablement and representative HTML/fixture parser tests explicit.
- The audit improvement came from inspecting adjacent lifecycle/repository comments and catching the stale claim that `Task.Meta` is memory-only.
- The monitor case was already strong without the skill. It validates parity, not uplift.
- A post-eval source review found and corrected three additional defects before packaging: removed downloader capability contracts, missing explicit downloader header propagation, and use of action form values instead of `ActionRequest.ID`.

## Limitations

- One run per configuration means standard deviation and flakiness cannot be estimated.
- Harness notifications did not expose recoverable duration/token values, so timing efficiency is reported as unavailable rather than fabricated.
- The evals produce implementation/audit plans rather than modifying the crawler; compilation assertions therefore require a future file-producing integration eval.

See `benchmark.json` for machine-readable run records and grading source paths.
