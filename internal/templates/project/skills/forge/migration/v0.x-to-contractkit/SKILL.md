---
name: v0.x-to-contractkit
description: Migrate generated mock/middleware/tracing/metrics shape from inline-everything to contractkit shim + library. Forge versions before 1.5 used the old shape; 1.5+ uses contractkit.
---

# Migrating from pre-1.5 inline codegen to `forge/pkg/contractkit`

Use this skill when `forge upgrade` reports a jump across forge `1.4.x → 1.5.x`
(or the broader `0.x → 1.5+` range for legacy projects). It is the canonical
example of a per-version migration skill — future migrations follow the same
six-section shape.

## 1. What changed

Forge versions before 1.5 emitted ~30 lines per method per generated file
across `mock_gen.go`, `middleware_gen.go`, `tracing_gen.go`, and
`metrics_gen.go`. Every method's recording, log-attribute building, span
naming, and metric recording was inlined into the generated body, so a
5-method contract produced four files of ~150 LOC each.

Forge `1.5.0+` emits a thin per-method shim (~3 lines) that delegates into
`forge/pkg/contractkit`. Behavioural fingerprints are preserved exactly:

- Mock not-set error string: `MockMyService.XxxFunc not set`
- Slog attribute keys (`method`, `error`, `dur_ms`, etc.)
- Span name shape (`<package>.<Method>`)
- Metric names (`<package>_<method>_duration_seconds`, etc.)

The library owns the recording shape; templates own the per-method signature.

## 2. Detection

How to tell which shape the project currently uses:

```bash
# Old shape: no contractkit import, large mock_gen.go bodies.
grep -l "contractkit\." internal/*/mock_gen.go 2>/dev/null \
  || echo "OLD SHAPE — contractkit not yet in use"

# Quick LOC check: old-shape mocks weigh in around 50+ lines for a
# single 5-method contract; new-shape ones are closer to 30.
wc -l internal/*/mock_gen.go 2>/dev/null
```

## 3. Migration (deterministic part)

```bash
# Optional safety: list everything that's about to be regenerated.
git diff --name-only -- '*_gen.go' > /tmp/forge-gen-files-before.txt

# Apply: regenerate everything in-place.
forge generate

# Verify: build should be clean. If it's not, check for direct
# references to renamed symbols (rare — see section 4).
go build ./...
```

This part is fully automated by `forge upgrade` itself when it runs
`forge generate` after the template upgrade.

## 4. Migration (manual part)

What user code might need to change:

- **Direct `XxxFunc` field access on a mock — no change.** The `Func`
  fields on each mock are preserved exactly.
- **Comparing mock not-set error strings — no change.** The format
  `MockMyService.XxxFunc not set` is preserved exactly and is locked by
  `TestMockNotSet_FingerprintLocked` in `forge/pkg/contractkit`.
- **Setting log/trace/metrics options via the old wrapper struct.**
  The old shape buried slog/otel options inside the generated wrapper
  itself. The new shape exposes `contractkit.Recorder` plus helper
  functions; configure once at construction time. See
  `forge/pkg/contractkit` package docs.
- **Custom code that imported the old per-template helper symbols.**
  The old generator emitted unexported helpers like `mockNotSet` /
  `recordSpan` directly into each `_gen.go`. They've moved to
  `contractkit.MockNotSet` / `contractkit.SpanRecorder`. If your
  hand-written code reached into those — rare — update the import.

## 5. Verification

```bash
go build ./... && go test ./... && forge lint
```

If all three pass, `forge upgrade` will bump `forge_version` in
`forge.yaml` to the target version automatically. If you're applying
the migration manually rather than via `forge upgrade`, hand-edit
`forge_version: 1.5.0` (or whatever ships contractkit).

## 6. Rollback

If something breaks:

```bash
git revert <forge-generate-commit>      # undo the regen
forge upgrade --to 1.4.x                # pin back to the prior version
```

`--to 1.4.x` requires having the older forge build on `PATH` first;
install with `go install github.com/reliant-labs/forge/cmd/forge@vX.Y.Z`.

The forge_version field in forge.yaml will be reset to `1.4.x` so that
subsequent `forge generate` runs won't warn about a mismatch with the
older binary.
