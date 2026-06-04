# Diagnostics: surfacing unwired scaffolds at boot

- **Status**: proposed
- **Author**: forge core (kalshi-trader silent-stall postmortem)
- **Related**: `FORGE_BACKLOG.md` top entry (2026-06-03)
- **Touches**: new `pkg/diagnostics`, codegen hooks in `internal/codegen/wire_gen.go`, templates `internal/templates/project/wire_gen.go.tmpl` + `internal/templates/service/handlers.go.tmpl` + `internal/templates/project/bootstrap.go.tmpl`, audit category in `internal/cli/audit.go`, lint rule alongside `internal/cli/lint_wire_coverage.go`.

## Context

On 2026-06-03 the kalshi-trader bot placed zero trades for ~24 hours. Root cause: a scaffolded `internal/botconfig/config.go::LoadFromYAML` returning `ErrNotImplemented`, the user-owned `pkg/app/setup.go` quietly falling back to a Go-literal default whose knobs were 2–6× looser than the on-disk YAML (`kelly_fraction 0.25` vs `0.10`), and several `wire_gen.go` Deps constructed as `nil` with a `DELIBERATELY LEFT NIL` comment. Boot succeeded, cron workers ticked and returned immediately, no errors raised.

The contract layer is doing its job: `validateDeps` (`internal/codegen/wire_gen.go:39`) fails at construction when a `required` Deps field is missing, and `forge lint --wire-coverage` (`internal/cli/lint_wire_coverage.go:19-46`) reports the canonical `// TODO: wire <field>` markers wire-gen emits. The gap is between *codegen-time signals* (only visible to a developer running `forge lint`) and *runtime visibility* (the operator watching boot logs in prod). A stubbed loader returning `ErrNotImplemented` is **caller-handled** today — `setup.go` deliberately catches it and falls back — so neither validateDeps nor wire-coverage fires. A `nil` Deps marked `// forge:optional-dep` silently passes through. Both are legitimate migration ergonomics; both caused the outage.

We need a third signal: a *runtime boot-warn surface* that names the unwired scaffolds out loud, structured, every time the binary starts. Warn, not fatal — migration paths legitimately ship partial scaffolds. Loud, not buried — ignorable for one tick is fine; ignorable for a week is the failure mode we're preventing.

## Decision drivers

1. **Warn by default, opt-in strict mode.** Default is `level=warn` log lines, not `os.Exit`. `forge.yaml: features.strict_wiring: true` flips warns to fatal for projects that want production-grade enforcement.
2. **Codegen-time detection, runtime-time emission.** We already have AST machinery for both rules (wire-gen has the producer-resolution map at `internal/codegen/wire_gen.go:175-197`; the scaffolds linter walks bodies at `internal/linter/scaffolds/scaffolds.go:160-220`). Doing the detection at boot via runtime reflection would be expensive and brittle. We emit a `diagnostics_gen.go` slice of literals at generate time and the boot path iterates it.
3. **One `Diagnostic` shape, JSON-marshalable.** Reuses the `forge audit --json` additive-extension contract (see the `audit-json` skill). Existing consumers don't break; a new finding type appears as a new key on the audit category.
4. **Acknowledged-stub opt-out.** A `// forge:stub-ok reason=...` marker on the user's side (in `setup.go`, `app_extras.go`, or beside the offending stub function) suppresses the warning. Marker is in user-owned code, not generated code, so regen never wipes the acknowledgement.
5. **Adopt existing primitives.** The `Finding`/`Severity`/`Result` shape from `internal/linter/forgeconv/forgeconv.go:32-65` and `internal/linter/scaffolds/scaffolds.go:32-61` is the canonical lint diagnostic shape; the runtime `Diagnostic` mirrors it. The `pkg/observe` package already owns Connect interceptors and structured logging conventions — diagnostics emits via the same `*slog.Logger` the rest of the boot path uses.

## Package location and naming

The user prompt floated `pkg/forge/diagnostics`. There is no `pkg/forge/` namespace in the forge repo today — packages are flat under `pkg/` (`pkg/auth`, `pkg/observe`, `pkg/svcerr`, `pkg/testkit`, etc., per the `forge-libraries` skill index). The proposal is **`pkg/diagnostics`**, imported as `github.com/reliant-labs/forge/pkg/diagnostics`. Matches every other `pkg/*` package in the repo and keeps the import line short.

Sub-files mirror the `pkg/auth` multi-file pattern (`auth.go`, `multi.go`, ...) rather than the single-file `pkg/svcerr/svcerr.go` shape, because diagnostics has four distinct lifecycle concerns (types, registry, emitter, strict mode) and a single 600-line file would obscure them.

## Section 1 — Types and public API

### `Diagnostic`

```go
type Kind string

const (
    KindStubImpl Kind = "stub-impl"  // generated body is solely return ..., ErrNotImplemented
    KindNilDep   Kind = "nil-dep"    // wire_gen.go constructed a Deps field as nil
)

type Severity string

const (
    SeverityWarn  Severity = "warn"
    SeverityError Severity = "error"  // emitted only when strict_wiring is on
)

type Diagnostic struct {
    Kind      Kind     `json:"kind"`
    Symbol    string   `json:"symbol"`              // "internal/botconfig.LoadFromYAML" or "app.PgUnsettled"
    File      string   `json:"file"`                // project-relative path
    Line      int      `json:"line"`
    Component string   `json:"component,omitempty"` // wire_gen function: "wireWorkerCalibratorRefitDeps"
    DepName   string   `json:"dep_name,omitempty"`  // for KindNilDep: the Deps field name
    Message   string   `json:"message"`             // human-readable one-liner
    Severity  Severity `json:"severity"`
}
```

JSON-marshalable so `forge audit --json` can roll the slice up under a new `unwired_scaffolds` category. The `Symbol` shape matches the canonical Go package.identifier form used in lint findings and stack traces. `File` is project-relative (same convention as `forgeconv.Finding.File`, see `internal/linter/forgeconv/forgeconv.go:45`).

### `Emitter`

```go
type Emitter interface {
    Emit(Diagnostic)         // one structured log line per call
    Summary([]Diagnostic)    // single roll-up line at the end of Boot
}
```

Implementations live in `emitter.go`:

- `LogEmitter{Logger *slog.Logger}` — default; writes structured slog lines at `Warn` level with the stable event name `forge.scaffold.unwired`. Summary emits `forge.scaffold.unwired.summary count=N items=[...]`.
- `NopEmitter{}` — for tests that don't care about diagnostic output.
- `MetricsEmitter{Meter metric.Meter}` — opt-in OTel counter (`forge.scaffold.unwired.count{kind, component}`) so wiring gaps show up on a dashboard. Composable with `LogEmitter` via `MultiEmitter`.

### Registry and lifecycle

```go
type Registry struct { ... }

func NewRegistry() *Registry

// RegisterStub is called by generated code (diagnostics_gen.go) at boot.
func (r *Registry) RegisterStub(symbol, file string, line int)

// RegisterNilDep is called by generated code (diagnostics_gen.go) at boot.
func (r *Registry) RegisterNilDep(component, depName, file string, line int)

// Boot emits all registered diagnostics through the supplied Emitter and
// returns the full slice for callers that want to roll up into audit JSON.
func (r *Registry) Boot(e Emitter) []Diagnostic

// Default is the package-level registry used when codegen-emitted
// diagnostics_gen.go init() runs before *App is constructed.
var Default = NewRegistry()
```

Two flat methods (Register / Boot) plus a Summary on the Emitter. Resist the temptation to wire this into a service-locator — it's observability infrastructure, not a framework.

### Strict mode

A `StrictEmitter` decorates any base `Emitter`:

```go
type StrictEmitter struct{ Base Emitter }

func (s StrictEmitter) Emit(d Diagnostic) {
    s.Base.Emit(d)
}

func (s StrictEmitter) Summary(ds []Diagnostic) {
    s.Base.Summary(ds)
    if len(ds) > 0 {
        // Single log line then exit so logs flush.
        log.Fatalf("forge.scaffold.unwired: strict_wiring is on and %d unwired scaffold(s) were registered", len(ds))
    }
}
```

`StrictEmitter` is *not* the default. Bootstrap wires it only when `cfg.Features.StrictWiring()` returns true (new field on `FeaturesConfig` at `internal/config/config.go:593` — additive, defaults to false, no migration impact).

## Section 2 — Generate-time detection

### Stub-impl detection (rule 1)

The simplest signal is a **marker comment in the generated function body** inserted by the template at generation time. Two existing templates emit Tier-1 stubs we want to detect:

- `internal/templates/service/handlers.go.tmpl:15` — emits `// FORGE_SCAFFOLD: implement business logic for {{.Name}}` then `return nil, connect.NewError(connect.CodeUnimplemented, ...)`.
- Any new Tier-1 scaffold (`internal/botconfig/config.go::LoadFromYAML` style) emitted via the internal-package template would carry the same marker.

The handler template gets one extra line:

```diff
   // FORGE_SCAFFOLD: implement business logic for {{.Name}}.
   // Remove this comment after editing.
+  // forge:gen unwired-stub symbol={{.ServicePackage}}.{{.Name}}
   func (s *Service) {{.Name}}(
```

The `forge:gen unwired-stub` marker is the codegen-side flag. A **second codegen step**, `GenerateDiagnostics`, runs after wire-gen and handler-gen, walks all `_gen.go` files for the marker, and emits a single `pkg/app/diagnostics_gen.go`:

```go
// Code generated by forge. DO NOT EDIT.
// Source: handlers/*/handlers.go (forge:gen unwired-stub markers) + pkg/app/wire_gen.go (nil-dep markers).
package app

import "github.com/reliant-labs/forge/pkg/diagnostics"

func init() {
    diagnostics.Default.RegisterStub("kalshi_admin.Login",   "handlers/kalshi_admin/handlers.go", 42)
    diagnostics.Default.RegisterStub("botconfig.LoadFromYAML", "internal/botconfig/config.go",      18)
    diagnostics.Default.RegisterNilDep("wireWorkerCalibratorRefitDeps", "PgUnsettled", "pkg/app/wire_gen.go", 128)
    diagnostics.Default.RegisterNilDep("wireWorkerCalibratorRefitDeps", "Predictions", "pkg/app/wire_gen.go", 129)
}
```

The codegen lives next to `internal/codegen/wire_gen.go` as `internal/codegen/diagnostics_gen.go`. It reuses the same AST-walk machinery `lint_wire_coverage.go:196-250` already uses to find `// TODO: wire <field>` markers. We extend that walk to also capture `// forge:gen unwired-stub symbol=...` markers in handler bodies.

This is cheap and unambiguous. AST-scanning user code at boot would be expensive (every `forge run` would re-parse the project) and brittle (user edits to handler bodies could move/rename functions). The codegen-time approach captures the state at the last regeneration boundary, which is exactly what the operator wants to know about.

### Nil-dep detection (rule 2)

`internal/codegen/wire_gen.go:344-349` already tracks `UnresolvedFields` per service. The template at `internal/templates/project/wire_gen.go.tmpl:108-110` renders them into a header comment block. Same data feeds `GenerateDiagnostics`:

```diff
   for _, df := range depsFields {
       expr, comment, unresolved := wireExpressionForApp(...)
+      if unresolved != "" && !df.StubOK {  // StubOK set when the Deps field carries // forge:stub-ok
+          diagEntries = append(diagEntries, DiagnosticEntry{
+              Kind: "nil-dep", Component: "wire" + fieldName + "Deps",
+              DepName: df.Name, File: "pkg/app/wire_gen.go", Line: ...,
+          })
+      }
```

We deliberately do *not* emit a `nil-dep` entry for fields marked `// forge:optional-dep` (already a recognized marker, see `internal/linter/forgeconv/optional_dep_marker.go:13-17`). The user explicitly opted in to "may be nil"; warning every regen would be noise. The new `// forge:stub-ok reason=...` marker is the second-class opt-out — same suppression, but the user must supply a reason string, which lands in the codegen-time comment and is greppable on review.

### Acknowledged-stub opt-out

```go
// forge:stub-ok reason="settlement backfill ships in v0.4; nil keeps Tick a no-op until then"
PgUnsettled UnsettledRepo
```

The marker:
- Suppresses the diagnostic entry at codegen time (no slot in `diagnostics_gen.go`).
- Survives `forge generate` (it's in user-owned `service.go`, not `wire_gen.go`).
- Is visible to anyone reading the Deps struct in code review.

We keep `// forge:optional-dep` as the existing semantic (gate validateDeps, suppress UNRESOLVED warning). `// forge:stub-ok` is the new orthogonal semantic ("I know this is a stub, don't warn me again"). Both can coexist on the same field.

## Section 3 — Runtime boot-warn surface

`pkg/app/bootstrap.go` already runs `Setup(app, cfg)` first, then constructs services / workers / operators (`internal/templates/project/bootstrap.go.tmpl:240`). The new wire point sits between Setup and the first wire-call:

```diff
   if err := Setup(app, cfg); err != nil {
       return nil, fmt.Errorf("setup: %w", err)
   }
+  // diagnostics_gen.go's init() has already populated diagnostics.Default.
+  // Emit now so warns land before any service or worker starts.
+  emitter := diagnostics.NewLogEmitter(logger)
+  if cfg.Features.StrictWiring() {
+      emitter = diagnostics.StrictEmitter{Base: emitter}
+  }
+  app.unwired = diagnostics.Default.Boot(emitter)
```

The summary line format is one structured log per diagnostic, then one roll-up:

```
WARN forge.scaffold.unwired kind=stub-impl symbol=botconfig.LoadFromYAML file=internal/botconfig/config.go line=18
WARN forge.scaffold.unwired kind=nil-dep   component=wireWorkerCalibratorRefitDeps dep=PgUnsettled file=pkg/app/wire_gen.go line=128
WARN forge.scaffold.unwired.summary count=2 items=["botconfig.LoadFromYAML","wireWorkerCalibratorRefitDeps.PgUnsettled"]
```

The `event=forge.scaffold.unwired` slog attribute is stable. Operators can grep boot logs for the event name; dashboards can chart the summary count over time and alert when count > 0 for >24h (the gap between "saw it once" and "ignored it for a week").

## Section 4 — Template changes (sketch)

### `internal/templates/service/handlers.go.tmpl`

```diff
   // {{.Name}} implements the {{.Name}} RPC.
   // FORGE_SCAFFOLD: implement business logic for {{.Name}}.
   // Remove this comment after editing.
+  // forge:gen unwired-stub symbol={{.ServicePackage}}.{{.Name}}
   func (s *Service) {{.Name}}(
       ctx context.Context,
       req *connect.Request[pb.{{.InputType}}],
   ) (*connect.Response[pb.{{.OutputType}}], error) {
       return nil, connect.NewError(connect.CodeUnimplemented, fmt.Errorf("handler for %s not yet implemented", "{{.Name}}"))
   }
```

The `// forge:gen unwired-stub` line is the codegen-side flag. When the user implements the handler and removes the `FORGE_SCAFFOLD:` line (per the existing `forge lint --scaffolds` rule, see `internal/linter/scaffolds/scaffolds.go:177-187`), the unwired-stub marker is also removed in the same edit, and the next regen sees a non-stub body — `diagnostics_gen.go` simply doesn't emit an entry for that symbol.

### `internal/templates/project/wire_gen.go.tmpl`

No template change. The codegen call site at `internal/codegen/wire_gen.go:344` collects entries; the new `internal/codegen/diagnostics_gen.go` emits the registration file. The existing `// UNRESOLVED FIELDS` header comment stays as the developer-facing grep signal; the runtime registration is the operator-facing signal.

### Generated `pkg/app/diagnostics_gen.go` before / after

**Before:** no file. Boot proceeds, `botconfig.LoadFromYAML` returns `ErrNotImplemented`, `setup.go` swallows, cron workers tick and no-op, operator sees nothing.

**After:**

```go
// Code generated by forge. DO NOT EDIT.
// Source: handlers/**/*_gen.go (forge:gen unwired-stub) + pkg/app/wire_gen.go (nil-dep).
package app

import "github.com/reliant-labs/forge/pkg/diagnostics"

func init() {
    diagnostics.Default.RegisterStub("botconfig.LoadFromYAML", "internal/botconfig/config.go", 18)
    diagnostics.Default.RegisterNilDep("wireWorkerCalibratorRefitDeps", "PgUnsettled", "pkg/app/wire_gen.go", 128)
}
```

Operator sees the warnings, fixes the loader, regen produces an empty `diagnostics_gen.go` (still committed — its emptiness is the clean-scaffold signal), warnings stop.

## Section 5 — `forge audit` and `forge lint` integration

### Audit

A new category `unwired_scaffolds` plugs into `internal/cli/audit.go:166-178` alongside the existing `wire_coverage` category:

```go
report.Categories["unwired_scaffolds"] = auditUnwiredScaffolds(abs)
```

Status semantics match the additive contract documented in the `audit-json` skill: `ok` (no diagnostics file or empty registry), `warn` (>0 entries), `error` (>0 entries AND `features.strict_wiring: true`). Details:

```jsonc
"unwired_scaffolds": {
  "status": "warn",
  "summary": "2 unwired scaffolds across 2 components",
  "details": {
    "stub_impl": [{"symbol": "...", "file": "...", "line": 18}],
    "nil_dep":   [{"component": "...", "dep_name": "...", "file": "...", "line": 128}],
    "strict_wiring_enabled": false
  }
}
```

Consumers `select` keys they care about per the additive-extension contract; unknown extras are tolerated.

### Lint

`forge lint --wire-coverage` already handles the nil-dep half via the `// TODO: wire <field>` scan (see `internal/cli/lint_wire_coverage.go:48-52` / `:188-250`). The stub-impl half lands as a sibling rule `forge lint --stub-impls` that reuses the AST walk and emits the same `Finding` shape:

```go
type stubImplFinding struct {
    Symbol, File string
    Line         int
}
```

The lint never gates the build by default — matches the `wire-coverage` precedent at `internal/cli/lint_wire_coverage.go:316-317` ("warnings only — not failing the build"). When `forge.yaml: features.strict_wiring: true` is set, both rules return non-nil errors so CI gates the merge.

## Section 6 — Testing strategy

### Unit tests (in the package)

- `registry_test.go` — `RegisterStub` / `RegisterNilDep` produce diagnostics with the right `Kind`, `Symbol`, `Component`, `DepName`. `Boot` emits one Emit call per registered entry plus one Summary call. Ordering: stable by `(Kind, Symbol)` so summary text is deterministic.
- `emitter_test.go` — `LogEmitter` writes the canonical `event=forge.scaffold.unwired` attribute and severity=warn level. Captured via `slog.NewTextHandler` against a `bytes.Buffer`.
- `strict_test.go` — `StrictEmitter` calls Base.Emit then exits on non-empty Summary. Test uses an `osExit` indirection (override `osExit = func(int){ panic("exit") }`) so the test process survives.

### Generate-time tests (codegen package)

- `internal/codegen/diagnostics_gen_test.go` — golden-file test: fixture project with one handler containing `// forge:gen unwired-stub` marker → emit `diagnostics_gen.go` matching the golden file. Mirror the pattern in `internal/codegen/wire_gen_test.go`.
- Scan the `forge:stub-ok reason=...` marker on Deps fields and assert that marked fields produce *no* entries.

### End-to-end test

Extend `internal/cli/scaffold_lifecycle_e2e_test.go` with a kalshi-trader-shaped fixture (stubbed loader, nil-wired Deps, no opt-out). Assert:

1. `forge generate` writes `pkg/app/diagnostics_gen.go` with two entries.
2. `go run ./cmd/server` emits both `forge.scaffold.unwired` log lines on stdout within 2s.
3. Adding `// forge:stub-ok reason="x"` to a Deps field drops that entry on regen.
4. `features.strict_wiring: true` makes `cmd/server` exit non-zero with the summary as the last log.

## Open questions

1. **Severity granularity.** Worth adding `info` for `forge:stub-ok`-marked entries (audit-visible, no boot log)? Lean *no* — opt-out is opt-out.
2. **Marker naming.** `// forge:gen unwired-stub` matches the `forge:<area> <subkind>` shape used at `internal/linter/scaffolds/banners.go:122`. Alternatives: `forge:diagnostic stub-impl`, `forge:unwired stub`.
3. **Detection scope.** Should we also flag user-edited setup.go fallbacks? *No* — that's user-owned; the `forge:stub-ok` marker on the consuming Deps field is the right place to express "caller knows it might be unwired."

## Consequences

**Good.**
- Operator sees structured warns at boot for every unwired scaffold — the kalshi-trader-style silent stall is no longer silent.
- Strict mode gives production-grade projects a build-gating signal without forcing it on migration users.
- The codegen-time → runtime pipeline reuses existing AST infrastructure (`lint_wire_coverage.go`, `scaffolds.go`); no new analysis machinery.
- Audit + lint integrations follow the additive-extension contract; no consumer breaks.

**Bad.**
- One more `_gen.go` file in `pkg/app/`.
- The codegen step must run *after* wire-gen and handler-gen, requiring a pipeline ordering check in `internal/cli/generate_pipeline.go`.
- Two new markers add to the existing `forge:scaffold one-shot` / `forge:optional-dep` / `forge:placeholder` / `forge:allow` set. Worth a follow-up consolidation pass once v0.4 markers stabilize.
