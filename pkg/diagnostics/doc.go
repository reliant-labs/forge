// Package diagnostics surfaces unwired forge scaffolds at runtime.
//
// # Why this exists
//
// Forge scaffolds Tier-1 stubs that compile cleanly but return
// `connect.CodeUnimplemented` / `ErrNotImplemented`, and wire_gen.go
// constructs Deps fields as typed-zero values when no producer matched
// (see `internal/codegen/wire_gen.go` for the producer-resolution
// rules). Both are legitimate during a migration: the user can ship a
// scaffold that hasn't been filled in yet and still have a passing
// build. Both also caused a real production outage (kalshi-trader,
// 2026-06-03) when an operator was unaware that the on-disk YAML
// config wasn't being loaded — the stubbed loader silently returned
// `ErrNotImplemented`, the caller fell back to a Go-literal default
// with looser knobs, and a cron worker no-op'd for ~24h before anyone
// noticed.
//
// `forge lint --wire-coverage` catches the nil-dep half at develop
// time. `forge lint --scaffolds` catches `FORGE_SCAFFOLD:` markers at
// commit time. Neither is visible to the operator watching boot logs
// in production. This package is the runtime third leg of that stool:
// a structured boot-time warn surface that emits one log line per
// unwired scaffold plus a roll-up summary, every time the binary
// starts.
//
// # Shape
//
// Codegen emits one `pkg/app/diagnostics_gen.go` per project. Its
// `init()` calls `diagnostics.Default.RegisterStub(...)` and
// `diagnostics.Default.RegisterNilDep(...)` for every scaffold the
// codegen detected at generate time. Bootstrap calls `Default.Boot(e)`
// after `Setup()` returns, where `e` is a `LogEmitter` wrapping the
// project's `*slog.Logger`. Each diagnostic produces one structured
// log line at warn level with the stable event name
// `forge.scaffold.unwired`; the roll-up summary uses
// `forge.scaffold.unwired.summary`.
//
// # Strict mode
//
// When `forge.yaml: features.strict_wiring: true`, Bootstrap wraps the
// base emitter with `StrictEmitter`, which calls `log.Fatalf` (and
// thus exits non-zero) after the summary if any diagnostic was
// emitted. Default-off: migration paths legitimately ship partial
// scaffolds, and forcing strict mode on every project would block the
// very scaffolding the package is designed to make safer to ship.
//
// # Acknowledged-stub opt-out
//
// Users can mark a Deps field with `// forge:stub-ok reason=...` to
// suppress the corresponding diagnostic. The marker lives in
// user-owned code (handlers/<svc>/service.go), so regen never wipes
// the acknowledgement. Distinct from `// forge:optional-dep`, which
// also gates validateDeps; `forge:stub-ok` only suppresses the
// unwired-scaffold diagnostic.
package diagnostics
