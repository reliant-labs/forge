//go:build e2e

// ── Testing tiers (read this before running tests) ─────────────────────────
//
// 1. Inner loop (agents, every edit):   go test -short ./...
//    Must complete the whole repo in <60s. Any test that takes >2s
//    (subprocesses, network, real scaffolds) must be -short-gated or
//    have its slow side-effect bypassed under -short, with the slow
//    path still exercised in full mode.
// 2. Package-targeted (pre-commit):     go test ./internal/<pkg>/
//    Full mode, no tag. internal/cli takes ~80s because the
//    TestRunAddFrontend_* tests run a real `npm install`.
// 3. Full gate (once per round / CI):
//    go test -tags e2e -count=1 -timeout 60m -run TestE2E ./internal/cli/
//    The e2e corpus: this file's five real-project fixtures, the
//    scaffold suite (scaffold_*_e2e_test.go), and the registration
//    lifecycle (serve_types_only_e2e_test.go). Each test scaffolds an
//    independent project in its own t.TempDir() and runs real
//    generate/tidy/build/boot, so they are t.Parallel(): wall-clock is
//    roughly the slowest fixture, not the sum. The forge binary is
//    built exactly once per process (sync.Once in scaffold_e2e_test.go).
//    Servers booted by tests must use freePortE2E(t) — never hard-code
//    a port.
//
// See CLAUDE.md "Testing tiers".
// ────────────────────────────────────────────────────────────────────────────

// File: internal/cli/fixture_corpus_e2e_test.go
//
// Real-project fixture corpus: scaffold → generate → build → boot, with
// project shapes complex enough to catch the deps-matcher bug class
// upstream (kalshi FORGE_BACKLOG #13: nondeterministic
// DepsAssignabilityMatcher silently emitting nil for cross-package
// interface deps — caught only when a real downstream project was
// regenerated).
//
// Four fixtures:
//
//   - "cp-forge-shaped" (TestE2EFixtureCorpusCPForgeShaped): 3 services
//     (one with a webhook), 2 internal packages where one package's
//     Deps references a repository interface satisfied by a concrete
//     adapter living in ANOTHER package, a `forge:optional-dep` field,
//     and AppExtras fields wired via setup.go.
//
//   - "kalshi-shaped" (TestE2EFixtureCorpusKalshiShaped): 3 workers
//     with snake_case multi-word names, one implementing
//     RunContext(ctx), one cron worker, and a worker Deps field typed
//     as a worker-local interface satisfied by a concrete adapter on
//     AppExtras — the literal kalshi regression shape.
//
//   - "zero-service" (TestE2EFixtureCorpusZeroService): bare `forge new`
//     with NO --service. Pins the binary-is-not-an-entity contract:
//     zero services scaffolded, generate idempotent at zero, no mcp
//     manifest, zero-component appkit table compiles and boots, and
//     the documented first step (`forge add service item`) works.
//
//   - "frontend-basepath-shaped" (TestE2EFixtureCorpusFrontendBasePath):
//     1 service + 1 Next.js frontend mounted under base_path /admin.
//     The first fixture that renders a FRONTEND: it pins next.config.ts
//     basePath/assetPrefix emission, the generated Tier-1
//     src/lib/basepath_gen.ts helper, prefix-clean generated TSX, and
//     (npm-gated) a real static-export `next build` with
//     /admin-prefixed assets plus the fail-loud empty-override guard.
//
// The Go-shaped fixtures assert, in order:
//
//  1. `forge generate` succeeds AND is idempotent: a second run
//     produces zero file changes (full tree hash) and zero
//     ownership-machinery warnings.
//  2. The generated wiring (wire_gen.go + bootstrap.go) contains NO
//     silent nil / dropped wire for the known name-matched Deps
//     fields — the deps-matcher pin.
//  3. The generated project compiles (`go build ./...` with a local
//     forge/pkg replace).
//  4. The built binary boots, /healthz returns 200, and SIGTERM shuts
//     it down cleanly within a bounded wait.
//  5. Disown lifecycle round-trip on pkg/app/wire_gen.go:
//     hand-edit → generate (drift error with the new option text) →
//     `forge disown --reason` (one-way transfer) → generate (file left
//     alone, zero warnings) → delete + generate (re-adopted to the
//     pristine render, entry back to Tier-1).
//
// Run with:
//
//	go test -tags e2e -run TestE2EFixtureCorpus -v ./internal/cli/
package cli

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/checksums"

	// Pure-Go sqlite driver (no cgo): the dev-mode boot test applies the
	// project's own migrations to the server's database file before boot
	// — the generated server does not auto-migrate.
	_ "modernc.org/sqlite"
)

// ───────────────────────── fixture 1: cp-forge-shaped ─────────────────────────

func TestE2EFixtureCorpusCPForgeShaped(t *testing.T) {
	t.Parallel() // independent project in its own t.TempDir; binary shared via sync.Once
	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()
	start := time.Now()

	// ── Scaffold: 3 services ──────────────────────────────────────────
	runCmd(t, dir, forgeBin, "new", "cpforge",
		"--mod", "example.com/cpforge",
		"--service", "api,billing,reporting",
	)
	projectDir := filepath.Join(dir, "cpforge")
	addCorpusForgePkgReplace(t, projectDir)

	// Webhook on billing (cheap: scaffold-only, registered in forge.yaml).
	runCmd(t, projectDir, forgeBin, "add", "webhook", "stripe", "--service", "billing")
	assertPathExistsE2E(t, filepath.Join(projectDir, "handlers", "billing", "webhook_stripe.go"))

	// ── Internal packages ─────────────────────────────────────────────
	// ledger: classic Service/Deps/New package whose Deps references a
	// repository interface satisfied by a concrete adapter in ANOTHER
	// package (internal/pgstore) — the deps-matcher killer.
	runCmd(t, projectDir, forgeBin, "add", "package", "ledger")
	// pgstore: adapter package hosting the concrete *Store.
	runCmd(t, projectDir, forgeBin, "add", "package", "pgstore", "--type", "adapter")

	// ledger contract: Service + the package-local Repository/Notifier
	// interfaces. Tier-2 (one-shot) — user edits are the steady state.
	writeCorpusFile(t, filepath.Join(projectDir, "internal", "ledger", "contract.go"), `// forge:scaffold one-shot — package contract; canonical pattern lives in `+"`forge skill load contracts`"+`.
package ledger

import "context"

// Service defines the ledger package boundary.
type Service interface {
	// Record persists one ledger entry.
	Record(ctx context.Context, entry string) error
}

// Repository is the ledger's persistence boundary. It is satisfied by
// the CONCRETE *pgstore.Store adapter living in internal/pgstore — the
// cross-package assignability case the deps matcher must prove instead
// of silently wiring nil (kalshi FORGE_BACKLOG #13 bug class). The
// context.Context in the method signatures is the cross-package named
// type that defeated the old two-universe matcher.
type Repository interface {
	SaveEntry(ctx context.Context, entry string) error
	ListEntries(ctx context.Context) ([]string, error)
}

// Notifier receives ledger events. Deliberately optional — see the
// forge:optional-dep marker on Deps.Notifier in service.go.
type Notifier interface {
	Notify(ctx context.Context, event string) error
}
`)

	// ledger Deps: cross-package repo interface + an optional-dep field
	// that IS satisfied via AppExtras (so a silent downgrade to nil
	// would be the exact kalshi regression).
	mustReplaceInFile(t, filepath.Join(projectDir, "internal", "ledger", "service.go"),
		"\tConfig *config.Config\n\t// Add your dependencies here.",
		`	Config *config.Config
	// LedgerRepo is satisfied by *pgstore.Store (concrete adapter in
	// another package) — name-matched against AppExtras.LedgerRepo.
	LedgerRepo Repository
	// Notifier is an optional collaborator, also satisfied by
	// *pgstore.Store on AppExtras. Optional must NOT mean "silently
	// unwired when the matcher gets confused".
	// forge:optional-dep
	Notifier Notifier`)

	// Service-interface implementation for ledger.
	writeCorpusFile(t, filepath.Join(projectDir, "internal", "ledger", "record.go"), `package ledger

import "context"

// Record persists the entry and (optionally) notifies.
func (s *service) Record(ctx context.Context, entry string) error {
	if err := s.deps.LedgerRepo.SaveEntry(ctx, entry); err != nil {
		return err
	}
	if s.deps.Notifier != nil {
		_ = s.deps.Notifier.Notify(ctx, "ledger.recorded")
	}
	return nil
}
`)

	// pgstore: the concrete adapter type. It never names the consumer
	// interfaces — assignability must be PROVEN across packages.
	writeCorpusFile(t, filepath.Join(projectDir, "internal", "pgstore", "store.go"), `package pgstore

import (
	"context"
	"sync"
)

// Store is a concrete in-memory adapter. One value satisfies three
// different consumer-local interfaces (ledger.Repository,
// ledger.Notifier, reporting.ReportSource) without ever importing
// them — the deps-assignability matcher must prove each wire.
type Store struct {
	mu      sync.Mutex
	entries []string
}

// NewStore builds an empty Store.
func NewStore() *Store { return &Store{} }

// SaveEntry appends one entry.
func (s *Store) SaveEntry(_ context.Context, entry string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries = append(s.entries, entry)
	return nil
}

// ListEntries returns a copy of all entries.
func (s *Store) ListEntries(_ context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.entries))
	copy(out, s.entries)
	return out, nil
}

// Notify records the event as an entry (test stand-in for a real sink).
func (s *Store) Notify(ctx context.Context, event string) error {
	return s.SaveEntry(ctx, "event:"+event)
}
`)

	// reporting handler: a HANDLER-LOCAL interface satisfied by the
	// cross-package concrete adapter on AppExtras — the wire_gen-side
	// matcher case (Matcher B).
	writeCorpusFile(t, filepath.Join(projectDir, "handlers", "reporting", "source.go"), `package reporting

import "context"

// ReportSource is reporting's read path. Handler-local interface,
// satisfied by the concrete *pgstore.Store held on AppExtras.
type ReportSource interface {
	ListEntries(ctx context.Context) ([]string, error)
}
`)
	mustReplaceInFile(t, filepath.Join(projectDir, "handlers", "reporting", "service.go"),
		"\tAuthorizer middleware.Authorizer\n\t// Add your dependencies here.",
		`	Authorizer middleware.Authorizer
	// Store is satisfied by *pgstore.Store via the assignability
	// matcher (name match, cross-package concrete type).
	Store ReportSource`)

	// api handler: exact-type match against an AppExtras field that
	// setup.go constructs (ledger.Service on both sides).
	mustReplaceInFile(t, filepath.Join(projectDir, "handlers", "api", "service.go"),
		"\tAuthorizer middleware.Authorizer\n\t// Add your dependencies here.",
		`	Authorizer middleware.Authorizer
	// Ledger is the ledger package's contract interface, constructed
	// in pkg/app/setup.go and exact-type-matched by wire_gen.
	Ledger ledger.Service`)
	mustReplaceInFile(t, filepath.Join(projectDir, "handlers", "api", "service.go"),
		"\t\"example.com/cpforge/pkg/config\"",
		"\t\"example.com/cpforge/internal/ledger\"\n\t\"example.com/cpforge/pkg/config\"")

	// ── AppExtras + setup.go wiring ───────────────────────────────────
	writeCorpusFile(t, filepath.Join(projectDir, "pkg", "app", "app_extras.go"), `// app_extras.go is YOUR code — forge generate will never overwrite it.
//
//forge:scaffold one-shot
//forge:allow
package app

import (
	"example.com/cpforge/internal/ledger"
	"example.com/cpforge/internal/pgstore"
)

// AppExtras is the user-owned extension surface for *App.
// LedgerRepo / Notifier / Store are all the same concrete *pgstore.Store
// satisfying three different consumer-local interfaces across packages.
type AppExtras struct {
	LedgerRepo *pgstore.Store
	Notifier   *pgstore.Store
	Store      *pgstore.Store
	Ledger     ledger.Service
}
`)
	mustReplaceFirstInFile(t, filepath.Join(projectDir, "pkg", "app", "setup.go"),
		"\n\treturn nil\n}",
		"\n\treturn setupExtras(app, cfg)\n}")
	writeCorpusFile(t, filepath.Join(projectDir, "pkg", "app", "setup_extras.go"), `package app

import (
	"fmt"
	"log/slog"

	"example.com/cpforge/internal/ledger"
	"example.com/cpforge/internal/pgstore"
	"example.com/cpforge/pkg/config"
)

// setupExtras wires the user-owned AppExtras fields. Called from Setup.
func setupExtras(app *App, cfg *config.Config) error {
	st := pgstore.NewStore()
	app.LedgerRepo = st
	app.Notifier = st
	app.Store = st

	ledgerSvc, err := ledger.New(ledger.Deps{
		Logger:     slog.Default().With("package", "ledger"),
		Config:     cfg,
		LedgerRepo: st,
		Notifier:   st,
	})
	if err != nil {
		return fmt.Errorf("init ledger: %w", err)
	}
	app.Ledger = ledgerSvc
	return nil
}
`)

	// ── 1. generate ×2 — idempotency is a first-class assertion ──────
	generateTwiceIdempotent(t, forgeBin, projectDir)

	// ── 2. no silent nil for the name-matched Deps fields ────────────
	wireGen := readFileE2E(t, filepath.Join(projectDir, "pkg", "app", "wire_gen.go"))
	assertFieldWired(t, "wire_gen.go", wireGen, "Store")
	assertFieldWired(t, "wire_gen.go", wireGen, "Ledger")
	bootstrap := readFileE2E(t, filepath.Join(projectDir, "pkg", "app", "bootstrap.go"))
	assertFieldWired(t, "bootstrap.go", bootstrap, "LedgerRepo")
	assertFieldWired(t, "bootstrap.go", bootstrap, "Notifier")

	// ── 3. compiles ───────────────────────────────────────────────────
	runCmd(t, projectDir, "go", "build", "./...")

	// ── 4. boots: /healthz 200, clean SIGTERM shutdown ───────────────
	bootHealthzAndShutdown(t, projectDir)

	// ── 5. disown lifecycle round-trip on pkg/app/wire_gen.go ────────
	disownRoundTrip(t, forgeBin, projectDir, "pkg/app/wire_gen.go")

	t.Logf("cp-forge-shaped fixture total: %s", time.Since(start))
}

// ───────────────────────── fixture 2: kalshi-shaped ─────────────────────────

func TestE2EFixtureCorpusKalshiShaped(t *testing.T) {
	t.Parallel() // independent project in its own t.TempDir; binary shared via sync.Once
	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()
	start := time.Now()

	runCmd(t, dir, forgeBin, "new", "kalshishape",
		"--mod", "example.com/kalshishape",
		"--service", "engine",
	)
	projectDir := filepath.Join(dir, "kalshishape")
	addCorpusForgePkgReplace(t, projectDir)

	// ── Workers: snake_case multi-word names, one cron ────────────────
	// --no-generate: scaffold-only; one explicit generate at the end
	// (the parallel-agent staging pattern the flag exists for).
	runCmd(t, projectDir, forgeBin, "add", "worker", "engine_shadow", "--no-generate")
	runCmd(t, projectDir, forgeBin, "add", "worker", "settlement_processor", "--no-generate")
	runCmd(t, projectDir, forgeBin, "add", "worker", "book_snapshotter",
		"--kind", "cron", "--schedule", "0 3 * * *", "--no-generate")
	for _, w := range []string{"engine_shadow", "settlement_processor", "book_snapshotter"} {
		assertPathExistsE2E(t, filepath.Join(projectDir, "workers", w, "worker.go"))
	}

	// settlement_processor: worker-local interface dep (ctx in the
	// method signature — the cross-package named type that defeated the
	// two-universe matcher), satisfied by a concrete adapter on
	// AppExtras, marked forge:optional-dep. The literal kalshi shape.
	mustReplaceInFile(t, filepath.Join(projectDir, "workers", "settlement_processor", "worker.go"),
		"type Deps struct {\n\tLogger *slog.Logger\n\tConfig *config.Config\n}",
		`// UnsettledSource feeds the settlement loop. Worker-local
// interface, satisfied by *marketfeed.Adapter on AppExtras.
type UnsettledSource interface {
	Pending(ctx context.Context) ([]string, error)
}

// Deps contains the dependencies for the settlement_processor worker.
type Deps struct {
	Logger *slog.Logger
	Config *config.Config
	// Unsettled drives the settlement loop. Optional — but optional
	// must never mean "silently downgraded to nil when the matcher
	// cannot prove assignability" (kalshi FORGE_BACKLOG #13).
	// forge:optional-dep
	Unsettled UnsettledSource
}`)
	mustReplaceInFile(t, filepath.Join(projectDir, "workers", "settlement_processor", "worker.go"),
		"\t// TODO: implement your per-cycle work here.",
		`	if w.deps.Unsettled != nil {
		pending, err := w.deps.Unsettled.Pending(ctx)
		if err != nil {
			return err
		}
		w.deps.Logger.InfoContext(ctx, "settlement cycle", "pending", len(pending))
	}`)

	// engine_shadow: ctx-aware run loop. Adding RunContext needs no
	// regenerate — appkit's wrapper detects it at boot.
	appendCorpusFile(t, filepath.Join(projectDir, "workers", "engine_shadow", "worker.go"), `
// RunContext is the ctx-aware run loop (serverkit.ContextWorker). The
// appkit wrapper prefers it over Start when present.
func (w *Worker) RunContext(ctx context.Context) error {
	return w.Start(ctx)
}
`)

	// marketfeed: the concrete adapter satisfying the worker-local
	// interface, living in a third package.
	runCmd(t, projectDir, forgeBin, "add", "package", "marketfeed", "--type", "adapter")
	writeCorpusFile(t, filepath.Join(projectDir, "internal", "marketfeed", "feed.go"), `package marketfeed

import "context"

// Adapter satisfies settlement_processor.UnsettledSource without ever
// naming it — assignability must be proven cross-package, in a single
// type universe.
type Adapter struct{}

// NewAdapter builds the adapter.
func NewAdapter() *Adapter { return &Adapter{} }

// Pending returns the open settlement candidates.
func (*Adapter) Pending(_ context.Context) ([]string, error) {
	return []string{"mkt-1"}, nil
}
`)

	// AppExtras + setup wiring.
	writeCorpusFile(t, filepath.Join(projectDir, "pkg", "app", "app_extras.go"), `// app_extras.go is YOUR code — forge generate will never overwrite it.
//
//forge:scaffold one-shot
//forge:allow
package app

import "example.com/kalshishape/internal/marketfeed"

// AppExtras is the user-owned extension surface for *App.
type AppExtras struct {
	// Unsettled is the CONCRETE adapter; the settlement_processor
	// worker consumes it as its worker-local UnsettledSource.
	Unsettled *marketfeed.Adapter
}
`)
	mustReplaceFirstInFile(t, filepath.Join(projectDir, "pkg", "app", "setup.go"),
		"\n\treturn nil\n}",
		"\n\tapp.Unsettled = marketfeed.NewAdapter()\n\treturn nil\n}")
	mustReplaceFirstInFile(t, filepath.Join(projectDir, "pkg", "app", "setup.go"),
		"\t\"example.com/kalshishape/pkg/config\"",
		"\t\"example.com/kalshishape/internal/marketfeed\"\n\t\"example.com/kalshishape/pkg/config\"")

	// ── 1. generate ×2 — idempotency ──────────────────────────────────
	generateTwiceIdempotent(t, forgeBin, projectDir)

	// ── 2. the kalshi pin: optional worker-local interface dep must be
	// wired to app.Unsettled, never silently nil ──────────────────────
	wireGen := readFileE2E(t, filepath.Join(projectDir, "pkg", "app", "wire_gen.go"))
	assertFieldWired(t, "wire_gen.go", wireGen, "Unsettled")
	// All three snake_case workers must have wire functions.
	for _, fn := range []string{"wireWorkerEngineShadowDeps", "wireWorkerSettlementProcessorDeps", "wireWorkerBookSnapshotterDeps"} {
		if !strings.Contains(wireGen, fn) {
			t.Errorf("wire_gen.go missing %s — snake_case worker not wired", fn)
		}
	}
	// Cron worker scaffold must carry its schedule.
	cronWorker := readFileE2E(t, filepath.Join(projectDir, "workers", "book_snapshotter", "worker.go"))
	if !strings.Contains(cronWorker, "0 3 * * *") {
		t.Errorf("book_snapshotter worker.go does not carry the cron schedule")
	}

	// ── 3. compiles ───────────────────────────────────────────────────
	runCmd(t, projectDir, "go", "build", "./...")

	// ── 4. boots with all workers registered; clean shutdown ─────────
	bootHealthzAndShutdown(t, projectDir)

	// ── 5. disown lifecycle round-trip ────────────────────────────────
	disownRoundTrip(t, forgeBin, projectDir, "pkg/app/wire_gen.go")

	t.Logf("kalshi-shaped fixture total: %s", time.Since(start))
}

// ─────────────────── fixture 3: frontend-basepath-shaped ───────────────────

// TestE2EFixtureCorpusFrontendBasePath is the first corpus fixture that
// renders a FRONTEND — template features without an end-to-end fixture
// are exactly how frontend regressions ship. It pins the
// frontends[].base_path feature end to end:
//
//	render level (always, no node needed):
//	  - forge.yaml persists base_path (it drives regeneration);
//	  - next.config.ts sources basePath from NEXT_PUBLIC_BASE_PATH with
//	    the "/admin" literal default, emits basePath AND assetPrefix
//	    (same value), and carries the static-branch fail-loud guard;
//	  - src/lib/basepath_gen.ts exists, is Tier-1-certified (verifying
//	    embedded forge:hash marker), exports BASE_PATH defaulting to
//	    "/admin" and an idempotent joinBasePath;
//	  - generated src/ TS/TSX (nav_gen, dashboard_gen, hooks, mocks,
//	    pages) contains NO hand-prefixed "/admin" string literals —
//	    Link/router handle basePath; bare literals double-prefix or go
//	    stale the day the mount point moves.
//
//	build level (gated: skipped with a logged reason when npm is not on
//	PATH or FORGE_E2E_SKIP_NPM is set — CI runs it, laptops may not):
//	  - `npm run build` with NEXT_PUBLIC_BASE_PATH explicitly "" must
//	    FAIL with the template's refusal message (a static export would
//	    bake root-mounted URLs that 404 behind the proxy);
//	  - `npm run build` with NEXT_PUBLIC_BASE_PATH=/admin must pass and
//	    the exported HTML must reference /admin/_next assets — the
//	    field-reported hydration bug class (chunk URLs skipping the
//	    prefix → JS 404s → React never hydrates).
//
// The frontend is named "console" while the base path is "/admin" ON
// PURPOSE: no path under frontends/console/ contains the substring
// "/admin", so any "/admin" literal found in generated src/ is
// attributable to base-path machinery, not the directory layout.
func TestE2EFixtureCorpusFrontendBasePath(t *testing.T) {
	t.Parallel() // independent project in its own t.TempDir; binary shared via sync.Once
	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()
	start := time.Now()

	runCmd(t, dir, forgeBin, "new", "febp",
		"--mod", "example.com/febp",
		"--service", "api",
	)
	projectDir := filepath.Join(dir, "febp")
	addCorpusForgePkgReplace(t, projectDir)

	// The scaffold proto is used AS EMITTED — no renames. The scaffold
	// ships convention-named CRUD RPCs (CreateItem/GetItem/UpdateItem/
	// DeleteItem/ListItems), so this fixture pins the true out-of-box
	// experience: a fresh `forge new` project must yield a real "Item"
	// entity from frontend codegen (nav routes, hooks, mocks, entity
	// pages). Historically the scaffold used bare verbs (Create/Get/...)
	// which matched NO entity, and the very first generate produced a
	// hollow frontend; this fixture is the regression guard.

	// `forge add frontend --base-path` is the user-facing entry point
	// for the feature. When npm is on PATH the add also runs
	// `npm install` in the frontend (forge's own behavior), which lets
	// `forge generate` exercise the local protoc-gen-es TS-stub pass —
	// the same path a real user hits.
	//
	// `--output static` is the EXPLICIT opt-in this fixture needs: the
	// scaffold default is standalone (the static export fails `next
	// build` on generated dynamic [id] CRUD routes), but this project
	// is the legitimate static case — the unannotated scaffold emits NO
	// entity pages, and the fixture's build-level pins (fail-loud
	// base-path guard, /admin-prefixed out/ export) are static-branch
	// behavior.
	runCmd(t, projectDir, forgeBin, "add", "frontend", "console", "--base-path", "/admin", "--output", "static")
	feDir := filepath.Join(projectDir, "frontends", "console")
	assertPathExistsE2E(t, filepath.Join(feDir, "package.json"))

	if !strings.Contains(readFileE2E(t, filepath.Join(projectDir, "forge.yaml")), "base_path: /admin") {
		t.Fatalf("forge.yaml does not persist `base_path: /admin` for frontend console — generate would lose the prefix on the next run")
	}

	// ── 1. generate ×2 — idempotency. Covers basepath_gen.ts, nav/
	// dashboard/hooks/mocks/pages emission and (when node_modules
	// exists) the buf TS-stub pass. ───────────────────────────────────
	generateTwiceIdempotent(t, forgeBin, projectDir)

	// ── 2. render-level pins (no node required) ──────────────────────
	assertNextConfigBasePath(t, feDir, "/admin")
	assertBasePathGenHelper(t, projectDir, "frontends/console", "/admin")
	assertGeneratedSrcPrefixClean(t, feDir, "/admin")

	// Honest-routes pin (review F2): nav derives from the SAME entity
	// set that gates page emission. The scaffold's Item message carries
	// no (forge.v1.entity) annotation, so NO entity pages are emitted —
	// and nav_gen must therefore advertise NO routes. The old behavior
	// (nav claiming "/items" while no page existed) was the 404-wall
	// bug this pin now keeps dead. The annotated-entity frontend path
	// is exercised end-to-end by TestE2EScaffoldFrontendBuilds (one
	// CRUD entity, standalone default, npm build+test); the
	// static-export/dynamic-route conflict was resolved by making
	// standalone the scaffold default and static an explicit opt-in.
	navGen := readFileE2E(t, filepath.Join(feDir, "src", "components", "nav_gen.tsx"))
	if strings.Contains(navGen, `path: "/`) {
		t.Errorf("nav_gen.tsx advertises routes but the unannotated scaffold emits no entity pages — the F2 404-wall regression is back; got:\n%s", navGen)
	}
	// Non-vacuousness for the prefix-clean scan above: files that DO
	// exist must be present and app-relative (basepath_gen carries the
	// only legitimate prefix literal; hooks must exist for the service).
	assertPathExistsE2E(t, filepath.Join(feDir, "src", "hooks", "api-service-hooks.ts"))
	assertPathExistsE2E(t, filepath.Join(feDir, "src", "lib", "basepath_gen.ts"))

	// Go-side compile/boot is pinned by fixtures 1–2; this fixture owns
	// the frontend surface, so no `go build` here (runtime budget).
	t.Logf("frontend-basepath fixture render phase: %s", time.Since(start))

	// ── 3. build-level pins (npm-gated) ──────────────────────────────
	if reason := corpusNpmSkipReason(); reason != "" {
		t.Logf("SKIPPING npm build phase: %s", reason)
		t.Logf("frontend-basepath fixture total: %s", time.Since(start))
		return
	}
	npmStart := time.Now()

	// `forge add frontend` already ran an install; re-running is a fast
	// no-op that also covers the npm-was-missing-at-add-time case. Same
	// flags as the scaffold-frontend e2e (forge's canonical install).
	runCmdTimeout(t, feDir, 5*time.Minute,
		"npm", "install", "--no-audit", "--no-fund", "--prefer-offline")

	// 3a. Fail-loud guard FIRST (near-instant: the throw happens while
	// next.config.ts is evaluated, before any compilation).
	out, err := runCorpusCmdEnv(feDir, 2*time.Minute,
		[]string{"NEXT_PUBLIC_BASE_PATH="}, "npm", "run", "build")
	if err == nil {
		t.Fatalf("npm run build with NEXT_PUBLIC_BASE_PATH=\"\" must FAIL (static-export fail-loud guard); output:\n%s", out)
	}
	if !strings.Contains(out, "refusing to bake a root-mounted static export") {
		t.Errorf("empty-override build failed, but without the template's fail-loud message; output:\n%s", out)
	}

	// 3b. The real build, prefix explicit. The package.json build script
	// sets NODE_ENV=production, so the static branch emits out/.
	out, err = runCorpusCmdEnv(feDir, 8*time.Minute,
		[]string{"NEXT_PUBLIC_BASE_PATH=/admin"}, "npm", "run", "build")
	if err != nil {
		t.Fatalf("npm run build (NEXT_PUBLIC_BASE_PATH=/admin) failed: %v\n%s", err, out)
	}

	// 3c. The export must reference /admin-prefixed assets.
	assertStaticExportPrefixed(t, feDir, "/admin")

	t.Logf("frontend-basepath fixture npm phase: %s", time.Since(npmStart))
	t.Logf("frontend-basepath fixture total: %s", time.Since(start))
}

// assertNextConfigBasePath pins the rendered next.config.ts contract for
// a declared base_path: the single canonical env var with the forge.yaml
// literal as default, basePath AND assetPrefix from the same value, and
// the static-branch fail-loud guard for empty overrides.
func assertNextConfigBasePath(t *testing.T, feDir, basePath string) {
	t.Helper()
	cfg := readFileE2E(t, filepath.Join(feDir, "next.config.ts"))

	wantConst := fmt.Sprintf("const basePath = process.env.NEXT_PUBLIC_BASE_PATH ?? %q;", basePath)
	if !strings.Contains(cfg, wantConst) {
		t.Errorf("next.config.ts must source basePath from NEXT_PUBLIC_BASE_PATH with the %q literal default; want %q in:\n%s", basePath, wantConst, cfg)
	}
	wantSpread := "...(basePath ? { basePath, assetPrefix: basePath } : {}),"
	if !strings.Contains(cfg, wantSpread) {
		t.Errorf("next.config.ts must emit basePath AND assetPrefix (same value) — omitting assetPrefix lets chunk URLs skip the prefix and hydration dies; want %q in:\n%s", wantSpread, cfg)
	}
	// Static output (this fixture's explicit opt-in) must refuse to
	// bake a root-mounted export when the override empties the prefix.
	if !strings.Contains(cfg, `process.env.NODE_ENV === "production" && basePath === ""`) ||
		!strings.Contains(cfg, "throw new Error") {
		t.Errorf("next.config.ts (static output) must carry the fail-loud empty-basePath production guard; got:\n%s", cfg)
	}
}

// assertBasePathGenHelper pins the generated src/lib/basepath_gen.ts:
// exists, exports BASE_PATH (env override, declared default) and an
// idempotent joinBasePath, and is Tier-1-certified via its embedded
// forge:hash marker (so hand edits trip the stomp guard and
// `forge generate` keeps regenerating it when base_path changes).
func assertBasePathGenHelper(t *testing.T, projectDir, feRel, basePath string) {
	t.Helper()
	rel := feRel + "/src/lib/basepath_gen.ts"
	bpPath := filepath.Join(projectDir, filepath.FromSlash(rel))
	assertPathExistsE2E(t, bpPath)
	bp := readFileE2E(t, bpPath)

	if !strings.Contains(bp, fmt.Sprintf("process.env.NEXT_PUBLIC_BASE_PATH ?? %q", basePath)) {
		t.Errorf("basepath_gen.ts must default BASE_PATH to the declared %q with NEXT_PUBLIC_BASE_PATH as the only override; got:\n%s", basePath, bp)
	}
	if !strings.Contains(bp, "export const BASE_PATH") {
		t.Errorf("basepath_gen.ts must export BASE_PATH; got:\n%s", bp)
	}
	if !strings.Contains(bp, "export function joinBasePath") {
		t.Errorf("basepath_gen.ts must export joinBasePath; got:\n%s", bp)
	}
	// The idempotency clause: an already-prefixed path passes through
	// unchanged, so accidental double-wrapping never double-prefixes.
	if !strings.Contains(bp, "startsWith(`${BASE_PATH}/`)") {
		t.Errorf("joinBasePath must be idempotent (already-prefixed paths returned unchanged); got:\n%s", bp)
	}

	// Tier-1 certification is embedded in the file itself now (the
	// global manifest is dead): the forge:hash marker must be present
	// and verify against the recomputed body hash.
	if got := checksums.Verify([]byte(bp)); got != checksums.Pristine {
		t.Errorf("%s Verify = %v, want Pristine — without a verifying embedded forge:hash marker, hand edits would never trip the Tier-1 stomp guard", rel, got)
	}
}

// assertGeneratedSrcPrefixClean walks the frontend's src/ tree and fails
// on any TS/TSX string literal starting with the base path. Next.js
// <Link> and the router prepend the configured basePath automatically,
// and hand-built URLs go through joinBasePath — a bare "/admin..."
// literal in generated code is the double-prefix / stale-mount bug
// class. basepath_gen.ts is exempt: it legitimately carries the literal
// as its baked default.
func assertGeneratedSrcPrefixClean(t *testing.T, feDir, basePath string) {
	t.Helper()
	srcDir := filepath.Join(feDir, "src")
	litRe := regexp.MustCompile("[\"'`]" + regexp.QuoteMeta(basePath) + `\b`)
	exempt := filepath.ToSlash(filepath.Join("lib", "basepath_gen.ts"))

	scanned := 0
	err := filepath.WalkDir(srcDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		switch filepath.Ext(path) {
		case ".ts", ".tsx", ".js", ".jsx":
		default:
			return nil
		}
		rel, rerr := filepath.Rel(srcDir, path)
		if rerr != nil {
			return rerr
		}
		if filepath.ToSlash(rel) == exempt {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		scanned++
		if loc := litRe.Find(body); loc != nil {
			t.Errorf("src/%s contains a hand-prefixed %q string literal (%q) — Link/router and joinBasePath own the prefix; bare literals double-prefix or go stale when the mount point moves", filepath.ToSlash(rel), basePath, string(loc))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan %s: %v", srcDir, err)
	}
	if scanned == 0 {
		t.Fatalf("prefix-clean scan saw zero TS/TSX files under %s — scaffold shape drifted", srcDir)
	}
}

// assertStaticExportPrefixed checks the static-export output (out/) for
// /admin-prefixed asset URLs: at least one exported HTML document must
// reference <basePath>/_next. This is the field-reported hydration bug
// class — chunk/asset URLs that skip the prefix 404 behind the proxy
// and React never hydrates.
func assertStaticExportPrefixed(t *testing.T, feDir, basePath string) {
	t.Helper()
	outDir := filepath.Join(feDir, "out")
	assertPathExistsE2E(t, outDir)

	htmlSeen := 0
	prefixed := false
	err := filepath.WalkDir(outDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if filepath.Ext(path) != ".html" {
			return nil
		}
		htmlSeen++
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(body), basePath+"/_next") {
			prefixed = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan static export %s: %v", outDir, err)
	}
	if htmlSeen == 0 {
		t.Fatalf("static export at %s contains no HTML files — `output: export` branch did not run", outDir)
	}
	if !prefixed {
		t.Errorf("no exported HTML references %s/_next — assets are root-mounted and would 404 behind the proxy (hydration bug class)", basePath)
	}
}

// corpusNpmSkipReason returns a non-empty human-readable reason when the
// npm build phase should be skipped: explicit env opt-out (laptops) or
// npm missing from PATH. CI provisions node and runs the full phase.
func corpusNpmSkipReason() string {
	if os.Getenv("FORGE_E2E_SKIP_NPM") != "" {
		return "FORGE_E2E_SKIP_NPM is set"
	}
	if _, err := exec.LookPath("npm"); err != nil {
		return "npm not on PATH"
	}
	return ""
}

// runCorpusCmdEnv runs a command with extra environment entries and a
// hard timeout, returning combined output and the error (callers assert
// pass/fail themselves — the fail-loud guard EXPECTS a failure).
func runCorpusCmdEnv(dir string, timeout time.Duration, extraEnv []string, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), extraEnv...)
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return string(out), fmt.Errorf("timed out after %s: %w", timeout, ctx.Err())
	}
	return string(out), err
}

// ───────────────────── fixture 4: zero-service scaffold ─────────────────────

// TestE2EFixtureCorpusZeroService pins the bare-scaffold contract: a
// binary is a deployment unit, NOT a domain entity, so `forge new`
// without --service must invent NO service from the binary name (the
// cp-forge field report: an admin-server binary grew an
// admin_server.v1.AdminServerService with five generic CRUD RPCs, zero
// callers ever, permanent 501s in every audit).
//
// Asserts, in order:
//
//  1. Bare `forge new zerosvc` scaffolds zero services: forge.yaml has
//     `services: []`, no proto/services/zerosvc/, no handlers/zerosvc/,
//     no frontend (hence no nav route anywhere), and no file in the
//     tree mentions a zerosvc.v1 proto package.
//  2. `forge generate` is clean AND idempotent at zero services.
//  3. gen/mcp/manifest.json is ABSENT — mcp_gen's
//     no-file-for-zero-services semantics (the manifest's absence is
//     the "publishes zero tools" signal; an empty file would be a
//     weaker claim).
//  4. The zero-component appkit table compiles (`go build ./...`) and
//     BOOTS: /healthz 200, clean SIGTERM shutdown.
//  5. The documented first step actually works: `forge add service
//     item` on the same project → generate → build.
func TestE2EFixtureCorpusZeroService(t *testing.T) {
	t.Parallel() // independent project in its own t.TempDir; binary shared via sync.Once
	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()
	start := time.Now()

	// Bare scaffold: no --service. (--skip-tools: plugin install is
	// covered elsewhere; this fixture stays cheap.)
	runCmd(t, dir, forgeBin, "new", "zerosvc",
		"--mod", "example.com/zerosvc",
		"--skip-tools",
	)
	projectDir := filepath.Join(dir, "zerosvc")
	addCorpusForgePkgReplace(t, projectDir)

	// ── 1. zero-service shape: nothing named after the binary ────────
	assertZeroServiceShape(t, projectDir, "zerosvc")
	if !strings.Contains(readFileE2E(t, filepath.Join(projectDir, "forge.yaml")), "services: []") {
		t.Fatalf("bare scaffold forge.yaml must declare `services: []`")
	}

	// ── 2. generate ×2 — clean and idempotent at zero services ───────
	generateTwiceIdempotent(t, forgeBin, projectDir)

	// Still nothing invented post-generate (descriptor pass, frontend
	// pass, cleanup pass all ran).
	assertZeroServiceShape(t, projectDir, "zerosvc")

	// ── 3. mcp manifest absence semantics ────────────────────────────
	if _, err := os.Stat(filepath.Join(projectDir, "gen", "mcp", "manifest.json")); !os.IsNotExist(err) {
		t.Errorf("gen/mcp/manifest.json must NOT exist for a zero-service project (absence == publishes zero tools); stat err=%v", err)
	}

	// ── 4. compiles + boots: /healthz 200, clean SIGTERM ─────────────
	runCmd(t, projectDir, "go", "build", "./...")
	bootHealthzAndShutdown(t, projectDir)

	// ── 5. documented first step: forge add service item ─────────────
	// (`forge add service` runs the generate pipeline itself; the
	// explicit generate after it pins that a follow-up run is clean.)
	runCmd(t, projectDir, forgeBin, "add", "service", "item")
	assertPathExistsE2E(t, filepath.Join(projectDir, "proto", "services", "item", "v1", "item.proto"))
	assertPathExistsE2E(t, filepath.Join(projectDir, "handlers", "item", "service.go"))
	out := runCorpusCmdOK(t, projectDir, forgeBin, "generate")
	assertNoForkNoise(t, "post-add generate", out)
	runCmd(t, projectDir, "go", "build", "./...")

	t.Logf("zero-service fixture total: %s", time.Since(start))
}

// assertZeroServiceShape fails if any trace of an invented <name>
// service exists in the project: proto dir, handlers dir, frontend nav
// route, or a `services.<name>.v1` proto-package reference in any
// non-vendored file.
func assertZeroServiceShape(t *testing.T, projectDir, name string) {
	t.Helper()
	for _, p := range []string{
		filepath.Join("proto", "services", name),
		filepath.Join("handlers", name),
		filepath.Join("gen", "services", name),
		"frontends", // bare scaffold has no frontend → no nav route can exist
	} {
		if _, err := os.Stat(filepath.Join(projectDir, p)); !os.IsNotExist(err) {
			t.Errorf("bare scaffold must not create %s (stat err=%v)", p, err)
		}
	}
	// Sweep the tree for the invented proto package. Skips .forge-pkg
	// (vendored library) and git/npm machinery.
	needle := "services." + name + ".v1"
	err := filepath.WalkDir(projectDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if corpusSkipDir(d.Name()) || d.Name() == ".forge-pkg" {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil || info.Size() > 1<<20 {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if strings.Contains(string(body), needle) {
			rel, _ := filepath.Rel(projectDir, path)
			t.Errorf("%s references %q — a %s service was invented from the binary name", rel, needle, name)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("zero-service tree sweep: %v", err)
	}
}

// ───────────────────────────── corpus helpers ─────────────────────────────

// addCorpusForgePkgReplace wires the two unpublished forge modules a
// scaffolded project imports to local sources, in BOTH module roots
// (project + gen/) so `go mod tidy` resolves on either side:
//
//   - github.com/reliant-labs/forge/pkg → <repo>/pkg (appkit,
//     serverkit, orm — revisions newer than any published snapshot).
//   - github.com/reliant-labs/forge/gen → a local shim module built by
//     the harness. The scaffolded proto/forge/v1/forge.proto keeps the
//     PUBLISHED go_package (github.com/reliant-labs/forge/gen/forge/v1)
//     so generated config code imports that module — which does not
//     exist on the proxy yet. The shim is a one-package module wrapping
//     the in-repo internal/gen/forge/v1/forge.pb.go.
func addCorpusForgePkgReplace(t *testing.T, projectDir string) {
	t.Helper()
	repoRoot := findRepoRoot(t)
	genShim := buildForgeGenShim(t, repoRoot)

	// Pre-vendor forge/pkg into <project>/.forge-pkg — the canonical
	// state `forge generate`'s vendor-sync would converge to anyway.
	// Doing it up front (a) keeps the replace target relative so the
	// workspace never sees conflicting absolute/vendored targets, and
	// (b) keeps generate run 1 vs run 2 byte-identical for the tree-
	// hash idempotency assertion (no mid-run go.mod rewrite).
	vendorCorpusForgePkg(t, repoRoot, projectDir)

	// Root module: vendored pkg replace + gen-shim replace.
	addReplaceLines(t, filepath.Join(projectDir, "go.mod"),
		"replace github.com/reliant-labs/forge/pkg => ./.forge-pkg",
		fmt.Sprintf("replace github.com/reliant-labs/forge/gen => %s", genShim),
	)
	// gen module: only the gen-shim replace (generated proto/config Go
	// imports github.com/reliant-labs/forge/gen/forge/v1).
	if _, err := os.Stat(filepath.Join(projectDir, "gen", "go.mod")); err == nil {
		addReplaceLines(t, filepath.Join(projectDir, "gen", "go.mod"),
			fmt.Sprintf("replace github.com/reliant-labs/forge/gen => %s", genShim),
		)
	}
}

// addReplaceLines appends each replace directive to the go.mod at path
// unless a replace for the same module is already present.
func addReplaceLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	content := string(data)
	for _, line := range lines {
		modPath := strings.Fields(line)[1]
		if strings.Contains(content, "replace "+modPath+" ") {
			continue
		}
		content += "\n" + line + "\n"
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// vendorCorpusForgePkg copies <repo>/pkg into <project>/.forge-pkg,
// mirroring forge's own vendor-sync (which skips .git/ and testdata/).
func vendorCorpusForgePkg(t *testing.T, repoRoot, projectDir string) {
	t.Helper()
	src := filepath.Join(repoRoot, "pkg")
	dst := filepath.Join(projectDir, ".forge-pkg")
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, path)
		if rerr != nil {
			return rerr
		}
		if d.IsDir() {
			if d.Name() == ".git" || d.Name() == "testdata" {
				return filepath.SkipDir
			}
			return os.MkdirAll(filepath.Join(dst, rel), 0o755)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		return os.WriteFile(filepath.Join(dst, rel), data, 0o644)
	})
	if err != nil {
		t.Fatalf("vendor forge/pkg into %s: %v", dst, err)
	}
}

// buildForgeGenShim materializes a local stand-in for the unpublished
// github.com/reliant-labs/forge/gen module: go.mod + forge/v1/forge.pb.go
// (copied from the repo's internal/gen — the source of truth the
// embedded forge.proto was generated from). Returns the module dir.
func buildForgeGenShim(t *testing.T, repoRoot string) string {
	t.Helper()
	shim := filepath.Join(t.TempDir(), "forge-gen-shim")
	pbSrc := filepath.Join(repoRoot, "internal", "gen", "forge", "v1", "forge.pb.go")
	pb, err := os.ReadFile(pbSrc)
	if err != nil {
		t.Fatalf("read %s (needed for the forge/gen shim): %v", pbSrc, err)
	}
	writeCorpusFile(t, filepath.Join(shim, "go.mod"), `module github.com/reliant-labs/forge/gen

go 1.23

require google.golang.org/protobuf v1.36.9
`)
	writeCorpusFile(t, filepath.Join(shim, "forge", "v1", "forge.pb.go"), string(pb))
	return shim
}

// generateTwiceIdempotent runs `forge generate` twice and asserts the
// second run (a) succeeds, (b) changes ZERO files (full tree hash
// compare), and (c) emits zero ownership-machinery warnings.
// Idempotency is a first-class assertion: the kalshi matcher bug
// manifested as regen output flip-flopping between app.<Field> and nil
// across runs.
func generateTwiceIdempotent(t *testing.T, forgeBin, projectDir string) {
	t.Helper()

	out1 := runCorpusCmdOK(t, projectDir, forgeBin, "generate")
	assertNoForkNoise(t, "generate run 1", out1)

	before := hashProjectTree(t, projectDir)
	beforeContent := snapshotSmallFiles(t, projectDir)
	out2 := runCorpusCmdOK(t, projectDir, forgeBin, "generate")
	assertNoForkNoise(t, "generate run 2", out2)
	after := hashProjectTree(t, projectDir)

	var diffs []string
	for path, h := range after {
		if before[path] != h {
			diffs = append(diffs, path)
		}
	}
	for path := range before {
		if _, ok := after[path]; !ok {
			diffs = append(diffs, path+" (deleted)")
		}
	}
	sort.Strings(diffs)
	if len(diffs) > 0 {
		var detail strings.Builder
		for _, p := range diffs {
			old, ok := beforeContent[p]
			if !ok {
				continue
			}
			cur, err := os.ReadFile(filepath.Join(projectDir, p))
			if err != nil {
				continue
			}
			fmt.Fprintf(&detail, "\n--- %s (run1 → run2) ---\n%s", p, corpusLineDiff(old, string(cur), 40))
		}
		t.Fatalf("second `forge generate` is not idempotent — %d file(s) changed:\n  %s\n%s",
			len(diffs), strings.Join(diffs, "\n  "), detail.String())
	}
}

// snapshotSmallFiles captures the content of textual files under 1MB so
// an idempotency failure can print a real diff instead of just paths.
func snapshotSmallFiles(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			if d != nil && d.IsDir() && corpusSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		info, ierr := d.Info()
		if ierr != nil || info.Size() > 1<<20 {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		out[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	return out
}

// corpusLineDiff is a crude unified-ish diff: lines present in only one
// of the two versions, capped at maxLines.
func corpusLineDiff(a, b string, maxLines int) string {
	aLines := strings.Split(a, "\n")
	bLines := strings.Split(b, "\n")
	aSet := map[string]int{}
	for _, l := range aLines {
		aSet[l]++
	}
	bSet := map[string]int{}
	for _, l := range bLines {
		bSet[l]++
	}
	var out []string
	for _, l := range aLines {
		if bSet[l] == 0 {
			out = append(out, "- "+l)
		}
	}
	for _, l := range bLines {
		if aSet[l] == 0 {
			out = append(out, "+ "+l)
		}
	}
	if len(out) > maxLines {
		out = append(out[:maxLines], fmt.Sprintf("… (%d more)", len(out)-maxLines))
	}
	if len(out) == 0 {
		return "(line-set identical — ordering or count difference)"
	}
	return strings.Join(out, "\n")
}

// assertNoForkNoise fails when a clean generate run prints ownership-
// machinery output: fork-era warnings (must never appear again) or
// disown/migration lines (must not appear on a clean tree).
func assertNoForkNoise(t *testing.T, label, out string) {
	t.Helper()
	for _, needle := range []string{"forked file(s)", "fork-coherence", "now forks", "disowned", "legacy forked"} {
		if strings.Contains(out, needle) {
			t.Errorf("%s: unexpected ownership-machinery output (%q) in:\n%s", label, needle, out)
		}
	}
}

// assertFieldWired pins the deps-matcher fix at the grep level: the
// name-matched Deps field must wire `Field: app.Field` and must NOT be
// a silent typed-zero (`Field: nil`) or carry an unresolved TODO.
// gofmt column-aligns struct-literal values, so match `Field:` followed
// by any run of spaces.
func assertFieldWired(t *testing.T, file, content, field string) {
	t.Helper()
	wired := regexp.MustCompile(regexp.QuoteMeta(field) + `: +app\.` + regexp.QuoteMeta(field))
	if !wired.MatchString(content) {
		t.Errorf("%s: missing `%s: app.%s` — name-matched dep was not wired", file, field, field)
	}
	nilWire := regexp.MustCompile(regexp.QuoteMeta(field) + `: +nil`)
	if nilWire.MatchString(content) {
		t.Errorf("%s: contains silent `%s: nil` — the deps-matcher downgrade bug class", file, field)
	}
	if strings.Contains(content, "TODO: wire "+field) {
		t.Errorf("%s: contains `TODO: wire %s` — field should have resolved", file, field)
	}
}

// corpusSkipDir reports whether a directory is excluded from the
// idempotency tree hash / content snapshot. `.git` is repo machinery;
// `node_modules` and `.next` are npm/Next-owned trees `forge generate`
// never writes — hashing them (tens of thousands of files once the
// frontend fixture installs dependencies) would dominate runtime and,
// for the content snapshot, memory, without guarding anything.
func corpusSkipDir(name string) bool {
	return name == ".git" || name == "node_modules" || name == ".next"
}

// hashProjectTree walks the project and returns rel-path → sha256 for
// every regular file. Nothing forge-owned is excluded: .forge state files,
// side renders, go.sum — a second generate must leave ALL of it
// untouched. (Only non-forge trees are skipped; see corpusSkipDir.)
func hashProjectTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if corpusSkipDir(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		sum := sha256.Sum256(data)
		out[filepath.ToSlash(rel)] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatalf("hash tree %s: %v", root, err)
	}
	return out
}

// bootHealthzAndShutdown builds the server binary, boots it, verifies
// /healthz returns 200, then SIGTERMs it and requires a clean exit
// within a bounded wait — the appkit-merge boot recipe.
//
// The port is allocated via freePortE2E (scaffold_e2e_test.go): the
// corpus runs t.Parallel(), so a hard-coded per-fixture port would be a
// collision against any other test (or stray process) on the machine.
func bootHealthzAndShutdown(t *testing.T, projectDir string, extraEnv ...string) {
	t.Helper()
	port := freePortE2E(t)

	serverBin := filepath.Join(projectDir, "corpus-server")
	runCmd(t, projectDir, "go", "build", "-o", serverBin, "./cmd/...")

	cmd := exec.Command(serverBin, "server")
	cmd.Dir = projectDir
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("PORT=%d", port),
		"DATABASE_URL=",           // health checks must not need a DB (no-entity projects)
		"ENVIRONMENT=development", // dev authorizer; no real authz backend
		"AUTH_MODE=none",          // explicit no-auth (NewAuthInterceptor errors at startup otherwise)
	)
	// Caller overrides REPLACE the defaults (duplicate-env behavior is
	// platform-dependent, so force-rewrite instead of appending) — the
	// CRUD-lifecycle fixture boots WITH a real sqlite DATABASE_URL to
	// prove the generated bootstrap constructs the DB/ORM pair from
	// config.
	for _, kv := range extraEnv {
		if k, v, ok := strings.Cut(kv, "="); ok {
			cmd.Env = withForcedEnv(cmd.Env, k, v)
		}
	}
	var serverOut strings.Builder
	cmd.Stdout = &serverOut
	cmd.Stderr = &serverOut
	if err := cmd.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	killed := false
	defer func() {
		if !killed {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	if !waitForServer(t, base+"/healthz", 15*time.Second) {
		t.Fatalf("server did not become ready\noutput:\n%s", serverOut.String())
	}
	resp, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz = %d, want 200\noutput:\n%s", resp.StatusCode, serverOut.String())
	}

	// Clean shutdown: SIGTERM, bounded wait.
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		killed = true
		if err != nil {
			t.Fatalf("server did not shut down cleanly on SIGTERM: %v\noutput:\n%s", err, serverOut.String())
		}
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		killed = true
		t.Fatalf("server did not exit within 20s of SIGTERM\noutput:\n%s", serverOut.String())
	}
	_ = os.Remove(serverBin)
}

// disownRoundTrip exercises the full ownership lifecycle on ONE
// Tier-1 generated file (rel, e.g. pkg/app/wire_gen.go or
// internal/db/item_orm.go):
//
//	hand-edit → generate (drift error, new option text) →
//	`forge disown --reason` (one-way transfer) → generate leaves the
//	file alone with ZERO warnings → delete + generate re-adopts to the
//	pristine render (entry back to Tier-1).
func disownRoundTrip(t *testing.T, forgeBin, projectDir, rel string) {
	t.Helper()
	wireGenPath := filepath.Join(projectDir, rel)
	pristine := readFileE2E(t, wireGenPath)

	// Hand-edit: append a user marker.
	const marker = "func userDisownMarker() {}"
	appendCorpusFile(t, wireGenPath, "\n// user edit — must survive every generate after disown\n"+marker+"\n")

	// 1. Plain generate must trip the Tier-1 stomp guard, and the error
	// must teach the new option set: extension point first, then
	// --explain-drift / --force / friction, with `forge disown --reason`
	// as the explicit last-resort one-way door. No fork-era guidance.
	out, err := runCorpusCmd(projectDir, forgeBin, "generate")
	if err == nil {
		t.Fatalf("generate over a hand-edited Tier-1 file must fail (drift guard); output:\n%s", out)
	}
	if !strings.Contains(out, "Tier-1") || !strings.Contains(out, rel) {
		t.Fatalf("drift error should name the Tier-1 guard and %s; got:\n%s", rel, out)
	}
	for _, want := range []string{
		"extension point",
		"--explain-drift",
		"forge friction add",
		"forge disown <path> --reason",
		"ONE-WAY",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("drift error missing option text %q; got:\n%s", want, out)
		}
	}
	// The old report taught the fork escape hatch; that guidance must be
	// gone. (Needles are the old option-text phrases, not bare flag
	// names — cobra's usage dump legitimately lists the deprecated
	// --accept flag for one release.)
	for _, stale := range []string{"Re-run with `--accept`", "unfork --merge", "fork the file"} {
		if strings.Contains(out, stale) {
			t.Errorf("drift error still teaches fork-era escape hatch %q; got:\n%s", stale, out)
		}
	}

	// 2. Disowning requires a reason — refuse without one.
	out, err = runCorpusCmd(projectDir, forgeBin, "disown", rel)
	if err == nil {
		t.Fatalf("disown without --reason must refuse; output:\n%s", out)
	}
	if !strings.Contains(out, "--reason is required") {
		t.Errorf("reason-less disown should explain the requirement; got:\n%s", out)
	}

	// 3. `forge disown --reason`: the one-way transfer. Confirms the
	// path and documents the delete + generate re-adoption flow.
	out = runCorpusCmdOK(t, projectDir, forgeBin, "disown", rel,
		"--reason", "corpus e2e: custom wiring the generated file can't express")
	if !strings.Contains(out, "disowned "+rel) {
		t.Errorf("disown should confirm the path; got:\n%s", out)
	}
	if !strings.Contains(out, "delete it and run `forge generate`") {
		t.Errorf("disown should document the re-adoption path; got:\n%s", out)
	}

	// 4. Generate now leaves the file alone FOREVER — clean exit, the
	// user content survives, and there is no warning noise of any kind
	// (disowned files are a legitimate end state, not a nag target).
	// No side renders are parked either: there is no reconcile-later
	// limbo.
	for run := 1; run <= 2; run++ {
		out = runCorpusCmdOK(t, projectDir, forgeBin, "generate")
		assertNoForkNoise(t, fmt.Sprintf("post-disown generate run %d", run), out)
		if got := readFileE2E(t, wireGenPath); !strings.Contains(got, marker) {
			t.Fatalf("disowned %s was overwritten by generate run %d — disown not honored", rel, run)
		}
	}
	if _, err := os.Stat(filepath.Join(projectDir, ".forge", "render", rel)); !os.IsNotExist(err) {
		t.Errorf("side render parked for a disowned file — the reconcile-later limbo should be gone (stat err=%v)", err)
	}

	// 5. Re-adoption: delete the file, run generate. The emitter
	// re-emits the pristine render and the entry returns to Tier-1.
	if err := os.Remove(wireGenPath); err != nil {
		t.Fatalf("delete disowned file: %v", err)
	}
	out = runCorpusCmdOK(t, projectDir, forgeBin, "generate")
	assertNoForkNoise(t, "re-adoption generate", out)
	readopted := readFileE2E(t, wireGenPath)
	if readopted != pristine {
		t.Errorf("re-adoption generate did not restore the pristine render of %s", rel)
	}
	// Tier-1 ownership is embedded in the file now: the re-emitted
	// render must carry a verifying forge:hash marker, and the disowned
	// record must be gone from .forge/disowned.json (the file itself is
	// deleted when the last record clears).
	if got := checksums.Verify([]byte(readopted)); got != checksums.Pristine {
		t.Errorf("%s Verify = %v after re-adoption, want Pristine (re-certified Tier-1)", rel, got)
	}
	if raw, err := os.ReadFile(filepath.Join(projectDir, checksums.DisownedFile)); err == nil {
		var disowned struct {
			Files map[string]any `json:"files"`
		}
		if jerr := json.Unmarshal(raw, &disowned); jerr != nil {
			t.Fatalf("parse %s: %v", checksums.DisownedFile, jerr)
		}
		if _, still := disowned.Files[rel]; still {
			t.Errorf("%s still recorded in %s after re-adoption", rel, checksums.DisownedFile)
		}
	}
}

// ─────────────────── fixture 5: executed CRUD lifecycle ───────────────────

// crudLifecycleProbeSrc is the in-project probe test the CRUD-lifecycle
// fixture writes into handlers/item/ and executes with the project's own
// `go test`. It drives the REAL generated stack — generated CRUD wiring →
// pkg/crud lifecycle → internal/db ORM → real SQLite — against the
// project's OWN migration SQL (db/migrations/*.up.sql applied verbatim),
// with claims attached the same way the generated middleware would.
//
// Every assertion is a real semantic, not AnyOutcome:
//
//	create×2     → two rows, distinct non-empty IDs, created_at set
//	get          → returns the created row
//	list         → both rows visible
//	update       → actually mutates (re-read observes the change)
//	delete → get → NotFound with a CLEAN client message (no SQL text)
//	list+search  → filter finds the matching row (never silently empty,
//	               never an Internal error from a phantom column)
//	order_by     → declared column accepted; undeclared column rejected
//	               as InvalidArgument (allowlist, not identifier-shape)
//
// This is exactly the bug class the 2026-06 generated-output review
// found shipping: forge validated template RENDERING, never executed
// behavior.
const crudLifecycleProbeSrc = `package item_test

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/reliant-labs/forge/pkg/orm"
	"github.com/reliant-labs/forge/pkg/testkit"

	pb "example.com/crudlife/gen/services/item/v1"
	"example.com/crudlife/pkg/app"
	"example.com/crudlife/pkg/middleware"
)

// corpusMigratedDB builds an in-memory DB and applies the project's own
// migration SQL (db/migrations/*.up.sql, in order) — the schema the
// generated CRUD code is supposed to run against.
func corpusMigratedDB(t *testing.T) orm.Context {
	t.Helper()
	db := testkit.NewSQLiteMemDB(t)
	migDir := filepath.Join("..", "..", "db", "migrations")
	entries, err := os.ReadDir(migDir)
	if err != nil {
		t.Fatalf("read project migrations dir: %v", err)
	}
	var ups []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
			ups = append(ups, e.Name())
		}
	}
	if len(ups) == 0 {
		t.Fatalf("project has no .up.sql migrations in %s", migDir)
	}
	sort.Strings(ups)
	for _, name := range ups {
		sql, rerr := os.ReadFile(filepath.Join(migDir, name))
		if rerr != nil {
			t.Fatalf("read migration %s: %v", name, rerr)
		}
		if _, xerr := db.Exec(context.Background(), string(sql)); xerr != nil {
			t.Fatalf("apply project migration %s: %v\nSQL:\n%s", name, xerr, sql)
		}
	}
	return db
}

func authedCtx() context.Context {
	return middleware.ContextWithClaims(context.Background(),
		&middleware.Claims{UserID: "corpus-user", Email: "corpus@example.com"})
}

// requireCleanClientError asserts an error surface fit for clients:
// the right code and NO leaked SQL/driver internals in the message.
func requireCleanClientError(t *testing.T, label string, err error, want connect.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected %v error, got nil", label, want)
	}
	if got := connect.CodeOf(err); got != want {
		t.Errorf("%s: code = %v, want %v (err: %v)", label, got, want, err)
	}
	msg := err.Error()
	for _, leak := range []string{"sql:", "SQL", "SELECT", "no rows in result set", "no such column"} {
		if strings.Contains(msg, leak) {
			t.Errorf("%s: client-visible error leaks driver/SQL text (%q): %s", label, leak, msg)
		}
	}
}

func TestCorpusCRUDLifecycle(t *testing.T) {
	db := corpusMigratedDB(t)
	svc := app.NewTestItem(t, app.WithDB(db))
	ctx := authedCtx()

	// ── create ×2: two rows, distinct non-empty IDs, created_at set ──
	r1, err := svc.CreateItem(ctx, connect.NewRequest(&pb.CreateItemRequest{Name: "first", Description: "alpha"}))
	if err != nil {
		t.Fatalf("create#1: %v", err)
	}
	id1 := r1.Msg.GetItem().GetId()
	if id1 == "" {
		t.Errorf("create#1 returned an EMPTY id — the generated create path never generates one")
	}
	if r1.Msg.GetItem().GetCreatedAt() == nil {
		t.Errorf("create#1: created_at is nil despite timestamps:true on the entity")
	}

	r2, err := svc.CreateItem(ctx, connect.NewRequest(&pb.CreateItemRequest{Name: "second", Description: "beta"}))
	if err != nil {
		t.Fatalf("create#2: %v", err)
	}
	id2 := r2.Msg.GetItem().GetId()
	if id2 == "" {
		t.Errorf("create#2 returned an EMPTY id")
	}
	if id1 == id2 {
		t.Errorf("create#1 and create#2 returned the SAME id (%q) — second create overwrote the first (upsert-as-create data loss)", id1)
	}

	lr, err := svc.ListItems(ctx, connect.NewRequest(&pb.ListItemsRequest{PageSize: 10}))
	if err != nil {
		t.Fatalf("list after two creates: %v", err)
	}
	if got := len(lr.Msg.GetItems()); got != 2 {
		t.Errorf("list after two creates: %d row(s), want 2 — creates are not inserts", got)
	}

	// ── get returns the created row ──────────────────────────────────
	if id1 != "" {
		gr, gerr := svc.GetItem(ctx, connect.NewRequest(&pb.GetItemRequest{Id: id1}))
		if gerr != nil {
			t.Fatalf("get created row: %v", gerr)
		}
		if gr.Msg.GetItem().GetName() != "first" {
			t.Errorf("get: name = %q, want %q", gr.Msg.GetItem().GetName(), "first")
		}

		// ── update actually mutates ──────────────────────────────────
		updated := gr.Msg.GetItem()
		updated.Name = "renamed"
		_, uerr := svc.UpdateItem(ctx, connect.NewRequest(&pb.UpdateItemRequest{Item: updated}))
		if uerr != nil {
			t.Fatalf("update: %v (the scaffold's own Update RPC must be wired, not a custom-read-shape stub)", uerr)
		}
		gr2, gerr2 := svc.GetItem(ctx, connect.NewRequest(&pb.GetItemRequest{Id: id1}))
		if gerr2 != nil {
			t.Fatalf("get after update: %v", gerr2)
		}
		if gr2.Msg.GetItem().GetName() != "renamed" {
			t.Errorf("update did not mutate: name = %q, want %q", gr2.Msg.GetItem().GetName(), "renamed")
		}
		if gr2.Msg.GetItem().GetCreatedAt() == nil {
			t.Errorf("update nulled created_at — timestamps must be managed, not round-tripped through the request")
		}

		// ── delete, then get-missing = clean NotFound ────────────────
		if _, derr := svc.DeleteItem(ctx, connect.NewRequest(&pb.DeleteItemRequest{Id: id1})); derr != nil {
			t.Fatalf("delete: %v", derr)
		}
		_, gerr3 := svc.GetItem(ctx, connect.NewRequest(&pb.GetItemRequest{Id: id1}))
		requireCleanClientError(t, "get-after-delete", gerr3, connect.CodeNotFound)

		// ── soft delete is REAL: the row survives in the database ────
		// deleted_at present means Delete is UPDATE ... SET deleted_at,
		// reads filter it out, and the data is still there. A hard
		// DELETE here means soft_delete was decorative again.
		var survivors int64
		row := db.QueryRow(ctx, "SELECT COUNT(*) FROM items WHERE id = ? AND deleted_at IS NOT NULL", id1)
		if scanErr := row.Scan(&survivors); scanErr != nil {
			t.Fatalf("count soft-deleted row: %v", scanErr)
		}
		if survivors != 1 {
			t.Errorf("soft_delete:true must keep the row with deleted_at set; rows with deleted_at NOT NULL = %d, want 1 (hard DELETE = decorative soft delete)", survivors)
		}

		lr2, lerr2 := svc.ListItems(ctx, connect.NewRequest(&pb.ListItemsRequest{PageSize: 10}))
		if lerr2 != nil {
			t.Fatalf("list after delete: %v", lerr2)
		}
		if got := len(lr2.Msg.GetItems()); got != 1 {
			t.Errorf("list after delete: %d row(s), want 1", got)
		}
	}

	// ── get a never-existed id = clean NotFound ──────────────────────
	_, merr := svc.GetItem(ctx, connect.NewRequest(&pb.GetItemRequest{Id: "corpus-never-existed"}))
	requireCleanClientError(t, "get-missing", merr, connect.CodeNotFound)

	// ── list with the scaffold's own search filter finds the row ─────
	search := "second"
	sr, serr := svc.ListItems(ctx, connect.NewRequest(&pb.ListItemsRequest{PageSize: 10, Search: &search}))
	if serr != nil {
		t.Fatalf("list with search=%q errored: %v — the generated filter maps to a phantom column instead of the entity's string columns", search, serr)
	}
	if got := len(sr.Msg.GetItems()); got != 1 {
		t.Errorf("list with search=%q: %d row(s), want exactly 1 — search must not silently return nothing", search, got)
	} else if sr.Msg.GetItems()[0].GetName() != "second" {
		t.Errorf("list with search=%q returned the wrong row: %q", search, sr.Msg.GetItems()[0].GetName())
	}

	// ── order_by: declared column accepted, undeclared rejected ──────
	if _, oerr := svc.ListItems(ctx, connect.NewRequest(&pb.ListItemsRequest{PageSize: 10, OrderBy: "name"})); oerr != nil {
		t.Errorf("list order_by=name (a declared column) errored: %v", oerr)
	}
	_, berr := svc.ListItems(ctx, connect.NewRequest(&pb.ListItemsRequest{PageSize: 10, OrderBy: "password_hash"}))
	requireCleanClientError(t, "list-orderby-undeclared", berr, connect.CodeInvalidArgument)
}
`

// TestE2EFixtureCorpusCRUDLifecycle is the executed-lifecycle gate: the
// first corpus fixture that asserts what generated CRUD code DOES, not
// what it renders as.
//
// Shape (the schema-is-truth flow — NO entity protos anywhere):
//
//  1. `forge new` scaffolds the Item service: CRUD RPCs + wire message,
//     no migration, no entity, no ORM. The schema starts empty.
//  2. `forge add entity item ...` emits the create-table migration —
//     SQL is the schema declaration. The scaffold proto already carries
//     the Item wire contract, so add-entity leaves it alone.
//  3. `forge generate` shadow-applies db/migrations, introspects, and
//     projects entity struct + ORM + CRUD wiring from the REAL columns.
//     Conventions come off the columns: deleted_at ⇒ soft delete,
//     created_at+updated_at ⇒ managed timestamps, text ⇒ searchable.
//  4. A second entity (bookmark, with a repeated []string field) is
//     born through the FULL `forge add entity` surface, including the
//     one-time schema→wire CRUD scaffold into the service proto.
//  5. A HAND-WRITTEN migration ladder evolves bookmarks (add column +
//     data movement splitting a column) and `forge generate` again —
//     the ORM must follow the applied schema with no proto change.
//
// Also pins the projection-vs-implementation split for CRUD:
//
//   - handlers_crud.go — the RPC implementations — is USER-OWNED from
//     line one (scaffold-once, no DO-NOT-EDIT banner) and THIN: it
//     never names an entity field, so schema changes never rot it.
//   - handlers_crud_ops_gen.go — the per-entity wiring (field mapping,
//     filters, proto<->struct conversion, packers) — stays Tier-1.
//   - handlers_crud_gen.go (the old Tier-1 implementation file) is DEAD
//     and must not be emitted.
//   - the scaffold's own proto must never trip the custom-read-shape stub
//     (the F2 root cause: the descriptor collapsed message-typed fields
//     to the literal string "message").
//   - the emitted migration guards the id invariant (CHECK (id <> ”))
//     and stores timestamps as TIMESTAMPTZ, not TEXT.
func TestE2EFixtureCorpusCRUDLifecycle(t *testing.T) {
	t.Parallel() // independent project in its own t.TempDir; binary shared via sync.Once
	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()
	start := time.Now()

	runCmd(t, dir, forgeBin, "new", "crudlife",
		"--mod", "example.com/crudlife",
		"--service", "item",
	)
	projectDir := filepath.Join(dir, "crudlife")
	addCorpusForgePkgReplace(t, projectDir)

	// ── 0. the schema starts EMPTY: no boilerplate migration, and a
	// pristine generate emits no entities (honest scaffold). ──────────
	if entries, err := os.ReadDir(filepath.Join(projectDir, "db", "migrations")); err == nil {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".sql") {
				t.Errorf("pristine scaffold ships migration %s — tables must be born via `forge add entity`", e.Name())
			}
		}
	}

	// Declare the entity: ONE command, SQL out. The wire contract (Item
	// message + CRUD RPCs) is already in the scaffold proto, which
	// add-entity detects and leaves alone.
	runCmd(t, projectDir, forgeBin, "add", "entity", "item",
		"name:string", "description:string", "active:bool", "--soft-delete")

	// sqlite driver: the boot step below starts the REAL server against a
	// real database file so the generated bootstrap's DB+ORM construction
	// is executed, not just rendered. (postgres would need a container;
	// the construction code path is identical modulo the driver import,
	// which TestGenerateBootstrap_ConstructsDatabaseAndORM pins for both.)
	// The scaffolded forge.yaml is MINIMAL (database: is a derived
	// default), so the swap is an explicit override APPEND, not a
	// string replace of a line that isn't there.
	appendToCorpusFile(t, filepath.Join(projectDir, "forge.yaml"),
		"\ndatabase:\n  driver: sqlite\n")

	// ── 1. generate ×2 — idempotent with an entity in play ───────────
	generateTwiceIdempotent(t, forgeBin, projectDir)

	// features.deploy is shape-derived for service projects: the scaffold
	// ships deploy/kcl/dev/main.k importing deploy.kcl.dev.config_gen, so
	// a pristine generate MUST emit that file or the scaffold's own KCL
	// import is unresolvable and `forge run` can't compose per-env config
	// (the J1 features.deploy catch-22: gate said deploy=false, schema
	// rejected features.deploy, main.k imported the never-generated file).
	if _, err := os.Stat(filepath.Join(projectDir, "deploy", "kcl", "dev", "config_gen.k")); err != nil {
		t.Errorf("pristine generate did not emit deploy/kcl/dev/config_gen.k — the scaffold's own deploy/kcl/dev/main.k import is unresolvable (features.deploy catch-22): %v", err)
	}

	handlerDir := filepath.Join(projectDir, "handlers", "item")

	// ── 2. projection vs implementation split ────────────────────────
	// (Non-fatal: a missing file here must not hide the executed-
	// lifecycle results below — this gate's whole point is maximum
	// behavioral signal per run.)
	// The implementation file: user-owned from line one, thin.
	shimPath := filepath.Join(handlerDir, "handlers_crud.go")
	if _, err := os.Stat(shimPath); os.IsNotExist(err) {
		t.Errorf("handlers_crud.go (the user-owned thin CRUD implementation file) was not scaffolded")
	} else {
		shim := readFileE2E(t, shimPath)
		if strings.Contains(shim, "DO NOT EDIT") {
			t.Errorf("handlers_crud.go carries a DO-NOT-EDIT banner — the CRUD implementation file must be user-owned from line one")
		}
		if !strings.Contains(shim, "crud.Handle") {
			t.Errorf("handlers_crud.go does not delegate to pkg/crud; got:\n%s", shim)
		}
		// THIN IS LOAD-BEARING: the owned file must never name entity
		// fields, or schema changes rot it. "Description" only exists as
		// an Item field name.
		if strings.Contains(shim, "Description") {
			t.Errorf("handlers_crud.go names entity fields — field mapping belongs in the generated ops file, not the owned shim:\n%s", shim)
		}
	}
	// The projection file: Tier-1, regenerated, names the fields.
	if _, err := os.Stat(filepath.Join(handlerDir, "handlers_crud_ops_gen.go")); os.IsNotExist(err) {
		t.Errorf("handlers_crud_ops_gen.go (the Tier-1 CRUD wiring projection) was not generated")
	}
	// The old Tier-1 implementation file is dead.
	if _, err := os.Stat(filepath.Join(handlerDir, "handlers_crud_gen.go")); !os.IsNotExist(err) {
		t.Errorf("handlers_crud_gen.go still emitted — the Tier-1 CRUD implementation file must be dead (stat err=%v)", err)
	}
	// The scaffold's own conventions must never mismatch themselves.
	if entries, err := os.ReadDir(handlerDir); err == nil {
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			body := readFileE2E(t, filepath.Join(handlerDir, e.Name()))
			// Check both spellings: the current marker and the legacy
			// one (which fresh output must never re-grow either).
			if strings.Contains(body, "forge:custom-read-shape") || strings.Contains(body, "FORGE_CRUD_SHAPE_MISMATCH") {
				t.Errorf("%s contains a custom-read-shape marker — the scaffold's own proto failed the shape matcher (F2)", e.Name())
			}
		}
	}

	// ── 3. migration semantics ───────────────────────────────────────
	migration := readFileE2E(t, filepath.Join(projectDir, "db", "migrations", "00001_create_items.up.sql"))
	if !strings.Contains(migration, "CHECK (id <> '')") {
		t.Errorf("migration lacks CHECK (id <> '') on the string PK — empty-id rows were the silent-upsert data-loss vector; got:\n%s", migration)
	}
	if !strings.Contains(migration, "created_at TIMESTAMPTZ") {
		t.Errorf("migration stores created_at as something other than TIMESTAMPTZ; got:\n%s", migration)
	}

	// The ORM is a projection of the schema, not of any proto: the
	// entity struct lives in internal/db and uses time.Time (the
	// timestamppb impedance is confined to the wire seam).
	ormPath := filepath.Join(projectDir, "internal", "db", "item_orm.go")
	if orm := readFileE2E(t, ormPath); !strings.Contains(orm, "type Item struct") {
		t.Errorf("internal/db/item_orm.go does not declare the Item entity struct")
	} else if strings.Contains(orm, "timestamppb") {
		t.Errorf("entity ORM still references timestamppb — db structs must be schema projections (time.Time)")
	}

	// ── 4. compiles ───────────────────────────────────────────────────
	runCmd(t, projectDir, "go", "build", "./...")

	// ── 5. the executed lifecycle ─────────────────────────────────────
	writeCorpusFile(t, filepath.Join(handlerDir, "crud_lifecycle_corpus_test.go"), crudLifecycleProbeSrc)
	out, err := runCorpusCmd(projectDir, "go", "test", "-count=1", "-run", "TestCorpusCRUDLifecycle", "-v", "./handlers/item/")
	if err != nil {
		t.Errorf("EXECUTED CRUD lifecycle failed:\n%s", out)
	} else {
		t.Logf("executed CRUD lifecycle passed:\n%s", out)
	}

	// ── 6. second entity through the FULL add-entity surface ─────────
	// bookmark exercises: proto injection (CRUD messages + RPCs into the
	// service proto), a repeated []string column (native TEXT[] in the
	// migration, JSON text on the sqlite test DB), soft delete.
	runCmd(t, projectDir, forgeBin, "add", "entity", "bookmark",
		"url:string", "title:string", "tags:[]string", "done:bool", "--soft-delete")
	itemProto := readFileE2E(t, filepath.Join(projectDir, "proto", "services", "item", "v1", "item.proto"))
	if !strings.Contains(itemProto, "rpc CreateBookmark(CreateBookmarkRequest)") {
		t.Fatalf("add entity did not scaffold the Bookmark CRUD RPCs into the service proto")
	}
	if strings.Contains(itemProto, "forge.v1.entity") {
		t.Errorf("add entity emitted a forge.v1.entity annotation — entity protos are dead, SQL is the schema")
	}
	bookmarkMig := readFileE2E(t, filepath.Join(projectDir, "db", "migrations", "00002_create_bookmarks.up.sql"))
	if !strings.Contains(bookmarkMig, "tags TEXT[] NOT NULL DEFAULT '{}'") {
		t.Errorf("bookmark migration lacks the native array column; got:\n%s", bookmarkMig)
	}
	runCmd(t, projectDir, forgeBin, "generate")
	runCmd(t, projectDir, "go", "build", "./...")

	writeCorpusFile(t, filepath.Join(handlerDir, "bookmark_lifecycle_corpus_test.go"), bookmarkLifecycleProbeSrc)
	out, err = runCorpusCmd(projectDir, "go", "test", "-count=1", "-run", "TestCorpusBookmarkLifecycle", "-v", "./handlers/item/")
	if err != nil {
		t.Errorf("EXECUTED bookmark (repeated-field + soft-delete) lifecycle failed:\n%s", out)
	} else {
		t.Logf("executed bookmark lifecycle passed:\n%s", out)
	}

	// ── 7. the schema EVOLVES by hand and the ORM follows ────────────
	// Add a column + move data (split url into a derived column) in a
	// hand-written migration — no proto change anywhere — and re-run
	// generate. The projection must pick up the new column end to end:
	// struct field, column allowlist (order_by=domain accepted via the
	// real RPC), scan/insert.
	writeCorpusFile(t, filepath.Join(projectDir, "db", "migrations", "00003_bookmark_domain.up.sql"), `
ALTER TABLE bookmarks ADD COLUMN domain TEXT NOT NULL DEFAULT '';
UPDATE bookmarks SET domain = substr(url, instr(url, '//') + 2);
`)
	writeCorpusFile(t, filepath.Join(projectDir, "db", "migrations", "00003_bookmark_domain.down.sql"),
		"ALTER TABLE bookmarks DROP COLUMN domain;\n")
	runCmd(t, projectDir, forgeBin, "generate")
	bookmarkORM := readFileE2E(t, filepath.Join(projectDir, "internal", "db", "bookmark_orm.go"))
	if !strings.Contains(bookmarkORM, "Domain string") {
		t.Errorf("hand-written migration added `domain` but the regenerated entity struct doesn't carry it — the ORM is not following the applied schema")
	}
	runCmd(t, projectDir, "go", "build", "./...")
	writeCorpusFile(t, filepath.Join(handlerDir, "bookmark_evolve_corpus_test.go"), bookmarkEvolveProbeSrc)
	out, err = runCorpusCmd(projectDir, "go", "test", "-count=1", "-run", "TestCorpusBookmarkEvolved", "-v", "./handlers/item/")
	if err != nil {
		t.Errorf("EXECUTED schema-evolution lifecycle failed:\n%s", out)
	} else {
		t.Logf("executed schema-evolution lifecycle passed:\n%s", out)
	}

	// ── 7b. legacy TEXT timestamps (M3, kalshi fr-3fba9166ba) ────────
	// A pre-forge schema stores created_at/updated_at as TEXT. The old
	// emitter stamped time.Now().UTC()/IsZero() unconditionally, so
	// `forge generate` emitted a trade_orm.go that could NEVER compile
	// (`undefined: time`, `msg.CreatedAt.IsZero undefined (type
	// string)`) — and the only "fix" was hand-patching a do-not-edit
	// file. Pin the whole path: generate exits 0, the projection keeps
	// the columns as strings, the project builds, and the executed ORM
	// stamps real RFC3339Nano text.
	runCmd(t, projectDir, forgeBin, "add", "entity", "trade", "ticker:string")
	tradeMigPath := filepath.Join(projectDir, "db", "migrations", "00004_create_trades.up.sql")
	tradeMig := readFileE2E(t, tradeMigPath)
	tradeMig = strings.ReplaceAll(tradeMig,
		"created_at TIMESTAMPTZ NOT NULL DEFAULT (now())", "created_at TEXT NOT NULL DEFAULT ''")
	tradeMig = strings.ReplaceAll(tradeMig,
		"updated_at TIMESTAMPTZ NOT NULL DEFAULT (now())", "updated_at TEXT NOT NULL DEFAULT ''")
	if !strings.Contains(tradeMig, "created_at TEXT") {
		t.Fatalf("failed to rewrite the trade migration to legacy TEXT timestamps; got:\n%s", tradeMig)
	}
	writeCorpusFile(t, tradeMigPath, tradeMig)
	runCmd(t, projectDir, forgeBin, "generate")
	tradeORM := readFileE2E(t, filepath.Join(projectDir, "internal", "db", "trade_orm.go"))
	if !strings.Contains(tradeORM, "CreatedAt string") {
		t.Errorf("TEXT created_at should project as a string struct field; got:\n%s", tradeORM)
	}
	if strings.Contains(tradeORM, "msg.CreatedAt.IsZero()") {
		t.Errorf("trade_orm.go calls IsZero on a string created_at — the fr-3fba9166ba shape is back")
	}
	runCmd(t, projectDir, "go", "build", "./...")
	writeCorpusFile(t, filepath.Join(handlerDir, "trade_text_timestamps_corpus_test.go"), tradeTextTimestampsProbeSrc)
	out, err = runCorpusCmd(projectDir, "go", "test", "-count=1", "-run", "TestCorpusTradeTextTimestamps", "-v", "./handlers/item/")
	if err != nil {
		t.Errorf("EXECUTED legacy-TEXT-timestamps lifecycle failed:\n%s", out)
	} else {
		t.Logf("executed legacy-TEXT-timestamps lifecycle passed:\n%s", out)
	}

	// ── 8. boots WITH a database: the Tier-1 bootstrap constructs the
	// DB + ORM pair from cfg.DatabaseUrl (ensureDatabase). Previously
	// NOTHING constructed app.ORM for a project that grew its first
	// entity post-scaffold (setup.go is scaffold-once and was rendered
	// with HasDatabase=false), so the typed-nil panicked on the first
	// RPC. /healthz 200 + clean SIGTERM against a real sqlite file pins
	// the constructed path end-to-end.
	bootHealthzAndShutdown(t, projectDir,
		"DATABASE_URL=file:"+filepath.Join(t.TempDir(), "corpus-boot.db"))

	// ── 8b. dev mode is usable with ZERO auth config: a real Connect
	// CRUD call over HTTP, with NO token, NO auth pack, NO AUTH_MODE —
	// just ENVIRONMENT=development. The authn passthrough must attach
	// the project's synthetic dev principal (devClaims hook), so the
	// auth-required generated CRUD path (middleware.GetUser) succeeds.
	// This was the J1 day-one rage-quit: forge run + browser = 401 on
	// every RPC until the user reverse-engineered the jwt-auth pack.
	bootDevCRUDNoToken(t, projectDir)

	// ── 9. and WITHOUT a database it fails AT BOOT, loudly ───────────
	// validateDeps (not the first RPC) is the gate: wire_gen passes a
	// true nil orm.Context via app.ORMContext(), the injected
	// `Deps.DB is required` check rejects it, and the process exits
	// non-zero naming DATABASE_URL — never a typed-nil panic mid-request.
	bootMustFailWithoutDatabase(t, projectDir)

	// ── 10. disown lifecycle on a generated ORM file (M3, kalshi
	// fr-4dfef712e9, reproduced verbatim): the escape hatch every
	// *_orm.go header advertises ("forge disown to take ownership")
	// must actually work. Before M3 the ORM emitter bypassed the
	// ownership chokepoint (its outputs carried no forge certification),
	// so disown refused on
	// internal/db/*_orm.go. Full round-trip: hand-edit → drift error →
	// disown → generate leaves it alone → delete + generate re-adopts.
	disownRoundTrip(t, forgeBin, projectDir, "internal/db/item_orm.go")

	t.Logf("crud-lifecycle fixture total: %s", time.Since(start))
}

// bootMustFailWithoutDatabase starts the already-built corpus server
// with an empty DATABASE_URL and asserts the boot FAILS fast with an
// actionable message. This is the validateDeps-at-boot pin for projects
// whose services require the database.
func bootMustFailWithoutDatabase(t *testing.T, projectDir string) {
	t.Helper()
	serverBin := filepath.Join(projectDir, "corpus-server")
	runCmd(t, projectDir, "go", "build", "-o", serverBin, "./cmd/...")

	cmd := exec.Command(serverBin, "server")
	cmd.Dir = projectDir
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("PORT=%d", freePortE2E(t)),
		"DATABASE_URL=",
		"ENVIRONMENT=development",
		"AUTH_MODE=none",
	)
	var out strings.Builder
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			t.Errorf("server BOOTED with no DATABASE_URL while serving DB-backed CRUD — validateDeps must reject at boot\noutput:\n%s", out.String())
		}
	case <-time.After(20 * time.Second):
		_ = cmd.Process.Kill()
		<-done
		t.Fatalf("server still running 20s after boot with no DATABASE_URL — the absence must fail AT BOOT, not on the first RPC\noutput:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "Deps.DB is required") {
		t.Errorf("boot failure should name the missing dep (Deps.DB is required); output:\n%s", out.String())
	}
	_ = os.Remove(serverBin)
}

// applyProjectMigrationsSQLite applies the project's db/migrations
// *.up.sql files, in order, to the given sqlite database file — the
// schema the generated server expects but does not create itself.
func applyProjectMigrationsSQLite(t *testing.T, projectDir, dbFile string) {
	t.Helper()
	db, err := sql.Open("sqlite", dbFile)
	if err != nil {
		t.Fatalf("open sqlite db %s: %v", dbFile, err)
	}
	defer db.Close()

	migDir := filepath.Join(projectDir, "db", "migrations")
	entries, err := os.ReadDir(migDir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}
	var ups []string
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".up.sql") {
			ups = append(ups, e.Name())
		}
	}
	if len(ups) == 0 {
		t.Fatalf("no .up.sql migrations in %s", migDir)
	}
	sort.Strings(ups)
	for _, name := range ups {
		sqlSrc, rerr := os.ReadFile(filepath.Join(migDir, name))
		if rerr != nil {
			t.Fatalf("read migration %s: %v", name, rerr)
		}
		if _, xerr := db.Exec(string(sqlSrc)); xerr != nil {
			t.Fatalf("apply migration %s: %v", name, xerr)
		}
	}
}

// bootDevCRUDNoToken boots the corpus server in dev mode (ENVIRONMENT=
// development, NO AUTH_MODE, no token, no pack) against a real sqlite
// file and makes a real Connect JSON call to an auth-required CRUD RPC.
// Pins the zero-config dev path end-to-end: authn passthrough attaches
// the scaffold's synthetic dev principal (the devClaims hook in
// pkg/middleware), middleware.GetUser finds claims, the dev authorizer
// allows, and the row round-trips.
func bootDevCRUDNoToken(t *testing.T, projectDir string) {
	t.Helper()
	port := freePortE2E(t)

	serverBin := filepath.Join(projectDir, "corpus-server")
	runCmd(t, projectDir, "go", "build", "-o", serverBin, "./cmd/...")

	// The generated server does not auto-migrate; give it a database
	// with the project's own schema applied (same SQL the in-process
	// lifecycle tests run against).
	dbFile := filepath.Join(t.TempDir(), "corpus-devclaims.db")
	applyProjectMigrationsSQLite(t, projectDir, dbFile)

	cmd := exec.Command(serverBin, "server")
	cmd.Dir = projectDir
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("PORT=%d", port),
		"DATABASE_URL=file:"+dbFile,
		"ENVIRONMENT=development",
	)
	// Dev mode ALONE must be enough — force AUTH_MODE empty in case the
	// host shell leaked one.
	cmd.Env = withForcedEnv(cmd.Env, "AUTH_MODE", "")
	var serverOut strings.Builder
	cmd.Stdout = &serverOut
	cmd.Stderr = &serverOut
	if err := cmd.Start(); err != nil {
		t.Fatalf("start dev-mode server: %v", err)
	}
	defer func() {
		_ = cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _ = cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
		_ = os.Remove(serverBin)
	}()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	if !waitForServer(t, base+"/healthz", 15*time.Second) {
		t.Fatalf("dev-mode server did not become ready\noutput:\n%s", serverOut.String())
	}

	resp, err := http.Post(
		base+"/services.item.v1.ItemService/CreateItem",
		"application/json",
		strings.NewReader(`{"name":"dev-claims-proof","description":"no token attached"}`),
	)
	if err != nil {
		t.Fatalf("POST CreateItem (no token, dev mode): %v", err)
	}
	defer resp.Body.Close()
	var body strings.Builder
	_, _ = io.Copy(&body, resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("dev-mode CreateItem without a token = HTTP %d, want 200 — the zero-config dev path is broken (devClaims not attached?)\nresponse: %s\nserver output:\n%s",
			resp.StatusCode, body.String(), serverOut.String())
	}
	var created struct {
		Item struct {
			Id   string `json:"id"`
			Name string `json:"name"`
		} `json:"item"`
	}
	if err := json.Unmarshal([]byte(body.String()), &created); err != nil {
		t.Fatalf("parse CreateItem response %q: %v", body.String(), err)
	}
	if created.Item.Id == "" || created.Item.Name != "dev-claims-proof" {
		t.Errorf("dev-mode CreateItem response missing the created row: %s", body.String())
	}
}

// appendToCorpusFile appends content to an existing file — used to add
// explicit override sections (e.g. database.driver) to the minimal
// scaffolded forge.yaml, whose derived-default sections aren't written
// to disk and so can't be string-replaced.
func appendToCorpusFile(t *testing.T, path, content string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := os.WriteFile(path, append(data, []byte(content)...), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// bookmarkLifecycleProbeSrc exercises the second entity born through
// the FULL `forge add entity` surface: repeated-field round trip
// (tags []string — native TEXT[] on postgres, JSON text on the sqlite
// test DB) and the soft-delete conventions read off the deleted_at
// column (delete is an UPDATE; reads filter; ListAll sees the corpse).
// tradeTextTimestampsProbeSrc executes the generated ORM against the
// legacy-TEXT-timestamps schema (created_at/updated_at TEXT): Create
// must stamp RFC3339Nano strings (caller-provided created_at wins),
// the row must round-trip, and Update must re-stamp updated_at while
// leaving created_at immutable. This is the runtime half of the
// fr-3fba9166ba pin — the compile half is the `go build` above.
const tradeTextTimestampsProbeSrc = `package item_test

import (
	"testing"
	"time"

	"example.com/crudlife/internal/db"
)

func TestCorpusTradeTextTimestamps(t *testing.T) {
	dbc := corpusMigratedDB(t)
	ctx := authedCtx()

	tr := &db.Trade{Ticker: "KXBTC"}
	if err := db.CreateTrade(ctx, dbc, tr); err != nil {
		t.Fatalf("CreateTrade against the TEXT-timestamp schema: %v", err)
	}
	if tr.Id == "" {
		t.Error("CreateTrade should mint a ULID id")
	}
	if tr.CreatedAt == "" || tr.UpdatedAt == "" {
		t.Fatalf("managed TEXT timestamps not stamped: created_at=%q updated_at=%q", tr.CreatedAt, tr.UpdatedAt)
	}
	if _, err := time.Parse(time.RFC3339Nano, tr.CreatedAt); err != nil {
		t.Errorf("created_at is not RFC3339Nano text: %q (%v)", tr.CreatedAt, err)
	}
	if _, err := time.Parse(time.RFC3339Nano, tr.UpdatedAt); err != nil {
		t.Errorf("updated_at is not RFC3339Nano text: %q (%v)", tr.UpdatedAt, err)
	}

	got, err := db.GetTradeByID(ctx, dbc, tr.Id)
	if err != nil {
		t.Fatalf("GetTradeByID: %v", err)
	}
	if got.Ticker != "KXBTC" || got.CreatedAt != tr.CreatedAt {
		t.Errorf("round-trip mismatch: ticker=%q created_at=%q (want %q / %q)", got.Ticker, got.CreatedAt, "KXBTC", tr.CreatedAt)
	}

	prevUpdated := got.UpdatedAt
	time.Sleep(5 * time.Millisecond) // RFC3339Nano resolution: force a distinct stamp
	got.Ticker = "KXETH"
	if err := db.UpdateTrade(ctx, dbc, got); err != nil {
		t.Fatalf("UpdateTrade: %v", err)
	}
	re, err := db.GetTradeByID(ctx, dbc, tr.Id)
	if err != nil {
		t.Fatalf("re-read after update: %v", err)
	}
	if re.Ticker != "KXETH" {
		t.Errorf("update did not mutate: ticker=%q", re.Ticker)
	}
	if re.UpdatedAt == prevUpdated {
		t.Errorf("updated_at was not re-stamped on update: %q", re.UpdatedAt)
	}
	if re.CreatedAt != tr.CreatedAt {
		t.Errorf("created_at must be immutable across updates: %q -> %q", tr.CreatedAt, re.CreatedAt)
	}
}
`

const bookmarkLifecycleProbeSrc = `package item_test

import (
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/fieldmaskpb"

	pb "example.com/crudlife/gen/services/item/v1"
	"example.com/crudlife/internal/db"
	"example.com/crudlife/pkg/app"
)

func TestCorpusBookmarkLifecycle(t *testing.T) {
	dbc := corpusMigratedDB(t)
	svc := app.NewTestItem(t, app.WithDB(dbc))
	ctx := authedCtx()

	// ── repeated-field round trip through the real RPC stack ─────────
	// The embedded comma is load-bearing: values must come back EXACTLY,
	// not flattened into a comma-joined string (the old non-compiling-ORM
	// bug's sibling failure mode).
	tags := []string{"go", "sql", "comma,inside"}
	cr, err := svc.CreateBookmark(ctx, connect.NewRequest(&pb.CreateBookmarkRequest{
		Url: "https://example.com/a", Title: "alpha", Tags: tags,
	}))
	if err != nil {
		t.Fatalf("create bookmark: %v", err)
	}
	id := cr.Msg.GetBookmark().GetId()
	if id == "" {
		t.Fatal("create returned empty id")
	}
	if got := cr.Msg.GetBookmark().GetTags(); len(got) != 3 {
		t.Errorf("create response tags = %v, want %v", got, tags)
	}
	gr, err := svc.GetBookmark(ctx, connect.NewRequest(&pb.GetBookmarkRequest{Id: id}))
	if err != nil {
		t.Fatalf("get bookmark: %v", err)
	}
	got := gr.Msg.GetBookmark().GetTags()
	if len(got) != len(tags) {
		t.Fatalf("repeated field did not round-trip: got %v want %v", got, tags)
	}
	for i := range tags {
		if got[i] != tags[i] {
			t.Errorf("tags[%d] = %q, want %q", i, got[i], tags[i])
		}
	}
	if gr.Msg.GetBookmark().GetCreatedAt() == nil {
		t.Error("created_at nil — managed-timestamp convention (created_at+updated_at columns) not honored")
	}

	// ── AIP-134 masked update: ONLY fields named in update_mask are
	// written. The submitted entity deliberately carries clobber values
	// for url and tags; the mask names only title — a masked
	// title-update must leave url/tags intact. This is the silent-data-
	// loss vector the J1 journey hit (updating title NULLED url+tags).
	if _, err := svc.UpdateBookmark(ctx, connect.NewRequest(&pb.UpdateBookmarkRequest{
		Bookmark: &pb.Bookmark{
			Id:    id,
			Title: "masked-title",
			Url:   "https://evil.example/clobber",
			Tags:  []string{"clobbered"},
		},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"title"}},
	})); err != nil {
		t.Fatalf("masked update (paths=[title]): %v", err)
	}
	grm, err := svc.GetBookmark(ctx, connect.NewRequest(&pb.GetBookmarkRequest{Id: id}))
	if err != nil {
		t.Fatalf("get after masked update: %v", err)
	}
	if got := grm.Msg.GetBookmark().GetTitle(); got != "masked-title" {
		t.Errorf("masked update did not write the masked field: title = %q, want %q", got, "masked-title")
	}
	if got := grm.Msg.GetBookmark().GetUrl(); got != "https://example.com/a" {
		t.Errorf("masked title-update CLOBBERED url: %q, want %q — update_mask is being ignored (whole-row write)", got, "https://example.com/a")
	}
	if got := grm.Msg.GetBookmark().GetTags(); len(got) != 3 {
		t.Errorf("masked title-update CLOBBERED tags: %v, want the original 3 — update_mask is being ignored (whole-row write)", got)
	}

	// A mask naming an unknown/immutable path is INVALID_ARGUMENT, not a
	// silent partial write.
	if _, err := svc.UpdateBookmark(ctx, connect.NewRequest(&pb.UpdateBookmarkRequest{
		Bookmark:   &pb.Bookmark{Id: id, Title: "x"},
		UpdateMask: &fieldmaskpb.FieldMask{Paths: []string{"no_such_field"}},
	})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("masked update with unknown path: err = %v, want InvalidArgument", err)
	}

	// ── unmasked update = documented full-object replace ─────────────
	upd := grm.Msg.GetBookmark()
	upd.Tags = []string{"only-one"}
	if _, err := svc.UpdateBookmark(ctx, connect.NewRequest(&pb.UpdateBookmarkRequest{Bookmark: upd})); err != nil {
		t.Fatalf("update bookmark: %v", err)
	}
	gr2, err := svc.GetBookmark(ctx, connect.NewRequest(&pb.GetBookmarkRequest{Id: id}))
	if err != nil {
		t.Fatalf("get after update: %v", err)
	}
	if g := gr2.Msg.GetBookmark().GetTags(); len(g) != 1 || g[0] != "only-one" {
		t.Errorf("update did not mutate tags: %v", g)
	}

	// ── soft delete: UPDATE-not-DELETE, reads filter, ListAll sees ────
	if _, err := svc.DeleteBookmark(ctx, connect.NewRequest(&pb.DeleteBookmarkRequest{Id: id})); err != nil {
		t.Fatalf("delete bookmark: %v", err)
	}
	if _, err := svc.GetBookmark(ctx, connect.NewRequest(&pb.GetBookmarkRequest{Id: id})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Errorf("get after soft delete: err = %v, want NotFound", err)
	}
	lr, err := svc.ListBookmarks(ctx, connect.NewRequest(&pb.ListBookmarksRequest{PageSize: 10}))
	if err != nil {
		t.Fatalf("list after delete: %v", err)
	}
	if n := len(lr.Msg.GetBookmarks()); n != 0 {
		t.Errorf("list after soft delete: %d rows, want 0", n)
	}
	// ListAll bypasses ONLY the soft-delete filter: the corpse is
	// visible with deleted_at set — delete was an UPDATE, not a DELETE.
	all, err := db.ListAllBookmark(ctx, dbc)
	if err != nil {
		t.Fatalf("ListAllBookmark: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("ListAll after soft delete: %d rows, want 1 (the soft-deleted row)", len(all))
	}
	if all[0].DeletedAt == nil {
		t.Error("soft-deleted row has nil deleted_at — delete did not stamp the column")
	}
}
`

// bookmarkEvolveProbeSrc runs AFTER the hand-written migration ladder
// (add column + data movement) and `forge generate`: the ORM followed
// the applied schema, so the new `domain` column is a declared column —
// order_by=domain is accepted by the real RPC, and the struct carries
// the field (compile-time proof via direct ORM use).
const bookmarkEvolveProbeSrc = `package item_test

import (
	"testing"

	"connectrpc.com/connect"

	pb "example.com/crudlife/gen/services/item/v1"
	"example.com/crudlife/internal/db"
	"example.com/crudlife/pkg/app"
)

func TestCorpusBookmarkEvolved(t *testing.T) {
	dbc := corpusMigratedDB(t)
	svc := app.NewTestItem(t, app.WithDB(dbc))
	ctx := authedCtx()

	cr, err := svc.CreateBookmark(ctx, connect.NewRequest(&pb.CreateBookmarkRequest{
		Url: "https://example.com/b", Title: "beta",
	}))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// The new column is a first-class struct field (schema projection).
	row, err := db.GetBookmarkByID(ctx, dbc, cr.Msg.GetBookmark().GetId())
	if err != nil {
		t.Fatalf("orm get: %v", err)
	}
	if row.Domain != "" {
		t.Logf("domain = %q (DEFAULT '' for new rows is fine)", row.Domain)
	}

	// order_by on the migration-added column must be ACCEPTED — the
	// column allowlist is regenerated from the applied schema.
	if _, err := svc.ListBookmarks(ctx, connect.NewRequest(&pb.ListBookmarksRequest{PageSize: 10, OrderBy: "domain"})); err != nil {
		t.Errorf("order_by=domain (a column added by hand-written migration) rejected: %v — the ORM did not follow the applied schema", err)
	}
	// ...and a phantom column must still be REJECTED.
	if _, err := svc.ListBookmarks(ctx, connect.NewRequest(&pb.ListBookmarksRequest{PageSize: 10, OrderBy: "phantom"})); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Errorf("order_by=phantom: err = %v, want InvalidArgument", err)
	}
}
`

// ── small file/exec utilities (corpus-local; no t.Fatal-free variants
// exist in the sibling e2e files) ──────────────────────────────────────

func writeCorpusFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func appendCorpusFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open %s for append: %v", path, err)
	}
	defer f.Close()
	if _, err := f.WriteString(content); err != nil {
		t.Fatalf("append %s: %v", path, err)
	}
}

// mustReplaceInFile replaces old with new in path, failing if old is
// absent — a scaffold-shape drift signal, not a soft skip.
func mustReplaceInFile(t *testing.T, path, old, new string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	content := string(data)
	if !strings.Contains(content, old) {
		t.Fatalf("scaffold drift: %s does not contain anchor:\n%s", path, old)
	}
	content = strings.Replace(content, old, new, 1)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// mustReplaceFirstInFile is mustReplaceInFile for anchors that appear
// multiple times — only the FIRST occurrence is replaced.
func mustReplaceFirstInFile(t *testing.T, path, old, new string) {
	mustReplaceInFile(t, path, old, new)
}

// runCorpusCmd runs a command and returns its combined output and error
// (callers assert on failure modes themselves).
func runCorpusCmd(dir, name string, args ...string) (string, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// runCorpusCmdOK runs a command and fails the test on error, returning
// the combined output for content assertions.
func runCorpusCmdOK(t *testing.T, dir, name string, args ...string) string {
	t.Helper()
	out, err := runCorpusCmd(dir, name, args...)
	if err != nil {
		t.Fatalf("command %q in %s failed: %v\n%s", append([]string{name}, args...), dir, err, out)
	}
	return out
}
