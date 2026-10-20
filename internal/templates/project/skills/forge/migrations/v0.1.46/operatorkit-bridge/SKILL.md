---
name: migration-v0.1.46-operatorkit-bridge
description: Upgrade an owned internal/app/lifecycle.go that names lifecyclekit.Run, lifecyclekit.Controller or lifecyclekit.Options to operatorkit, so importing lifecyclekit never links controller-runtime.
relevance: migration
---

# v0.1.46 — the operator bridge moved out of lifecyclekit

Use when `internal/app/lifecycle.go` calls `lifecyclekit.Run` or names
`lifecyclekit.Controller` / `lifecyclekit.Options`.

## What changed

`forge/pkg/lifecyclekit` re-exported `operatorkit.Run`, `Controller` and
`Options`, so every binary that imported lifecyclekit — for `Supervise`,
`WrapWorker` or `Singleton` — linked `sigs.k8s.io/controller-runtime` and its
Kubernetes tree, operators or not. The three names are gone from lifecyclekit;
they live only in `forge/pkg/appkit/operatorkit`, which a binary imports only
when it runs operators. Nothing else in lifecyclekit changed.

## Steps

1. In `internal/app/lifecycle.go` (yours; `forge generate` does not rewrite
   it), import `github.com/reliant-labs/forge/pkg/appkit/operatorkit` and
   replace `lifecyclekit.Controller`, `lifecyclekit.Options` and
   `lifecyclekit.Run` with `operatorkit.Controller`, `operatorkit.Options` and
   `operatorkit.Run`. The signatures are identical.
2. Any other file naming those three lifecyclekit symbols: the same rename.

## Verify

`go build ./...` passes, and `go list -deps ./<a package that imports only
lifecyclekit>` no longer lists `sigs.k8s.io/controller-runtime`.
