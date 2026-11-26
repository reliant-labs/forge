package cli

// `forge generate --scope <dir>`: refresh the derived contract files of the
// named packages, and nothing else.
//
// A package's contract_gen.go, mock_gen.go and middleware_gen.go are a
// function of that package's own source (its //forge:contract type or
// contract.go, its forge:deps deps.go). Adding one method to a consumer's
// Store interface used to cost a whole-project generate — buf, the schema
// projection, every frontend, and a validating compile of every package,
// including the half-edited packages of every other agent in the checkout —
// just to rewrite one mock (forge friction #36). --scope runs the "contracts"
// step preset over the named packages only:
//
//   - the SAME per-package writers the full run uses (deriveInternalContracts,
//     contract.GenerateWithOptions, contract.WriteObservedDecorator), so the
//     bytes are the full run's;
//   - through the pipeline: the project lock, the rollback journal, the
//     checksums chokepoint and manifest, goimports and the re-stamp of what
//     it wrote;
//   - validated by compiling the scoped packages (not the tree) without
//     linking. Any compile error, including in an imported dependency,
//     rolls the run back unless --keep-on-build-failure is set.
//
// A narrowed run is never recorded as a verified fixed point
// (generateCacheModeFor), and holds an express-lane heavy slot: its work is
// bounded by the packages it names.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cli/cmdutil"
	"github.com/reliant-labs/forge/internal/contractsrc"
	"github.com/reliant-labs/forge/internal/depsurface"
	"github.com/reliant-labs/forge/internal/goexec"
	"github.com/reliant-labs/forge/pkg/hostslot"
)

// contractsStepPreset names the step preset --scope runs (see
// stepPresetAllowlist).
const contractsStepPreset = "contracts"

// scopedValidateStep names the compile check of a --scope run.
const scopedValidateStep = "go build (validate scoped packages)"

// contractScope is the set of module-relative package directories a --scope
// run regenerates, each covering itself and every package beneath it. nil
// covers every package: the full run.
type contractScope []string

// covers reports whether the package at module-relative rel is in scope.
func (s contractScope) covers(rel string) bool {
	if s == nil {
		return true
	}
	for _, dir := range s {
		if rel == dir || strings.HasPrefix(rel, dir+"/") {
			return true
		}
	}
	return false
}

// leadsTo reports whether rel is an ancestor of a scoped directory: a walk
// must descend through it, generating nothing there.
func (s contractScope) leadsTo(rel string) bool {
	for _, dir := range s {
		if strings.HasPrefix(dir, rel+"/") {
			return true
		}
	}
	return false
}

// withScope narrows flags to the contract files of the packages in scope
// (already normalized by normalizeGenerateScope). No scope leaves flags as
// they are.
func (flags pipelineFlags) withScope(scope contractScope) pipelineFlags {
	if len(scope) == 0 {
		return flags
	}
	flags.Scope = scope
	flags.Steps = contractsStepPreset
	return flags
}

// markGenerateScopeExclusive refuses --scope beside the flags that ask for a
// different run: another step preset, the template-only pass, a whole-tree
// check or watch, and the drift and cleanup modes that act on the whole tree.
func markGenerateScopeExclusive(cmd *cobra.Command) {
	for _, other := range []string{"steps", "templates-only", "check", "watch", "explain-drift", "force-cleanup", "heal"} {
		cmd.MarkFlagsMutuallyExclusive("scope", other)
	}
}

// generateLockedInSlotFor is generateLockedInSlot, in the express lane for a
// --scope run: compiling a few packages is bounded work, and must not queue
// behind hour-long holders of the bulk lane.
func generateLockedInSlotFor(cmd *cobra.Command, flags pipelineFlags, fn func() error) error {
	if len(flags.Scope) == 0 {
		return generateLockedInSlot(cmd, fn)
	}
	release, err := cmdutil.HoldHeavySlotInLane(cmd.Context(), cmd.ErrOrStderr(), hostslot.LaneExpress)
	if err != nil {
		return err
	}
	defer release()
	return withGenerateLock(".", fn)
}

// normalizeGenerateScope turns --scope arguments into module-relative,
// slash-separated package directories, refusing any that names nothing
// --scope can regenerate. Accepted spellings: internal/foo, ./internal/foo/,
// ./internal/foo/... and an absolute path inside the project.
func normalizeGenerateScope(projectDir string, raw []string) (contractScope, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	absProject, err := filepath.Abs(projectDir)
	if err != nil {
		return nil, fmt.Errorf("resolve project dir: %w", err)
	}
	var scope contractScope
	for _, arg := range raw {
		p := strings.TrimSpace(arg)
		if p == "" {
			continue
		}
		p = strings.TrimSuffix(filepath.ToSlash(p), "/...")
		if filepath.IsAbs(p) {
			rel, relErr := filepath.Rel(absProject, p)
			if relErr != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				return nil, fmt.Errorf("--scope %q is outside the project", arg)
			}
			p = rel
		}
		p = filepath.ToSlash(filepath.Clean(p))
		if p != "internal" && !strings.HasPrefix(p, "internal/") {
			return nil, fmt.Errorf("--scope %q: only packages under internal/ have derived contract files "+
				"(contract_gen.go, mock_gen.go, middleware_gen.go); a proto, migration or config change needs a full `%s generate`", arg, Name())
		}
		abs := filepath.Join(absProject, filepath.FromSlash(p))
		if info, statErr := os.Stat(abs); statErr != nil || !info.IsDir() {
			return nil, fmt.Errorf("--scope %q: no such directory", arg)
		}
		if !hasContractPackage(abs) {
			return nil, fmt.Errorf("--scope %q: no package there has a contract to regenerate — a //forge:contract type, "+
				"a contract.go, or a deps.go whose first line is `// forge:deps`", arg)
		}
		if !slices.Contains(scope, p) {
			scope = append(scope, p)
		}
	}
	slices.Sort(scope)
	return scope, nil
}

// hasContractPackage reports whether dir, or a package beneath it, carries
// anything the contracts step derives files from — the same shapes
// generateInternalPackageContracts recognizes.
func hasContractPackage(dir string) bool {
	found := false
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || found || !d.IsDir() {
			return nil
		}
		if d.Name() == "testdata" {
			return filepath.SkipDir
		}
		if contractsrc.Path(path) != "" || contractsrc.HasMarkedType(path) || depsurface.HasDepsMarker(path) {
			found = true
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

// gateScoped passes for a --scope run.
func gateScoped(ctx *pipelineContext) bool { return len(ctx.Scope) > 0 }

// stepValidateScopedPackages compiles the scoped packages, without linking
// and without their tests: the files a --scope run writes are all non-test
// sources of those packages. A compile error in a scoped package fails the
// step — the run rolls back, or keeps its writes under
// --keep-on-build-failure, as the full run's compile check does. Errors in
// imported dependencies also fail validation: the scoped packages cannot be
// validated until those dependencies compile.
func stepValidateScopedPackages(ctx *pipelineContext) error {
	fmt.Printf("\n🔨 Validating %s...\n", strings.Join(ctx.Scope, ", "))
	rep, err := compileScopedPackages(ctx.ProjectDir, ctx.Scope)
	if err != nil {
		return goBuildValidateFailure(ctx.ProjectDir, err.Error(), err)
	}
	output := rep.Scoped + rep.Elsewhere
	if output == "" {
		fmt.Println("  ✅ The scoped packages compile")
		return nil
	}
	fmt.Fprint(os.Stderr, output)
	verr := goBuildValidateFailure(ctx.ProjectDir, output, errors.New("the scoped packages or their dependencies do not compile"))
	if !ctx.KeepOnBuildFailure {
		return verr
	}
	ctx.keptBuildFailure = verr
	return ctx.warnOrFail(scopedValidateStep, verr)
}

// scopedCompileReport splits a scoped compile's errors by where they are.
type scopedCompileReport struct {
	// Scoped is the compiler output for packages inside the scope.
	Scoped string
	// Elsewhere is the output for packages outside it that the scoped
	// packages import.
	Elsewhere string
}

// compileScopedPackages compiles every package under the scoped directories,
// and what they import, through `go list -e -export -deps` — the compile
// without the link that the full run's check uses (generate_validate_compile.go).
// Each package's own Error is attributed by its directory; DepsErrors only
// repeat a dependency's, so they are not read.
func compileScopedPackages(projectDir string, scope contractScope) (scopedCompileReport, error) {
	args := []string{"list", "-e", "-export", "-deps", "-json=" + compileFields}
	for _, dir := range scope {
		args = append(args, "./"+dir+"/...")
	}
	cmd := exec.Command("go", args...)
	cmd.Dir = projectDir
	cmd.Env = goexec.CheckEnv()
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return scopedCompileReport{}, &listFailure{stderr: stderr.String(), err: err}
	}
	absProject, err := filepath.Abs(projectDir)
	if err != nil {
		return scopedCompileReport{}, err
	}
	var scoped, elsewhere []string
	seen := map[string]bool{}
	dec := json.NewDecoder(&stdout)
	for {
		p := new(listedPackage)
		if err := dec.Decode(p); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return scopedCompileReport{}, &listFailure{stderr: stderr.String(), err: fmt.Errorf("decode go list output: %w", err)}
		}
		if p.Error == nil {
			continue
		}
		t := p.Error.text()
		if seen[t] {
			continue
		}
		seen[t] = true
		if rel, relErr := filepath.Rel(absProject, packageErrorDir(absProject, p)); relErr == nil && scope.covers(filepath.ToSlash(rel)) {
			scoped = append(scoped, t)
		} else {
			elsewhere = append(elsewhere, t)
		}
	}
	var rep scopedCompileReport
	if len(scoped) > 0 {
		rep.Scoped = strings.Join(scoped, "\n") + "\n"
	}
	if len(elsewhere) > 0 {
		rep.Elsewhere = strings.Join(elsewhere, "\n") + "\n"
	}
	return rep, nil
}

// packageErrorDir is the directory p's error is in: the file its position
// cites when it has one — an import no module provides is reported on the
// missing package, which has no directory, at the importing file's line —
// else the package's own directory.
func packageErrorDir(absProject string, p *listedPackage) string {
	if file, _, ok := strings.Cut(p.Error.Pos, ":"); ok && file != "" {
		if !filepath.IsAbs(file) {
			file = filepath.Join(absProject, file)
		}
		return filepath.Dir(file)
	}
	return p.Dir
}
