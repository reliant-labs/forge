## V4: `forge env status` is the ONE read view

Implements task **V4** of `docs/adr/env-verbs.md` (merged as #378). Owner-approved.

Reading an environment took six verbs — `env status`, `env verify`, `env wait`,
`env rollout`, `env topology`, `env history` — and they were views of a single
question, split by which half of the answer each happened to own. A reader had
to know, *before they could ask*, that the bound release lived in `verify`, the
rollout phase in `rollout`, the runtime ports in `status`, and the promotion
that caused all of it in `history`. Two were literal duplicates: `env rollout`
**was** `env wait --timeout 0`.

They are modes of one command now:

```bash
forge env status                  # every environment, and how far behind each is
forge env status prod             # prod right now: runtime AND release
forge env status prod --wait      # block until the rollout settles
forge env status prod --history   # prod's promotion ledger, newest first
```

One env's report carries the bound release, verify (running digests vs the
binding, in all five states — through the control plane's observer for a hosted
env, since forge cannot read a hosted cluster), the rollout phase per workload,
runtime health, gates, and ledger freshness.

### Every exit code is unchanged, structurally

Each mode reaches **the same run function the deleted verb called** —
`runEnvWait`, `runEnvHistory`, `runEnvTopology`, and verify's run function
(renamed `runEnvStatusRelease`). Nothing was reimplemented in the move, which
is what makes "exit codes unchanged" a property of the wiring rather than a
promise: 0/1/2 for the read, plus 5 (timed out, still progressing) and 6
(superseded) under `--wait`.

**The two halves of the default view are deliberately asymmetric about
failure.** Runtime health *reports* — "the app is down" is a state this command
must be able to print, and a status verb that exited non-zero for it would be
unusable as the one read view. The release half makes a *claim* (the env is or
is not running what it declares), so its verdict is the exit code.

### Two design points worth reviewing

**`--timeout` serves two modes with different budgets** — 60s for a cluster
read, 15m for a wait. One flag cannot declare two defaults, so its declared
default is an unset sentinel that each mode resolves (`resolveWaitBudget`). The
flag's help names both numbers so the `0s` shown in `--help` is not read as "no
timeout". `--wait --timeout 0` still means one read, never blocking — the
distinction is only visible where cobra's `Changed` is, and getting it wrong in
either direction is bad in its own way.

**The release fields stay FLAT in `--json`, not nested.** `forge gate record
--from` recognises a deploy-verification document by top-level `bound` +
`images` (`gate_doc.go`'s "verify" recipe). Moving them under a `release` key
would make every recorded gate fall through to the generic ok/exit_code
reading, silently losing the unbound-env-is-SKIPPED distinction — a green check
in the evidence trail attesting to images nobody checked. The *runtime* half is
nested under `runtime`, precisely so `gate record` cannot recognise a document
by anything in it.

### Three bugs the gates caught

Each is now pinned by a test verified to fail before the fix and pass after:

1. **`--json` emitted TWO documents** (runtime, then release) — a stream no
   `jq` invocation can read, and the failure reads as malformed JSON rather
   than as two halves sharing an output. The runtime half is now *collected*
   and nested. `TestEnvStatusJSON_EmitsExactlyOneDocument` decodes the whole
   stream, because `Unmarshal` on a concatenation stops happily after the first
   value and would not catch it.
2. **`--history --release v1` returned every promotion.** The shared
   `--release` flag was bound only to the wait options, so the history query's
   filter silently no-opped — the worst kind of wrong answer, because it is
   plausible. Found by `deadcodeguard` (phantom field: written only by tests).
3. **An `env topology` ARGV invocation survived the text sweep** in the hosted
   e2e test — `runForge(t, "env", "topology", ...)`, where the tokens are never
   adjacent in source — and would have failed at runtime. Caught by the
   removalguard's ARGV pattern.

### Deleted, with a removalguard entry

`forge env verify|wait|rollout|topology|history` are gone — no alias, no hidden
name (pre-1.0). One removal entry pins all five spellings across every surface
forge ships, with narrow allowances for the CHANGELOG's record of the releases
that *did* ship them, the ADR that decided the merge, and the test that proves
the removal is complete.

`forge release where` and `forge release verify` are untouched: both answer a
question about a RELEASE rather than an environment.

**F3/F7's internal helpers are deliberately unchanged** — `runEnvWait`,
`runEnvWaitForCmd`, `waitForRollout`, `envWaitOptions` (incl. `Once`),
`waitTarget`, `resolveDeclaredWaitTarget`, `readRollout`,
`readDeclaredRollout`, `buildTopologyEnvRow`. Only the command surface moved,
so V3's `env deploy` keeps calling the wait helper it depends on.

### Tests

The deleted commands' tests were merged into the status command's, preserving
every behaviour and exit code they pinned. `status_verbs_test.go` was split
into `env_history_test.go` (the ledger reader, which both backends implement
identically) and `release_where_test.go` (not env-scoped, so it survived the
merge) — the command-layer cases moved into `env_status_cmd_test.go`.

### Gates

- `go test -short ./...` — exit 0
- `go test -count=1 ./internal/cli/` (full, 179s) — exit 0
- `forge lint --no-fix` — exit 0

`funlen` flagged the 247-line command constructor; rather than suppress it with
a `//nolint`, the mode dispatch was extracted (`dispatchEnvStatus`,
`resolveWaitBudget`), which also removed a duplicated topology branch and makes
the mode *choice* one readable sequence.
