package lint

// In-process contract analysis — the version-skew fix for `forge lint
// --contract`.
//
// forge used to shell out to a separately-installed `contractlint`
// binary (PATH, then bin/, then `go run`), an artifact of the era when
// x/tools only exposed the analysis framework through
// multichecker.Main. A ~/go/bin contractlint installed weeks ago could
// silently disagree with a freshly-built forge — fresh forge + stale
// contractlint produced phantom violations with no version handshake to
// catch the skew. x/tools now exposes a programmatic driver
// (go/analysis/checker), so the analyzers — which live in-tree at
// internal/linter/contract — run inside the forge binary itself: the
// analysis is exactly as new as forge, with no PATH lookup and no
// possible skew. cmd/contractlint still ships for `go vet -vettool=`
// integration; it registers the same analyzer set.

import (
	"context"
	"fmt"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/analysis"
	"golang.org/x/tools/go/analysis/checker"
	"golang.org/x/tools/go/packages"

	"github.com/reliant-labs/forge/internal/linter/contract"
)

// contractAnalyzers is the analyzer set `forge lint --contract` runs —
// identical to what cmd/contractlint registers with multichecker.
func contractAnalyzers() []*analysis.Analyzer {
	return []*analysis.Analyzer{
		contract.Analyzer,
		contract.RequireContractAnalyzer,
		contract.ExportedVarsAnalyzer,
	}
}

// contractDiagnostic is one analyzer diagnostic with its position
// resolved (relative to the working directory when possible).
type contractDiagnostic struct {
	Analyzer string
	Pos      token.Position
	Message  string
}

func (d contractDiagnostic) String() string {
	return fmt.Sprintf("%s: %s (%s)", d.Pos, d.Message, d.Analyzer)
}

// runContractAnalysisInProcess loads the requested packages and runs
// the contract analyzers in-process, returning position-sorted
// diagnostics. A load or analyzer failure is an error; diagnostics are
// findings, not errors.
func runContractAnalysisInProcess(ctx context.Context, paths, excludes []string) ([]contractDiagnostic, error) {
	// The exclude list mirrors forge.yaml's contracts.exclude — same
	// registration cmd/contractlint performs before multichecker.Main.
	contract.SetExcludes(excludes)

	// Same module-resolution discipline the subprocess used: outside a
	// go.work workspace, pin GOWORK=off and -mod=mod so the loader can
	// fetch missing modules; inside one (forge itself), honour the
	// workspace so the local pkg/ checkout resolves.
	env := os.Environ()
	if !hasWorkspaceGoMod() {
		env = appendEnvIfUnset(env, "GOWORK", "off")
		env = appendEnvIfUnset(env, "GOFLAGS", "-mod=mod")
	}
	env = ensureEnvDefault(env, "GOPROXY", "https://proxy.golang.org,direct")

	cfg := &packages.Config{
		Context: ctx,
		// checker.Analyze requires LoadAllSyntax-equivalent data.
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedImports | packages.NeedDeps | packages.NeedTypes |
			packages.NeedSyntax | packages.NeedTypesInfo | packages.NeedTypesSizes |
			packages.NeedModule,
		Env: env,
		// Parity with the retired multichecker driver (-test=true):
		// test files are analyzed too.
		Tests: true,
	}
	pkgs, err := packages.Load(cfg, paths...)
	if err != nil {
		return nil, fmt.Errorf("load packages %v: %w", paths, err)
	}

	// Broken packages mean the type information the analyzers depend on
	// is incomplete — fail like the subprocess driver did rather than
	// reporting half-analyzed results.
	var loadErrs []string
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		for _, e := range p.Errors {
			loadErrs = append(loadErrs, e.Error())
		}
	})
	if len(loadErrs) > 0 {
		const maxShown = 5
		shown := loadErrs
		if len(shown) > maxShown {
			shown = append(append([]string{}, shown[:maxShown]...), fmt.Sprintf("… and %d more", len(loadErrs)-maxShown))
		}
		return nil, fmt.Errorf("packages failed to load:\n  %s", strings.Join(shown, "\n  "))
	}

	graph, err := checker.Analyze(contractAnalyzers(), pkgs, nil)
	if err != nil {
		return nil, fmt.Errorf("contract analysis: %w", err)
	}

	cwd, _ := os.Getwd()
	seen := map[string]bool{}
	var out []contractDiagnostic
	for act := range graph.All() {
		if !act.IsRoot {
			continue
		}
		if act.Err != nil {
			return nil, fmt.Errorf("%s on %s: %w", act.Analyzer.Name, act.Package.PkgPath, act.Err)
		}
		for _, d := range act.Diagnostics {
			pos := act.Package.Fset.Position(d.Pos)
			if cwd != "" {
				if rel, rerr := filepath.Rel(cwd, pos.Filename); rerr == nil && !strings.HasPrefix(rel, "..") {
					pos.Filename = rel
				}
			}
			// Tests:true loads each package again as its test variant;
			// dedupe the identical diagnostics that produces.
			key := fmt.Sprintf("%s|%d|%d|%s|%s", pos.Filename, pos.Line, pos.Column, act.Analyzer.Name, d.Message)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, contractDiagnostic{Analyzer: act.Analyzer.Name, Pos: pos, Message: d.Message})
		}
	}

	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Pos.Filename != b.Pos.Filename {
			return a.Pos.Filename < b.Pos.Filename
		}
		if a.Pos.Line != b.Pos.Line {
			return a.Pos.Line < b.Pos.Line
		}
		if a.Pos.Column != b.Pos.Column {
			return a.Pos.Column < b.Pos.Column
		}
		return a.Analyzer < b.Analyzer
	})
	return out, nil
}
