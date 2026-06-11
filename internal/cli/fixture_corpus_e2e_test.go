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
// Two fixtures:
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
// Each fixture asserts, in order:
//
//  1. `forge generate` succeeds AND is idempotent: a second run
//     produces zero file changes (full tree hash) and zero fork
//     warnings.
//  2. The generated wiring (wire_gen.go + bootstrap.go) contains NO
//     silent nil / dropped wire for the known name-matched Deps
//     fields — the deps-matcher pin.
//  3. The generated project compiles (`go build ./...` with a local
//     forge/pkg replace).
//  4. The built binary boots, /healthz returns 200, and SIGTERM shuts
//     it down cleanly within a bounded wait.
//  5. Fork lifecycle round-trip on pkg/app/wire_gen.go:
//     hand-edit → generate (drift error) → generate --accept (fork +
//     coherence warning) → generate (side render + loud skip) →
//     unfork --merge (clean reconcile) → generate (clean again).
//
// Run with:
//
//	go test -tags e2e -run TestE2EFixtureCorpus -v ./internal/cli/
package cli

import (
	"crypto/sha256"
	"encoding/hex"
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

	// ── 5. fork lifecycle round-trip on pkg/app/wire_gen.go ──────────
	forkRoundTrip(t, forgeBin, projectDir)

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

	// ── 5. fork lifecycle round-trip ──────────────────────────────────
	forkRoundTrip(t, forgeBin, projectDir)

	t.Logf("kalshi-shaped fixture total: %s", time.Since(start))
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
// compare), and (c) emits zero fork warnings. Idempotency is a
// first-class assertion: the kalshi matcher bug manifested as regen
// output flip-flopping between app.<Field> and nil across runs.
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
			if d != nil && d.IsDir() && d.Name() == ".git" {
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

// assertNoForkNoise fails when a clean (fork-free) generate run prints
// fork machinery output.
func assertNoForkNoise(t *testing.T, label, out string) {
	t.Helper()
	for _, needle := range []string{"forked file(s)", "fork-coherence", "now forks"} {
		if strings.Contains(out, needle) {
			t.Errorf("%s: unexpected fork warning (%q) in output:\n%s", label, needle, out)
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

// hashProjectTree walks the project and returns rel-path → sha256 for
// every regular file. Nothing is excluded: checksums.json, side
// renders, go.sum — a second generate must leave ALL of it untouched.
func hashProjectTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if d.Name() == ".git" {
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

// forkRoundTrip exercises the full fork lifecycle on ONE file
// (pkg/app/wire_gen.go):
//
//	hand-edit → generate (drift error) → generate --accept (fork +
//	coherence warning) → generate (side render + loud skip) →
//	unfork --merge (clean three-way reconcile) → generate (clean).
func forkRoundTrip(t *testing.T, forgeBin, projectDir string) {
	t.Helper()
	const rel = "pkg/app/wire_gen.go"
	wireGenPath := filepath.Join(projectDir, rel)
	pristine := readFileE2E(t, wireGenPath)

	// Hand-edit: append a user marker (an edit a 3-way merge keeps).
	const marker = "func userForkMarker() {}"
	appendCorpusFile(t, wireGenPath, "\n// user fork edit — must survive unfork --merge\n"+marker+"\n")

	// 1. Plain generate must trip the Tier-1 stomp guard.
	out, err := runCorpusCmd(projectDir, forgeBin, "generate")
	if err == nil {
		t.Fatalf("generate over a hand-edited Tier-1 file must fail (drift guard); output:\n%s", out)
	}
	if !strings.Contains(out, "Tier-1") || !strings.Contains(out, rel) {
		t.Fatalf("drift error should name the Tier-1 guard and %s; got:\n%s", rel, out)
	}

	// 2. --accept: fork the file. The accept run itself completes the
	// pipeline, so it must print (a) the accept confirmation, (b) the
	// fork-coherence warning (wire_gen.go is in the app-wiring group),
	// and (c) the LOUD one-time skip line with the side-render location
	// and the unfork --merge hint.
	out = runCorpusCmdOK(t, projectDir, forgeBin, "generate", "--accept")
	if !strings.Contains(out, rel) || !strings.Contains(out, "--accept") {
		t.Errorf("--accept run should confirm the accepted path; got:\n%s", out)
	}
	if !strings.Contains(out, "fork-coherence") {
		t.Errorf("--accept on %s should print the fork-coherence group warning; got:\n%s", rel, out)
	}
	if !strings.Contains(out, "forked file(s)") || !strings.Contains(out, ".forge/render/"+rel) {
		t.Errorf("fork must be skipped loudly with its side-render path; got:\n%s", out)
	}
	if !strings.Contains(out, "unfork --merge") {
		t.Errorf("fork skip report should point at `forge unfork --merge`; got:\n%s", out)
	}

	// 3. Next generate: the fork is honored (file untouched, fresh
	// render parked at .forge/render/), and the skip stays QUIET — the
	// one-time-notice contract (Accepted flips after the first loud
	// report so established forks don't re-nag every run).
	out = runCorpusCmdOK(t, projectDir, forgeBin, "generate")
	if strings.Contains(out, "forked file(s)") {
		t.Errorf("established fork must not re-nag on later runs; got:\n%s", out)
	}
	assertPathExistsE2E(t, filepath.Join(projectDir, ".forge", "render", rel))
	assertPathExistsE2E(t, filepath.Join(projectDir, ".forge", "render-base", rel))
	if got := readFileE2E(t, wireGenPath); !strings.Contains(got, marker) {
		t.Fatalf("forked %s was overwritten by generate — fork not honored", rel)
	}

	// 4. unfork --merge: three-way reconcile. Template did not change,
	// so the merge must resolve cleanly and keep the user marker.
	out = runCorpusCmdOK(t, projectDir, forgeBin, "unfork", "--merge", rel)
	merged := readFileE2E(t, wireGenPath)
	if !strings.Contains(merged, marker) {
		t.Fatalf("unfork --merge dropped the user edit; output:\n%s\nmerged:\n%s", out, merged)
	}
	if strings.Contains(merged, "<<<<<<<") {
		t.Fatalf("unfork --merge left conflict markers:\n%s", merged)
	}

	// 5. Final generate: forge owns the file again — clean run, no
	// drift error, no fork noise, and the render returns to pristine.
	out = runCorpusCmdOK(t, projectDir, forgeBin, "generate")
	assertNoForkNoise(t, "post-unfork generate", out)
	if got := readFileE2E(t, wireGenPath); got != pristine {
		t.Errorf("post-unfork generate did not restore the pristine render of %s", rel)
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
