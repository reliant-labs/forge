package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/codegen"
)

// TestBootstrapDepsCoverage_StaticMismatchNoSetup asserts that a
// project with a name-match-but-type-mismatch AND no setup.go
// re-construction still reports the finding (the legacy behavior —
// nothing closes the runtime hole).
func TestBootstrapDepsCoverage_StaticMismatchNoSetup(t *testing.T) {
	t.Parallel()
	projectDir := t.TempDir()
	writeAppExtras(t, projectDir, `package app

import "example.com/proj/internal/db"

type App struct {
	*AppExtras
}

type AppExtras struct {
	Repo *db.PostgresRepository
}
`)
	writeContract(t, projectDir, "audit", `package audit

type Repository interface{ Log() }

type Deps struct {
	Repo Repository
}
`)

	got := runAndCollect(t, projectDir)
	if len(got) != 1 {
		t.Fatalf("expected 1 finding, got %d: %+v", len(got), got)
	}
	if got[0].Package != "audit" || got[0].Field != "Repo" {
		t.Errorf("finding = %+v, want audit/Repo", got[0])
	}
}

// TestBootstrapDepsCoverage_SetupReconstructionClears asserts that
// a setup.go re-construction with the conflicting field assigned to
// a non-nil expression clears the finding. The runtime hole is closed
// (the package gets a live Repo at construction time), so the lint
// should match runtime reality and stay silent.
func TestBootstrapDepsCoverage_SetupReconstructionClears(t *testing.T) {
	t.Parallel()
	projectDir := t.TempDir()
	writeAppExtras(t, projectDir, `package app

import "example.com/proj/internal/db"

type App struct {
	*AppExtras
}

type AppExtras struct {
	Repo *db.PostgresRepository
}
`)
	writeContract(t, projectDir, "audit", `package audit

type Repository interface{ Log() }

type Deps struct {
	Repo Repository
}
`)
	writeSetup(t, projectDir, `package app

import (
	"example.com/proj/internal/audit"
	"example.com/proj/pkg/config"
)

func Setup(app *App, cfg *config.Config) error {
	app.AuditService = audit.New(audit.Deps{Repo: app.Repo})
	return nil
}
`)

	got := runAndCollect(t, projectDir)
	if len(got) != 0 {
		t.Fatalf("expected 0 findings (setup.go closes the hole), got %d: %+v", len(got), got)
	}
}

// TestBootstrapDepsCoverage_NilValueDoesNotClear asserts that
// re-construction with an explicit `nil` value for the conflicting
// field does NOT clear the finding. The setup.go author "filed the
// paperwork" by re-constructing, but the runtime hole is still open —
// the package gets a nil Repo just as it would without re-construction.
func TestBootstrapDepsCoverage_NilValueDoesNotClear(t *testing.T) {
	t.Parallel()
	projectDir := t.TempDir()
	writeAppExtras(t, projectDir, `package app

import "example.com/proj/internal/db"

type App struct {
	*AppExtras
}

type AppExtras struct {
	Repo *db.PostgresRepository
}
`)
	writeContract(t, projectDir, "audit", `package audit

type Repository interface{ Log() }

type Deps struct {
	Repo Repository
}
`)
	writeSetup(t, projectDir, `package app

import (
	"example.com/proj/internal/audit"
	"example.com/proj/pkg/config"
)

func Setup(app *App, cfg *config.Config) error {
	app.AuditService = audit.New(audit.Deps{Repo: nil})
	return nil
}
`)

	got := runAndCollect(t, projectDir)
	if len(got) != 1 {
		t.Fatalf("expected 1 finding (nil value leaves hole open), got %d: %+v", len(got), got)
	}
}

// TestBootstrapDepsCoverage_OnlyRelevantFieldCleared asserts that a
// setup.go re-construction clears only the field it actually assigns —
// other mismatched fields on the same package or other packages still
// report normally.
func TestBootstrapDepsCoverage_OnlyRelevantFieldCleared(t *testing.T) {
	t.Parallel()
	projectDir := t.TempDir()
	writeAppExtras(t, projectDir, `package app

import "example.com/proj/internal/db"

type App struct {
	*AppExtras
}

type AppExtras struct {
	Repo  *db.PostgresRepository
	Cache *db.PostgresRepository
}
`)
	writeContract(t, projectDir, "audit", `package audit

type Repository interface{ Log() }
type CacheStore interface{ Get() }

type Deps struct {
	Repo  Repository
	Cache CacheStore
}
`)
	writeSetup(t, projectDir, `package app

import (
	"example.com/proj/internal/audit"
	"example.com/proj/pkg/config"
)

func Setup(app *App, cfg *config.Config) error {
	// Wires Repo only — Cache still leaks.
	app.AuditService = audit.New(audit.Deps{Repo: app.Repo})
	return nil
}
`)

	got := runAndCollect(t, projectDir)
	if len(got) != 1 {
		t.Fatalf("expected 1 finding (Cache leaks; Repo cleared), got %d: %+v", len(got), got)
	}
	if got[0].Field != "Cache" {
		t.Errorf("finding = %+v, want Cache", got[0])
	}
}

// TestScanSetupReconstructions_DetectsPatterns exercises the parser
// directly with the five real-world setup.go shapes seen in cp-forge:
// direct `app.X = pkg.New(pkg.Deps{...})`, `pkg.NewWithLogger(...)`
// (extra args after Deps), `pkg.New(...) (T, error)` (multi-return
// with intermediate variable). Each should land the keyed field in
// the wired map.
func TestScanSetupReconstructions_DetectsPatterns(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	setup := filepath.Join(tmp, "setup.go")
	src := `package app

import (
	"log/slog"

	"example.com/proj/internal/audit"
	"example.com/proj/internal/daemontoken"
	"example.com/proj/internal/gitcredential"
	"example.com/proj/internal/org"
	"example.com/proj/internal/user"
	"example.com/proj/pkg/config"
)

func Setup(app *App, cfg *config.Config) error {
	auditSvc := audit.NewWithLogger(audit.Deps{Repo: app.Repo}, slog.Default())
	app.AuditService = auditSvc

	app.DaemontokenService = daemontoken.New(daemontoken.Deps{
		Repo:   app.Repo,
		Audit:  auditSvc,
		Logger: slog.Default(),
	})

	gitCredSvc, err := gitcredential.New(gitcredential.Deps{
		Logger:     slog.Default(),
		Repo:       app.Repo,
		DaemonRepo: app.Repo,
	})
	if err != nil {
		return err
	}
	app.GitcredentialService = gitCredSvc

	orgSvc, err := org.NewWithLogger(org.Deps{
		Repo:       app.Repo,
		AuthHelper: app.AuthHelper,
	}, slog.Default())
	if err != nil {
		return err
	}
	app.OrgService = orgSvc

	app.UserService = user.New(user.Deps{
		Repo:  app.Repo,
		Audit: auditSvc,
	})

	return nil
}
`
	if err := os.WriteFile(setup, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}

	wired, err := scanSetupReconstructions(setup)
	if err != nil {
		t.Fatalf("scanSetupReconstructions: %v", err)
	}

	cases := []struct {
		pkg, field string
	}{
		{"audit", "Repo"},
		{"daemontoken", "Repo"},
		{"daemontoken", "Audit"},
		{"gitcredential", "Repo"},
		{"gitcredential", "DaemonRepo"},
		{"org", "Repo"},
		{"org", "AuthHelper"},
		{"user", "Repo"},
		{"user", "Audit"},
	}
	for _, c := range cases {
		if !wired[c.pkg][c.field] {
			t.Errorf("expected wired[%q][%q] = true, got false (wired=%+v)", c.pkg, c.field, wired)
		}
	}
}

// TestScanSetupReconstructions_MissingFile asserts that a missing
// setup.go is not an error — it returns an empty map. Many projects
// never customize setup.go and the lint should still work.
func TestScanSetupReconstructions_MissingFile(t *testing.T) {
	t.Parallel()
	tmp := t.TempDir()
	wired, err := scanSetupReconstructions(filepath.Join(tmp, "setup.go"))
	if err != nil {
		t.Fatalf("scanSetupReconstructions: %v", err)
	}
	if len(wired) != 0 {
		t.Errorf("expected empty map for missing file, got %+v", wired)
	}
}

// TestFormatBootstrapCoverage_HintMentionsSetupGo asserts the
// remediation hint promises what the lint can actually deliver — the
// setup.go re-construction path is described as "the lint detects and
// clears" rather than the old "OR wire manually" line that misled
// cp-forge into following advice the lint then ignored.
func TestFormatBootstrapCoverage_HintMentionsSetupGo(t *testing.T) {
	t.Parallel()
	var buf strings.Builder
	formatBootstrapCoverage(&buf, []bootstrapCoverageFinding{
		{Package: "audit", Field: "Repo", DepsType: "Repository", AppType: "*db.PostgresRepository"},
	})
	out := buf.String()
	if !strings.Contains(out, "setup.go") {
		t.Errorf("hint should mention setup.go, got:\n%s", out)
	}
	if !strings.Contains(out, "clears the finding") {
		t.Errorf("hint should promise the lint detects+clears the re-construction, got:\n%s", out)
	}
}

// runAndCollect is a thin helper that invokes the lint against a temp
// project and returns the findings slice that would have been printed.
// It mirrors runBootstrapDepsCoverageLint's body so tests can assert
// on the structured findings rather than parse stdout.
func runAndCollect(t *testing.T, projectDir string) []bootstrapCoverageFinding {
	t.Helper()
	appDir := filepath.Join(projectDir, "pkg", "app")
	appFields, err := readAppFields(appDir)
	if err != nil {
		t.Fatalf("read app fields: %v", err)
	}
	appByName := map[string]string{}
	for name, typ := range appFields {
		appByName[name] = typ
	}
	internalDir := filepath.Join(projectDir, "internal")
	entries, err := os.ReadDir(internalDir)
	if err != nil {
		t.Fatalf("read internal: %v", err)
	}
	setupWired, err := scanSetupReconstructions(filepath.Join(appDir, "setup.go"))
	if err != nil {
		t.Fatalf("scanSetupReconstructions: %v", err)
	}
	var findings []bootstrapCoverageFinding
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		deps, err := readDeps(filepath.Join(internalDir, e.Name()))
		if err != nil || len(deps) == 0 {
			continue
		}
		for name, typ := range deps {
			if name == "Logger" || name == "Config" {
				continue
			}
			appType, hasName := appByName[name]
			if !hasName {
				continue
			}
			if appType == typ {
				continue
			}
			if fields, ok := setupWired[e.Name()]; ok && fields[name] {
				continue
			}
			findings = append(findings, bootstrapCoverageFinding{
				Package:  e.Name(),
				Field:    name,
				DepsType: typ,
				AppType:  appType,
			})
		}
	}
	return findings
}

// readAppFields / readDeps wrap the codegen parsers and project the
// (name, type) pairs the lint actually keys on. Kept narrow so the
// table-style helpers above don't have to know about DepsField /
// AppField specifics.
func readAppFields(appDir string) (map[string]string, error) {
	fields, err := codegen.ParseAppFields(appDir)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, f := range fields {
		out[f.Name] = f.Type
	}
	return out, nil
}

func readDeps(pkgDir string) (map[string]string, error) {
	fields, err := codegen.ParseServiceDeps(pkgDir)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, f := range fields {
		out[f.Name] = f.Type
	}
	return out, nil
}

func writeAppExtras(t *testing.T, projectDir, src string) {
	t.Helper()
	dir := filepath.Join(projectDir, "pkg", "app")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "app_extras.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeContract(t *testing.T, projectDir, pkg, src string) {
	t.Helper()
	dir := filepath.Join(projectDir, "internal", pkg)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "contract.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeSetup(t *testing.T, projectDir, src string) {
	t.Helper()
	dir := filepath.Join(projectDir, "pkg", "app")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "setup.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
}
