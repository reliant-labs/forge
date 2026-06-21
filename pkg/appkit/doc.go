// Package appkit holds the worker/operator supervised-component
// contracts that the generated internal/app composition layer re-uses.
//
// # Status (FORGE_SHAPE_REDESIGN §2)
//
// appkit was once the runtime DI engine behind a generated
// pkg/app/bootstrap.go table (a string-keyed Def/ServiceDef/Run that
// constructed, wired, and mounted every component). That engine is
// RETIRED. The live composition path is the generated cmd/server.go over
// the internal/app layer (OpenInfra → Build → PostBuild → mount via
// Inventory → serverkit.Run); construction is typed and owned, not
// string-keyed and reflected.
//
// What remains here is the small, stable supervised-component surface
// the new path still consults:
//
//   - [WorkerInstance] / [ContextWorkerInstance] — lifecycle wrappers
//     (Name / Start / Stop, plus the ctx-aware RunContext sibling) that
//     satisfy serverkit.Worker / serverkit.ContextWorker.
//   - [WrapWorker] — the runtime type-switch the generated
//     internal/app/lifecycle_gen.go calls per worker row: a worker that
//     implements RunContext gets the ctx-aware wrapper (per-worker
//     cancel-on-shutdown), everything else gets the legacy Start wrapper.
//   - [WorkerLifecycle] — the Start/Stop shape every generated worker
//     type exposes.
//
// Adding a worker needs no `forge generate` change to this package — the
// type-switch detects RunContext at runtime.
package appkit
