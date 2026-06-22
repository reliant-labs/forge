---
name: v0.1-to-v0.2
description: TOMBSTONE — retired migration. v0.2's name-matched wire_gen.go DI (pkg/app/{wire_gen,app_gen,app_extras}.go, AppExtras exact-name resolution) was itself removed. Do NOT migrate toward this shape. To arrive at the current explicit composition root, use v0.x-to-typed-di (and v0.x-to-serverkit-composed for the typed-mount cmd layer).
relevance: migration
---

# Migrating from forge v0.1 to v0.2 — RETIRED

**This migration target no longer exists. Do not follow it.**

v0.2 introduced a codegen-based, name-matched DI shape:
`pkg/app/wire_gen.go` with one `wireXxxDeps(app, cfg, logger)` per
service, an `*App` god-struct, and a user-extension `AppExtras` whose
fields wire_gen resolved against each `Deps` field by EXACT NAME. That
whole `pkg/app` DI unit has since been **deleted**. Name-matching
silently dropped narrow-interface mismatches (a consumer's narrow
`Repository` interface vs the concrete `*PostgresRepository` field would
be skipped, leaving a nil hazard at runtime), so it was replaced by
explicit, compile-checked composition.

If you are upgrading a project that is *still* on the v0.1
Bootstrap+Setup+ApplyDeps shape, or on the v0.2 `wire_gen.go` shape, do
NOT migrate to `wire_gen.go`. Migrate straight to the current explicit
composition root:

- **`migrations/v0.x-to-typed-di`** — the canonical "arrive at the
  explicit composition root" upgrade. The destination is an OWNED
  `internal/app/providers.go` (`Infra` struct + `OpenInfra`) plus a
  generated `internal/app/compose.go` with
  `func NewComponents(infra *Infra) (*Components, error)` that resolves
  every `Deps` field BY TYPE — no `*App`, no `AppExtras`, no name match,
  no `validateDeps`-fires-too-early problem. A missing provider is a loud
  generate-/compile-time error.
- **`migrations/v0.x-to-serverkit-composed`** — the matching cmd-layer
  upgrade: typed `(*app.Components).Mount<Svc>` mount selection and a
  real cobra subcommand per service, replacing string service-name
  selection through a registry.

Everything the old v0.1→v0.2 codemod did (promote setup-locals to fields,
strip per-RPC nil-checks) is subsumed by those two migrations against the
real, shipping shape.
