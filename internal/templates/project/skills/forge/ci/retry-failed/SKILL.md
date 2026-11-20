---
name: ci/retry-failed
description: Retry only exact unchanged Go test failures once in the merge tier with `forge ci retry-failed`, and verify the recorded evidence without executing candidate code with `forge ci verify-test-retry`.
---

# Retry an ordinary merge-tier Go failure once

Opt in from the actual required check, not a replacement check or an external
retry of an entire Actions job. Run `forge ci retry-failed` with a verified
Forge binary and the same clean tested checkout under a credential-free test
step. Pass the exact package list, not `./...`, and a per-gate output directory
outside the checkout:

```bash
forge ci retry-failed --repo "$REPOSITORY" --base "$BASE_SHA" --head "$TESTED_SHA" \
  --run "$RUN_ID" --pr "$PR_NUMBER" --timeout 120s \
  --json "$EVIDENCE_DIR/initial.ndjson" --report "$EVIDENCE_DIR/retry.json" \
  -- ./internal/example
```

The first invocation uses ordinary `go test -short -json`. At most five
unambiguous top-level tests can qualify, each with unchanged source on the
gate base. Each runs once again with `-count=1` and an anchored test name. A
race-enabled run, repeated-test lane, panic, build/host failure, unclassified
subtest or incomplete result is never a retry pass. Other steps still gate.
Do not use this in the changed-test repetition or release lanes.

A recovered test prints `retried:TestName (module/package)`. The original
stream is kept, and the successful report `merge-test-retry/v1` binds the
original and numbered retry streams (`retry.json.retry-0.ndjson`, etc.) to the
checked commit and tree. Preserve them together. A trusted cleanup workflow
can invoke `forge ci verify-test-retry` with independently supplied job
metadata (`--repo`, `--base`, `--head`, `--tree`, `--run`, `--pr`, `--module`)
and the same artifact paths. It verifies the test identities, digests, and
tracked source without running candidate code. Deletion PRs and issue filing
are repository policy; neither command makes GitHub writes. Treat test output
as untrusted and scrub it before publishing it on an issue.
