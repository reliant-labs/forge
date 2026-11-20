## review:changes

Lane: **MEDIUM** (one pass on gpt-6-luna@codex).

MEDIUM: agent-instructions (internal/templates/project/skills/forge/ci/SKILL.md, internal/templates/project/skills/forge/ci/retry-failed/SKILL.md); code (internal/cli/ci.go, internal/cli/ci_retry_failed.go, …)

R1.B1 is fixed: source lookup now lists the package tree directly, including nested package test files. A new blocker remains: verification accepts retry evidence without proving the retry ran in the same execution context as the original gate.

- **blocker** [B1] `internal/testretry/run.go:181` — Retry invocations inherit the complete process environment but do not receive the initial invocation's Go test flags as arguments. In particular, `-run` may exclude tests required by `TestMain` or test initialization, so a retry can pass in an execution context different from the original failing run. The report then certifies that failure as recovered even though the failure may be caused by the omitted test context. New in this round: The retry invocation and evidence-validation path were added after the previous review head; this issue was not present to inspect in round 1. Fix: Preserve the ordinary invocation's effective test selection and relevant test flags when retrying, while adding an exact target selection; reject combinations that cannot be safely composed.

**Earlier blockers, at this head:**
- R1.B1 fixed — internal/testretry/source.go:67-72@8979d54b5223

Fix the blockers and push: the push is reviewed again. (pr-fix takes over if nobody does.)

Reviewed `8979d54b5223bd50639e6ae88f4495406db68bd7`.
<!-- review-changes sha=8979d54b5223bd50639e6ae88f4495406db68bd7 -->
<!-- review-round sha=8979d54b5223bd50639e6ae88f4495406db68bd7 kind=code -->
<!-- review-blockers sha=8979d54b5223bd50639e6ae88f4495406db68bd7 [{"gap":"defect","kind":"code","line":181,"path":"internal/testretry/run.go","problem":"Retry invocations inherit the complete process environment but do not receive the initial invocation's Go test flags as arguments. In particular, `-run` may exclude tests required by `TestMain` or test initialization, so a retry can pass in an execution context different from the original failing run. The report then certifies that failure as recovered even though the failure may be caused by the omitted test context. New in this round: The retry invocation and evidence-validation path were added after the previous review head; this issue was not present to inspect in round 1."}] -->