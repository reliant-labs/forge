# V3: `forge env deploy <env> [vX | --from <src-env>]` absorbs `forge env promote`

Implements task **V3** of `docs/adr/env-verbs.md` (merged as #378). `forge env
promote` is **deleted** — pre-1.0, no alias, no hidden name — and a removalguard
entry pins the removal across every surface forge ships.

```bash
forge env deploy prod v1.4.0          # was: env promote v1.4.0 --to prod && env deploy prod
forge env deploy prod --from staging  # exactly what staging runs
forge env deploy prod                 # unchanged: re-apply prod's current binding
```

## The verb means record + apply + wait, and the wait is not a flag

Recording a binding ships nothing. That is right for a primitive and wrong for
the thing a pipeline actually asks for: a step that only promoted reported
success before any byte had moved, and the release's real failure surfaced
minutes later with nothing connecting the two. So every pipeline spelled it
`promote --deploy --wait`, and the spellings that omitted either half were bugs
waiting for an incident.

The health gate is therefore **on by default**. `--no-wait` opts out and names
`forge env status <env> --wait` as the way to gate later. `promote --wait` and
`promote --deploy` are removed rather than renamed, because both are now simply
what the command does.

## Who applies it is read off the ledger, not a flag

This is the hosted/self-managed parity the ADR asks for, and it comes from the
_same declarative fact_ as where the promotion is recorded — so the two cannot
disagree:

| Env                                            | Applied by                             | Health gate                                                                                |
| ---------------------------------------------- | -------------------------------------- | ------------------------------------------------------------------------------------------ |
| hosted (KCL declares `forge.ControlPlane`)     | the control-plane converger            | forge waits on the server-computed rollout, **pinned to the promotion this command wrote** |
| self-managed (`.forge/promotions/<env>.jsonl`) | this command, client-side render+apply | that apply's own per-resource rollout wait                                                 |

The pinning is the point of the hosted wait. If it re-read the env's _current_
promotion, a hotfix landing in the seconds after this deploy would silently
become the thing being waited on, and the pipeline would report its own release
healthy on the strength of somebody else's. An overtaking promote is reported
as SUPERSEDED (exit 6) instead.

The two branches are exclusive on purpose: a self-managed env has no
server-computed rollout, so polling one could only time out and would blame the
release for a converger that was never going to run; a hosted env is converged
server-side, so a client-side apply would be forge racing the converger to
write the same specs.

## Unchanged

Everything moved with the code: the compare-and-set (`--expect-current` /
`--expect-unbound` / `--supersede`), `--gate`, `--from` / `--from-promotion`,
`--plan`, the run flags, **every exit code and the JSON envelope**, and every
refusal. A deploy that names no version keeps today's spec-change behaviour.

Two refusals are new, and both reject a flag that would otherwise do nothing
silently — the failure worth refusing, because a pipeline that passed
`--fail-fast` and got no gate would believe it had one:

- a release-only flag (`--plan`, `--gate`, `--expect-current`, …) on a deploy
  that names **no** release, which reads as a guard and has none;
- `--timeout` / `--fail-fast` beside `--no-wait`, which contradict each other.

`--explain` plus a release is also refused, pointing at `--plan` (the preview
that writes nothing and shows the change set) or a bare `--explain`.

## Three phantom fields `internal/deadcodeguard` caught, and the design fixes

The guard found three fields whose only writers were tests, so every production
read observed a zero value. All three were real; none got a quarantine entry.

- **`promoteOptions.{Bindings,Releases}` + a `*bool Hosted` → one required
  `Ledger envLedger`.** The three-field shape let a caller state a hosted store
  and leave `Hosted` false — an env whose promotions live on a control plane
  that then applies nothing, exactly the state this split must not be able to
  express. `envLedger` already holds them together and its hosted constructor
  sets `Hosted: true` beside the stores. It is now **required**, resolved at the
  command by `resolveReleaseLedger`; resolving it inside `runPromote` is what
  had made it a field no production path set.
- **`promoteCmdFlags.from` → flat `fromEnv` / `fromPromotionID`**, folded into
  `promoteFromOptions` at the call site.
- **`envWaitOptions.AllowNonConverging` deleted**, with the bypass it guarded.
  Its only setter was the retired verb's apply-then-wait pair; a self-managed
  env no longer comes through that wait at all.

## Tests

`promote_test.go` → `deploy_promote_test.go`, `promote_wait{,_test}.go` →
`deploy_promote_follow{,_test}.go`. The new behaviour is pinned by
`TestDeployRelease_*`: waits-by-default, waits on the promotion the write
returned, the no-op retry, the wait's exit code becoming the deploy's, the
failed-wait-still-emits-`recorded.id` seam, `--no-wait`, and the four
self-managed cases (applies client-side, apply failure fails the deploy, the
gate flags tune the rollout policy, the apply flags are forwarded).

**Verified they fail before and pass after:** regressing `followPromote` to
opt-in waiting fails 7 tests; disabling the self-managed branch fails 4.

`promoteOptions.Follow` is a **pointer**, and nil means ledger-write only. That
is what keeps the plan/CAS/gates/provenance tests — and `forge release`'s
fixtures — asserting what was _written_ without needing a cluster.

The removalguard entry was worth its keep immediately: it found **12
stragglers** the grep pass missed, including five live
`runForge(t, "env", "promote", …)` calls whose tokens are never adjacent in the
source, which is exactly what the ARGV pattern exists for.

## Also

`newDeployCmd` was 229 lines and over the `funlen` gate. Split along the seam
the verb already has: `registerDeployApplyFlags` beside `registerPromoteFlags`,
the help text hoisted to `deployCmdLong`, and the twenty locals collapsed into
the one `deployCmdFlags` the dispatch already reads — which removes the
hand-copied struct literal in `RunE` where a newly added flag used to get
silently dropped.

## Gates

- `go test -short ./...` — clean
- full `internal/cli` (non-short, 280s) — `ok`
- `forge lint --no-fix` — exit 0 (golangci-lint: 0 issues)

## Scope

Touches only V3's files. The one shared line is `internal/cli/env.go`'s command
registration (`newPromoteCmd()` removed). `internal/cli/env_wait.go` is edited
for the `AllowNonConverging` deletion and the stale cross-references the
removalguard required; `forge env build` is **not** used anywhere — that is V2's
verb and has not landed, so build references keep today's `forge build` spelling
for V2/V5 to sweep.

## Drive-by: `origin/main` was already red, and this fixes it

Rebasing onto V1 (#379) surfaced that `internal/removalguard` fails on
`origin/main` **independently of this branch**. V1 committed `PR_BODY_V1.md`,
and a PR description for a removal names the removed spelling dozens of times —
that is what the description is for — so the guard reports every mention as a
surviving reference. Verified against a clean worktree of `origin/main`: 4
surviving references to "the top-level `forge run` dev runner", all of them
lines in that file.

Fixed by skipping `PR_BODY*.md` by basename prefix, beside `go.sum` and the
lockfiles in the existing "not a forge surface" seam. The alternative — one
allowance per PR body per removal — would be a standing carve-out in the table
for text no release ever reads, which is the too-permissive-allowance failure
that file explicitly warns against.
