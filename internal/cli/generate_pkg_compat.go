package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/mod/module"
	"golang.org/x/mod/semver"

	"github.com/reliant-labs/forge/internal/buildinfo"
	"github.com/reliant-labs/forge/internal/cliutil"
)

// forge↔project compatibility check (kalshi fr-ac69216583).
//
// The generator emits code that calls into forge/pkg/* — orm, crud, testkit,
// serverkit. Those are packages in the SINGLE forge module, so the library
// that code compiles against is whatever version the project requires, and
// the symbols it may call are the ones that existed as of THIS binary's
// version. A project pinning a forge OLDER than this binary can therefore
// lack symbols the generated code names. Without this check, `forge generate`
// rewrites the whole tree and only THEN fails its own `go build` validate
// with `undefined: ...`, leaving the repo mid-regen.
//
// WHY THIS IS A VERSION COMPARISON AND NOT A SYMBOL PROBE. It used to compile
// a throwaway program against the project's forge/pkg, exercising a
// hand-maintained list of symbols (`requiredPkgSymbols`) that whoever taught
// an emitter to call something new was expected to extend. That list is
// exactly as good as the memory of the person editing the emitter, and it
// rotted: forge grew `testkit.StubNotConfigured`, nobody added the entry, and
// a control-plane `forge generate` failed in validate with the tree already
// rewritten — the precise outcome the probe existed to prevent.
//
// Generator and runtime ship as one module now, so their versions are one
// number and the honest check is an inequality over it: the project's forge
// must be >= the forge doing the generating. That covers every symbol ever
// added, with no registry to forget, and costs one `go list` instead of a
// `go build`.
//
// A project resolving forge NEWER than this binary is allowed through: older
// templates calling into a newer library is the ordinary upgrade order, and
// only a symbol REMOVAL breaks it — which semver is the right place to
// signal, not a pre-codegen gate.

// forgeModuleRequirePath is the module a project requires to get forge's
// runtime libraries. `forge/pkg` is a package prefix inside it, not a module.
const forgeModuleRequirePath = "github.com/reliant-labs/forge"

// legacyForgePkgRequireRE matches a direct require on that module.
var legacyForgePkgRequireRE = regexp.MustCompile(
	`(?m)^[\t ]*(?:require[\t ]+)?github\.com/reliant-labs/forge/pkg[\t ]+(v[^\s]+)[\t ]*$`)

// checkPkgCompat verifies, BEFORE codegen mutates the tree, that the forge
// this project compiles against can satisfy the code this binary generates.
//
// Returns a user-facing error (tree untouched) on a genuine mismatch, and nil
// whenever the question doesn't apply or can't be answered — no go.mod, no
// forge requirement, or a toolchain that won't tell us how forge resolved.
// generate's existing validate step remains the backstop for those.
func checkPkgCompat(projectDir string) error {
	data, err := os.ReadFile(filepath.Join(projectDir, "go.mod"))
	if err != nil {
		return nil // no module → nothing to check; not our error to raise
	}
	gomod := string(data)

	// A DIRECT require on the retired submodule is fatal and produces the
	// least legible error in Go: both github.com/reliant-labs/forge (which
	// now contains pkg/) and github.com/reliant-labs/forge/pkg provide the
	// import path github.com/reliant-labs/forge/pkg/<x>, so requiring both
	// answers every such import with
	//
	//	ambiguous import: found package github.com/reliant-labs/forge/pkg/orm
	//	in multiple modules
	//
	// repeated once per import, naming no cause and no fix. Reading this
	// project's OWN go.mod is in scope; going looking for the same module
	// elsewhere in the dependency graph is not — see
	// directRetiredPkgRequire.
	if version, found := directRetiredPkgRequire(gomod); found {
		return retiredPkgModuleErr(projectDir, version)
	}
	if !strings.Contains(gomod, forgeModuleRequirePath) {
		return nil // project doesn't consume forge's libraries
	}

	projectVersion, local, ok := resolveProjectForge(projectDir)
	if !ok {
		return nil // can't ask the toolchain → defer to the validate backstop
	}

	binaryVersion := buildinfo.InstallableVersion()
	switch decideForgeCompat(binaryVersion, projectVersion, local) {
	case compatUnreleasableNoBridge:
		return unreleasableBuildErr(projectDir, projectVersion)
	case compatStalePin:
		return staleForgePinErr(projectDir, projectVersion, binaryVersion)
	}
	return nil
}

// compatVerdict is the outcome of the version comparison, split from both the
// toolchain query and the error prose so the decision table can be tested
// without a module graph (mirroring isDevBuildFrom / forgeRootFromFile).
type compatVerdict int

const (
	compatOK compatVerdict = iota
	// compatUnreleasableNoBridge: this binary exists on no module proxy and
	// the project resolves forge to a published version — the pairing that
	// produced `undefined: testkit.StubNotConfigured`.
	compatUnreleasableNoBridge
	// compatStalePin: the project's forge is older than the binary generating
	// into it.
	compatStalePin
)

// decideForgeCompat is the pure decision.
//
// binaryVersion is buildinfo.InstallableVersion() — "" for a build no proxy
// can serve. projectVersion/local describe how forge resolves IN the project.
//
// A local resolution always passes: the project compiles against source, so
// there is no version to be behind, and whoever wired the bridge owns keeping
// that checkout coherent. An unknown projectVersion also passes — guessing
// is worse than letting validate speak.
func decideForgeCompat(binaryVersion, projectVersion string, local bool) compatVerdict {
	if local {
		return compatOK
	}
	if binaryVersion == "" {
		return compatUnreleasableNoBridge
	}
	if projectVersion == "" {
		return compatOK
	}
	if semver.Compare(projectVersion, binaryVersion) < 0 {
		return compatStalePin
	}
	return compatOK
}

// directRetiredPkgRequire returns the version of the retired forge/pkg module
// if THIS PROJECT requires it directly, reading only its go.mod.
//
// IT DELIBERATELY DOES NOT LOOK AT THE DEPENDENCY GRAPH. It used to: it ran
// `go list -m github.com/reliant-labs/forge/pkg`, which resolves the whole
// build list, and refused to generate when the retired module appeared
// anywhere — then told the user to run `go mod why` and go fix a dependency.
//
// That is not forge's call to make. control-plane imports reliant, which
// imports forge, so that probe failed control-plane's generate because
// RELIANT had not bumped, while control-plane's own code and pins were
// correct. For anyone outside this org it is worse: "a dependency of yours
// requires forge/pkg, go fix that dependency" is advice they cannot act on,
// from a tool claiming authority over a graph it does not own — to cover a
// migration window that ages out.
//
// go.mod resolution is sufficient here, and MVS is why: Go selects ONE
// maximum version of forge for the whole build, so the library side is
// coherent no matter what a project and somebody else's library each pin. A
// real mismatch between the generating binary and that selected library is an
// ordinary compile error, and forge already answers that EMPIRICALLY by
// emitting, validating, and rolling back — which beats predicting it from
// version strings.
//
// A DIRECT require stays in scope: it is the project's own declaration, it
// makes every forge/pkg/* import ambiguous so nothing can build, and
// `go mod edit -droprequire` is something the reader can actually run.
func directRetiredPkgRequire(gomod string) (version string, found bool) {
	if m := legacyForgePkgRequireRE.FindStringSubmatch(gomod); m != nil {
		return m[1], true
	}
	return "", false
}

// resolveProjectForge asks the go toolchain how forge ACTUALLY resolves for
// this project — which is not necessarily what go.mod says, because a go.work
// or a replace can override it.
//
// local is true when forge resolves to a directory on disk (a workspace `use`
// or a directory `replace`) rather than a published version: a module in a
// workspace has no version, which is the signal. ok is false when the
// toolchain could not answer at all.
func resolveProjectForge(projectDir string) (version string, local, ok bool) {
	cmd := exec.Command("go", "list", "-m",
		"-f", "{{.Version}}|{{with .Replace}}{{.Version}}|{{.Path}}{{end}}",
		forgeModuleRequirePath)
	cmd.Dir = projectDir
	out, err := cmd.Output()
	if err != nil {
		return "", false, false
	}
	fields := strings.Split(strings.TrimSpace(string(out)), "|")
	version = strings.TrimSpace(fields[0])
	// A replace wins: its version (empty for a directory target) is what the
	// build actually compiles.
	if len(fields) >= 3 {
		replaceVersion, replacePath := strings.TrimSpace(fields[1]), strings.TrimSpace(fields[2])
		if replaceVersion == "" && replacePath != "" {
			return "", true, true // replaced by a directory
		}
		if replaceVersion != "" {
			version = replaceVersion
		}
	}
	if version == "" {
		return "", true, true // in the workspace: no version at all
	}
	if !semver.IsValid(version) {
		return "", false, false
	}
	return version, false, true
}

// unreleasableBuildErr is the error for the case that used to produce
// `undefined: testkit.StubNotConfigured` deep in validate: an unreleased
// forge generating into a project pinned to a published one.
func unreleasableBuildErr(projectDir, projectVersion string) error {
	root := buildinfo.DevForgeRoot
	if root == "" {
		root = buildinfo.DiscoverDevForgeRootFromSource()
	}
	bridge := "go work init . && go work use . <path-to-your-forge-checkout>"
	if root != "" {
		bridge = fmt.Sprintf("go work init . && go work use . %s", root)
	}

	base := cliutil.UserErr("forge generate (forge version compatibility)",
		fmt.Sprintf("this forge is %s — an unreleased build no module proxy can serve — but the project "+
			"resolves %s to the published %s. The generated code would call into a forge this project "+
			"cannot fetch, so generating would rewrite the tree and then fail its own validate. "+
			"No files were changed",
			buildinfo.Version(), forgeModuleRequirePath, projectVersion),
		"",
		"bridge the project to this forge's source, which is the supported way to generate with an "+
			"unreleased forge:\n    "+bridge+
			"\n  (go.work is machine-local — keep it out of version control.) "+
			"Or install a released forge and use that instead")

	return fmt.Errorf("%w\n\n%s", base, toolchainDiagnosis(projectDir))
}

// staleForgePinErr is the ordinary skew: a released forge newer than the
// project's pin.
func staleForgePinErr(projectDir, projectVersion, binaryVersion string) error {
	fix := fmt.Sprintf("if the pin is genuinely behind this binary, bring it up:"+
		"\n    go get %s@%s && go mod tidy"+
		"\n  (and re-run both in gen/ if the project has one). "+
		"If the two toolchains below disagree, the pin is not the problem — re-run with the SAME "+
		"forge that generated this tree, or reinstall both so they match",
		forgeModuleRequirePath, binaryVersion)

	// A pseudo-version names a COMMIT, and a proxy can only serve it once
	// that commit is pushed. Telling someone to `go get` an unpushed one sends
	// them to "unknown revision" — the same class of mistake as pinning a
	// version that cannot satisfy the generated code, which is what this whole
	// check exists to prevent. We cannot know from here whether it is pushed
	// (that needs the network, or knowledge of the remote), so say so and name
	// the alternative rather than asserting a ref that may not exist.
	if module.IsPseudoVersion(binaryVersion) {
		fix += fmt.Sprintf("\n  NOTE: %s is a pseudo-version — it names commit %s, and `go get` can only "+
			"resolve it once that commit is PUSHED. If it is not, bridge to the source instead:"+
			"\n    go work init . && go work use . <path-to-your-forge-checkout>",
			binaryVersion, shortPseudoCommit(binaryVersion))
	}

	base := cliutil.UserErr("forge generate (forge version compatibility)",
		fmt.Sprintf("this forge is %s but the project pins %s %s — older than the binary generating into "+
			"it, so the generated code may call symbols that release does not have. Generating would "+
			"rewrite the tree and then fail its own validate. No files were changed",
			binaryVersion, forgeModuleRequirePath, projectVersion),
		"", fix)

	return fmt.Errorf("%w\n\n%s", base, toolchainDiagnosis(projectDir))
}

// shortPseudoCommit pulls the commit out of a pseudo-version for the message,
// degrading to the whole version string if the shape is unexpected — a
// diagnostic must never be the thing that fails.
func shortPseudoCommit(v string) string {
	if rev, err := module.PseudoVersionRev(v); err == nil && rev != "" {
		return rev
	}
	return v
}

// retiredPkgModuleErr names the pre-collapse submodule. Only the DIRECT case
// reaches here — see directRetiredPkgRequire for why forge does not go looking
// for it in the graph.
func retiredPkgModuleErr(projectDir, version string) error {
	target := buildinfo.InstallableVersion()
	if target == "" {
		target = "latest"
	}

	what := fmt.Sprintf("this project's go.mod requires the retired module "+
		"github.com/reliant-labs/forge/pkg %s. It was merged into "+
		"github.com/reliant-labs/forge, and BOTH modules provide the import path "+
		"github.com/reliant-labs/forge/pkg/* — so every such import is ambiguous and nothing in the "+
		"project compiles. Import paths did NOT change; only the require line did. No files were "+
		"changed", version)

	fix := fmt.Sprintf("swap the requirement, in the root module and in gen/ if there is one:"+
		"\n    go mod edit -droprequire=github.com/reliant-labs/forge/pkg"+
		"\n    go get github.com/reliant-labs/forge@%s && go mod tidy", target)

	base := cliutil.UserErr("forge generate (forge version compatibility)", what, "", fix)
	return fmt.Errorf("%w\n\n%s", base, toolchainDiagnosis(projectDir))
}
