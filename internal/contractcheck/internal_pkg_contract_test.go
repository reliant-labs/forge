// File: internal/contractcheck/internal_pkg_contract_test.go
//
// Coverage for the internal-package-contract-names rule. These tests
// were ported from internal/linter/forgeconv/forgeconv_test.go when
// the rule moved into this package; assertions, fixtures, and table
// rows are byte-for-byte identical so a green run here proves the
// move preserved behavior.

package contractcheck

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/linter/forgeconv"
)

// TestInternalContracts_GoodFixtureClean verifies a contract.go that uses
// the canonical Service/Deps/New(Deps) Service shape produces zero findings.
func TestInternalContracts_GoodFixtureClean(t *testing.T) {
	fs, err := Inspect(context.Background(),
		filepath.Join("testdata", "contracts_good"),
		Options{Rules: []Rule{RuleInternalPackageContractNames}},
	)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if len(fs) != 0 {
		t.Fatalf("expected 0 findings on canonical contract, got %d:\n%s",
			len(fs), AsResult(fs).FormatText())
	}
	if HasErrors(fs) {
		t.Errorf("good fixture should not have errors")
	}
}

// TestInternalContracts_BadFixtureFiresThreeFindings verifies a contract.go
// using the wrong names (Sender/Config/NewSender) produces one finding for
// each of the three canonical pieces (Service, Deps, New(Deps) Service) so
// the user sees the full delta in one run rather than discovering it
// piecemeal across re-runs.
func TestInternalContracts_BadFixtureFiresThreeFindings(t *testing.T) {
	fs, err := Inspect(context.Background(),
		filepath.Join("testdata", "contracts_bad"),
		Options{Rules: []Rule{RuleInternalPackageContractNames}},
	)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	got := findingsForRule(fs, string(RuleInternalPackageContractNames))
	if len(got) != 3 {
		t.Fatalf("expected 3 findings (Service/Deps/New), got %d:\n%s",
			len(got), AsResult(fs).FormatText())
	}

	// Each finding must carry the same actionable phrase so users can
	// grep and find the convention doc.
	const sentinel = "internal-package contracts must declare 'type Service interface', 'type Deps struct', and 'func New(Deps) Service'"
	for _, f := range got {
		if !strings.Contains(f.Message, sentinel) {
			t.Errorf("finding missing canonical sentinel; got: %s", f.Message)
		}
		if f.Severity != forgeconv.SeverityError {
			t.Errorf("internal-package-contract-names should be an error, got %s", f.Severity)
		}
	}

	// The three findings should reference the three actual names found, so
	// the user sees what to rename.
	combined := AsResult(fs).FormatText()
	for _, want := range []string{"Sender", "Config", "NewSender"} {
		if !strings.Contains(combined, want) {
			t.Errorf("expected finding text to reference non-canonical name %q; got:\n%s", want, combined)
		}
	}

	if !HasErrors(fs) {
		t.Errorf("non-canonical contract must gate the build")
	}
}

// TestInternalContracts_HonorsExcludes verifies that directories listed
// in the excludes set are skipped — packages that legitimately don't
// follow the convention (analyzer sub-packages, embed-only packages,
// internal/packs which isn't bootstrap-managed) opt out via
// contracts.exclude in forge.yaml and the analyzer must respect it.
func TestInternalContracts_HonorsExcludes(t *testing.T) {
	// First, prove the fixture would otherwise fire (no exclude → findings).
	resBefore, err := Inspect(context.Background(),
		filepath.Join("testdata", "contracts_excluded"),
		Options{Rules: []Rule{RuleInternalPackageContractNames}},
	)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if len(resBefore) == 0 {
		t.Fatalf("fixture sanity: contracts_excluded must produce findings without an exclude")
	}

	// Now apply the exclude. Findings drop to zero.
	resAfter, err := Inspect(context.Background(),
		filepath.Join("testdata", "contracts_excluded"),
		Options{
			Rules:    []Rule{RuleInternalPackageContractNames},
			Excludes: []string{"internal/packs"},
		},
	)
	if err != nil {
		t.Fatalf("Inspect (excluded): %v", err)
	}
	if len(resAfter) != 0 {
		t.Fatalf("expected 0 findings with exclude, got %d:\n%s",
			len(resAfter), AsResult(resAfter).FormatText())
	}
}

// TestInternalContracts_NoInternalDir verifies the analyzer is a no-op
// in projects without an internal/ directory (CLI/library kinds typically
// don't have one).
func TestInternalContracts_NoInternalDir(t *testing.T) {
	tmp := t.TempDir()
	fs, err := Inspect(context.Background(), tmp,
		Options{Rules: []Rule{RuleInternalPackageContractNames}},
	)
	if err != nil {
		t.Fatalf("Inspect on empty project: %v", err)
	}
	if len(fs) != 0 {
		t.Errorf("empty project should produce 0 findings, got %d", len(fs))
	}
}

// TestInternalContracts_NewSignatureRejectsPointerDeps verifies a
// `func New(*Deps) Service` shape is rejected — the bootstrap template
// emits `<pkg>.New(<pkg>.Deps{...})` (a value), so a pointer receiver
// signature would compile-fail at the call site.
func TestInternalContracts_NewSignatureRejectsPointerDeps(t *testing.T) {
	tmp := t.TempDir()
	pkgDir := filepath.Join(tmp, "internal", "ptr")
	must(t, mkdirAll(pkgDir))
	must(t, writeFile(filepath.Join(pkgDir, "contract.go"), `package ptr

type Service interface { Do() error }
type Deps struct{}

// Pointer parameter — bootstrap template won't compile against this.
func New(d *Deps) Service { return nil }
`))
	fs, err := Inspect(context.Background(), tmp,
		Options{Rules: []Rule{RuleInternalPackageContractNames}},
	)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	got := findingsForRule(fs, string(RuleInternalPackageContractNames))
	if len(got) != 1 {
		t.Fatalf("expected 1 finding for ptr-Deps mismatch, got %d:\n%s",
			len(got), AsResult(fs).FormatText())
	}
	if !strings.Contains(got[0].Message, "New") {
		t.Errorf("finding should reference New constructor; got: %s", got[0].Message)
	}
}

// TestInternalContracts_InterfaceCatalogueSkipped asserts that a
// package whose contract.go declares only narrow interfaces (>= 2,
// no Deps struct, no New func) is treated as an "interface catalogue"
// — a collection of contracts consumed elsewhere, not a Service-shape
// package the bootstrap binds to. No findings should fire, so the user
// doesn't have to add the package to contracts.exclude.
//
// FRICTION 2026-06-02: cp-forge layer-3 natsio shipped a contract.go
// with 7 narrow interfaces (Publisher, CommandHandler, three Runners,
// two Repositories) and the lint fired three findings; the user had to
// add the package to forge.yaml's contracts.exclude to silence them.
func TestInternalContracts_InterfaceCatalogueSkipped(t *testing.T) {
	tmp := t.TempDir()
	pkgDir := filepath.Join(tmp, "internal", "natsio")
	must(t, mkdirAll(pkgDir))
	must(t, writeFile(filepath.Join(pkgDir, "contract.go"), `package natsio

// A catalogue of narrow interfaces consumed by other packages. There
// is no canonical Service surface and no Deps/New trio — this package
// just describes contracts.

type Publisher interface {
	Publish(subject string, data []byte) error
}

type CommandHandler interface {
	HandleCommand(msg []byte) error
}

type EventConsumer interface {
	Consume(subject string) error
}
`))
	fs, err := Inspect(context.Background(), tmp,
		Options{Rules: []Rule{RuleInternalPackageContractNames}},
	)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	got := findingsForRule(fs, string(RuleInternalPackageContractNames))
	if len(got) != 0 {
		t.Fatalf("interface-catalogue package should produce 0 findings, got %d:\n%s",
			len(got), AsResult(fs).FormatText())
	}
}

// TestInternalContracts_SingleInterfaceStillFires guards the
// catalogue heuristic's lower bound: a single interface declaration
// (no Deps/New) is more likely an incomplete contract than a
// deliberate catalogue, so it should still fire all three findings.
func TestInternalContracts_SingleInterfaceStillFires(t *testing.T) {
	tmp := t.TempDir()
	pkgDir := filepath.Join(tmp, "internal", "incomplete")
	must(t, mkdirAll(pkgDir))
	must(t, writeFile(filepath.Join(pkgDir, "contract.go"), `package incomplete

// One interface, no Deps, no New — looks like an unfinished Service
// scaffold, not an interface catalogue. Lint must surface the gap.

type Sender interface {
	Send(b []byte) error
}
`))
	fs, err := Inspect(context.Background(), tmp,
		Options{Rules: []Rule{RuleInternalPackageContractNames}},
	)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	got := findingsForRule(fs, string(RuleInternalPackageContractNames))
	if len(got) != 3 {
		t.Fatalf("single-interface package should fire 3 findings (Service/Deps/New missing), got %d:\n%s",
			len(got), AsResult(fs).FormatText())
	}
}

// TestInternalContracts_UtilityPackagesSkipped is the table-driven
// guard for the zero-interface auto-skip (utility_skip.go). Each row
// models one of the recurring utility-package shapes that previously
// forced cp-forge porters to add `contracts.exclude` entries during
// migration: constants-only, structs-only, top-level-funcs-only, and a
// sibling file split. The two negative-control rows (genuine service
// package, Deps+New without Service) prove the auto-skip is
// conservative — it doesn't swallow real incomplete-Service bugs.
//
// FRICTION 2026-06-02 / 2026-06-03: cp-forge migration shipped at
// least eight utility-shaped packages with no service-shape surface
// (internal/config, internal/metrics, internal/billing/provideradapters,
// internal/db, internal/planlimits, internal/ratelimit, internal/natsio,
// internal/daemonstate). natsio/daemonstate are already covered by the
// interface-catalogue early-out; the rest are now covered here.
func TestInternalContracts_UtilityPackagesSkipped(t *testing.T) {
	cases := []struct {
		name    string
		pkgName string
		// files maps file basename → contents. Multiple files supported so
		// we can test the sibling-file split (constants in contract.go,
		// helpers in service.go) the way real utility packages ship.
		files       map[string]string
		wantSkipped bool
	}{
		{
			// internal/planlimits — pure constants/data.
			name:    "constants-only utility",
			pkgName: "planlimits",
			files: map[string]string{
				"contract.go": `package planlimits

const (
	MaxRequestsPerMinute = 60
	MaxUploadBytes       = 10 * 1024 * 1024
)

var DefaultTier = "free"
`,
			},
			wantSkipped: true,
		},
		{
			// internal/config — struct + Load func, no interface.
			name:    "structs and funcs utility",
			pkgName: "config",
			files: map[string]string{
				"contract.go": `package config

type Config struct {
	DatabaseURL string
	Port        int
}

func Load(path string) (*Config, error) { return nil, nil }
`,
			},
			wantSkipped: true,
		},
		{
			// internal/metrics — top-level funcs operating on package globals.
			name:    "top-level funcs only",
			pkgName: "metrics",
			files: map[string]string{
				"contract.go": `package metrics

func IncRequest(label string) {}
func ObserveLatency(label string, ms float64) {}
`,
			},
			wantSkipped: true,
		},
		{
			// internal/db — sibling-file split: contract.go declares the
			// types, helpers.go declares the funcs. The auto-skip must
			// look across all non-test, non-gen files in the package.
			name:    "sibling-file split utility",
			pkgName: "db",
			files: map[string]string{
				"contract.go": `package db

type Pool struct{}

type Tx struct{}
`,
				"helpers.go": `package db

func Open(dsn string) (*Pool, error) { return nil, nil }
func (p *Pool) Begin() (*Tx, error) { return nil, nil }
`,
			},
			wantSkipped: true,
		},
		{
			// Negative control: a genuine, canonical Service package
			// must NOT be auto-skipped. (If it were, the rule would be
			// useless.)
			name:    "genuine service still fires",
			pkgName: "email",
			files: map[string]string{
				"contract.go": `package email

type Sender interface { Send(b []byte) error }
type Config struct{}
func NewSender(c Config) Sender { return nil }
`,
			},
			wantSkipped: false,
		},
		{
			// Negative control: a package with the canonical `Service`
			// interface but missing `Deps` / `New` MUST still fire. The
			// auto-skip only triggers on zero-interface packages; once an
			// interface is declared the rule examines names normally.
			name:    "service interface without deps still fires",
			pkgName: "almost",
			files: map[string]string{
				"contract.go": `package almost

type Service interface { Do() error }
`,
			},
			wantSkipped: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmp := t.TempDir()
			pkgDir := filepath.Join(tmp, "internal", tc.pkgName)
			must(t, mkdirAll(pkgDir))
			for name, content := range tc.files {
				must(t, writeFile(filepath.Join(pkgDir, name), content))
			}
			fs, err := Inspect(context.Background(), tmp,
				Options{Rules: []Rule{RuleInternalPackageContractNames}},
			)
			if err != nil {
				t.Fatalf("Inspect: %v", err)
			}
			got := findingsForRule(fs, string(RuleInternalPackageContractNames))
			if tc.wantSkipped {
				if len(got) != 0 {
					t.Fatalf("utility package %q should be auto-skipped, got %d findings:\n%s",
						tc.pkgName, len(got), AsResult(fs).FormatText())
				}
			} else {
				if len(got) == 0 {
					t.Fatalf("service-shape package %q must fire (not be auto-skipped); got 0 findings",
						tc.pkgName)
				}
			}
		})
	}
}

// TestInternalContracts_StrategyDirectiveSkipped verifies the
// `//forge:strategy` directive opts a strategy-registry package out of
// the canonical Service/Deps/New enforcement.
//
// FRICTION 2026-06-03: kalshi `internal/algos` shipped as a strategy
// registry (one Strategy interface + multiple impl structs each with
// their own constructor / Deps shape) and required a contracts.exclude
// entry. The directive replaces the forge.yaml entry with an in-file
// opt-in.
func TestInternalContracts_StrategyDirectiveSkipped(t *testing.T) {
	fs, err := Inspect(context.Background(),
		filepath.Join("testdata", "contracts_strategy"),
		Options{Rules: []Rule{RuleInternalPackageContractNames}},
	)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	got := findingsForRule(fs, string(RuleInternalPackageContractNames))
	if len(got) != 0 {
		t.Fatalf("strategy-registry package with //forge:strategy directive should produce 0 findings, got %d:\n%s",
			len(got), AsResult(fs).FormatText())
	}
}

// TestInternalContracts_StrategyShapeWithoutDirectiveStillFires is the
// other half of the contract: the directive is OPT-IN. A package
// shaped like a strategy registry but missing the directive should
// still fire all three findings — we intentionally do not auto-detect
// the shape (too many false-positives on incomplete service scaffolds).
func TestInternalContracts_StrategyShapeWithoutDirectiveStillFires(t *testing.T) {
	fs, err := Inspect(context.Background(),
		filepath.Join("testdata", "contracts_strategy_missing_directive"),
		Options{Rules: []Rule{RuleInternalPackageContractNames}},
	)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	got := findingsForRule(fs, string(RuleInternalPackageContractNames))
	if len(got) != 3 {
		t.Fatalf("strategy-shaped package without directive should fire 3 findings (Service/Deps/New), got %d:\n%s",
			len(got), AsResult(fs).FormatText())
	}
}
