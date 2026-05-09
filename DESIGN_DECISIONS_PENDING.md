# Design decisions pending review (forge)

This file accumulates design decisions surfaced during forge work that
need user input. The user reads/clears entries when convenient; the
agent fixes unambiguous bugs autonomously and parks real tradeoffs here.

---

(No open decisions.)

---

## Resolved

### 1. Wire-gen: how do users extend `*App` with new fields? — RESOLVED

**Resolution (2026-05-07).** Shipped **Option A** (Tier-2
`pkg/app/app_extras.go` scaffold). Forge writes an empty `app_extras.go`
with `//forge:allow` and a sibling `AppExtras` struct embedded into
`*App` via a pointer field. Go's field-promotion rules make
`app.<Field>` work for both forge-owned (Services / Workers / etc. on
the bootstrap-owned App struct) and user-owned (any field added to
AppExtras) fields. `ParseAppFields` in
`forge/internal/codegen/deps_parser.go` walks both structs and unions
their fields so wire_gen resolves either path uniformly.

The `// forge:optional-dep` marker shipped this session is the
companion piece for fields users intentionally leave unwired (NATS
publisher rollback path, optional gateway features, etc.) — wire_gen
emits the typed-zero silently and validateDeps skips the check.

### 2. BootstrapOnly lost its lazy-construction-in-shared-mode path — PARKED

Still parked as written. No reports of the lost optimization being
load-bearing; restore the gated construction if anyone notices.
