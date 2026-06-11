//go:build e2e

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
	"encoding/hex"
	"encoding/json"
	"fmt"
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
)

// ───────────────────────── fixture 1: cp-forge-shaped ─────────────────────────

func TestE2EFixtureCorpusCPForgeShaped(t *testing.T) {
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
	bootHealthzAndShutdown(t, projectDir, 18931)

	// ── 5. disown lifecycle round-trip on pkg/app/wire_gen.go ────────
	disownRoundTrip(t, forgeBin, projectDir)

	t.Logf("cp-forge-shaped fixture total: %s", time.Since(start))
}

// ───────────────────────── fixture 2: kalshi-shaped ─────────────────────────

func TestE2EFixtureCorpusKalshiShaped(t *testing.T) {
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
	bootHealthzAndShutdown(t, projectDir, 18932)

	// ── 5. disown lifecycle round-trip ────────────────────────────────
	disownRoundTrip(t, forgeBin, projectDir)

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
//	  - src/lib/basepath_gen.ts exists, is Tier-1-tracked in
//	    .forge/checksums.json, exports BASE_PATH defaulting to "/admin"
//	    and an idempotent joinBasePath;
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
	runCmd(t, projectDir, forgeBin, "add", "frontend", "console", "--base-path", "/admin")
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

	// Non-vacuousness pin: the default scaffold's Item CRUD must surface
	// as an APP-RELATIVE route in nav_gen.tsx. An empty route table
	// would make the prefix-clean scan above meaningless, and a
	// "/admin/items" path would be exactly the hand-prefix bug class it
	// exists to catch (Link/router add the prefix at render time).
	navGen := readFileE2E(t, filepath.Join(feDir, "src", "components", "nav_gen.tsx"))
	if !strings.Contains(navGen, `path: "/items"`) {
		t.Errorf("nav_gen.tsx must carry the app-relative \"/items\" route; got:\n%s", navGen)
	}

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
	// Static output (the scaffold default) must refuse to bake a
	// root-mounted export when the override empties the prefix.
	if !strings.Contains(cfg, `process.env.NODE_ENV === "production" && basePath === ""`) ||
		!strings.Contains(cfg, "throw new Error") {
		t.Errorf("next.config.ts (static output) must carry the fail-loud empty-basePath production guard; got:\n%s", cfg)
	}
}

// assertBasePathGenHelper pins the generated src/lib/basepath_gen.ts:
// exists, exports BASE_PATH (env override, declared default) and an
// idempotent joinBasePath, and is tracked as Tier-1 in
// .forge/checksums.json (so hand edits trip the stomp guard and
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

	// Tier-1 tracking in checksums.json.
	var cs struct {
		Files map[string]struct {
			Hash string `json:"hash"`
			Tier int    `json:"tier"`
		} `json:"files"`
	}
	raw := readFileE2E(t, filepath.Join(projectDir, ".forge", "checksums.json"))
	if err := json.Unmarshal([]byte(raw), &cs); err != nil {
		t.Fatalf("parse .forge/checksums.json: %v", err)
	}
	entry, ok := cs.Files[rel]
	switch {
	case !ok:
		t.Errorf("%s is not tracked in .forge/checksums.json — hand edits would never trip the Tier-1 stomp guard", rel)
	case entry.Tier != 1:
		t.Errorf("%s tracked with tier=%d, want tier=1 (regenerated-every-run)", rel, entry.Tier)
	case entry.Hash == "":
		t.Errorf("%s tracked with empty hash in .forge/checksums.json", rel)
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
	bootHealthzAndShutdown(t, projectDir, 18933)

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
// every regular file. Nothing forge-owned is excluded: checksums.json,
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
func bootHealthzAndShutdown(t *testing.T, projectDir string, port int) {
	t.Helper()

	serverBin := filepath.Join(projectDir, "corpus-server")
	runCmd(t, projectDir, "go", "build", "-o", serverBin, "./cmd/...")

	cmd := exec.Command(serverBin, "server")
	cmd.Dir = projectDir
	cmd.Env = append(os.Environ(),
		fmt.Sprintf("PORT=%d", port),
		"DATABASE_URL=",           // health checks must not need a DB
		"ENVIRONMENT=development", // dev authorizer; no real authz backend
		"AUTH_MODE=none",          // explicit no-auth (AuthInterceptor panics otherwise)
	)
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

// disownRoundTrip exercises the full ownership lifecycle on ONE file
// (pkg/app/wire_gen.go):
//
//	hand-edit → generate (drift error, new option text) →
//	`forge disown --reason` (one-way transfer) → generate leaves the
//	file alone with ZERO warnings → delete + generate re-adopts to the
//	pristine render (entry back to Tier-1).
func disownRoundTrip(t *testing.T, forgeBin, projectDir string) {
	t.Helper()
	const rel = "pkg/app/wire_gen.go"
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
	if got := readFileE2E(t, wireGenPath); got != pristine {
		t.Errorf("re-adoption generate did not restore the pristine render of %s", rel)
	}
	var cs struct {
		Files map[string]struct {
			Tier     int  `json:"tier"`
			Disowned bool `json:"disowned"`
		} `json:"files"`
	}
	raw := readFileE2E(t, filepath.Join(projectDir, ".forge", "checksums.json"))
	if err := json.Unmarshal([]byte(raw), &cs); err != nil {
		t.Fatalf("parse .forge/checksums.json: %v", err)
	}
	entry, ok := cs.Files[rel]
	switch {
	case !ok:
		t.Errorf("%s not tracked after re-adoption", rel)
	case entry.Tier != 1 || entry.Disowned:
		t.Errorf("%s entry = %+v after re-adoption, want tier=1 disowned=false", rel, entry)
	}
}

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
