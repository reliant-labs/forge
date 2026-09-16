package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

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

// legacyForgePkgRequireRE matches a require on the RETIRED forge/pkg
// submodule, which no longer exists as a module. A project still pinning it
// predates the single-module collapse and cannot resolve at all.
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

	// A project still pinning the retired submodule cannot build at all, and
	// the module proxy's error for it is opaque. Say what happened.
	if m := legacyForgePkgRequireRE.FindStringSubmatch(gomod); m != nil &&
		!strings.Contains(gomod, forgeModuleRequirePath+" v") {
		return legacyPkgPinErr(projectDir, m[1])
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
	base := cliutil.UserErr("forge generate (forge version compatibility)",
		fmt.Sprintf("this forge is %s but the project pins %s %s — older than the binary generating into "+
			"it, so the generated code may call symbols that release does not have. Generating would "+
			"rewrite the tree and then fail its own validate. No files were changed",
			binaryVersion, forgeModuleRequirePath, projectVersion),
		"",
		fmt.Sprintf("if the pin is genuinely behind this binary, bring it up:"+
			"\n    go get %s@%s && go mod tidy"+
			"\n  (and re-run both in gen/ if the project has one). "+
			"If the two toolchains below disagree, the pin is not the problem — re-run with the SAME "+
			"forge that generated this tree, or reinstall both so they match",
			forgeModuleRequirePath, binaryVersion))

	return fmt.Errorf("%w\n\n%s", base, toolchainDiagnosis(projectDir))
}

// legacyPkgPinErr names the pre-collapse two-module pin, whose proxy error
// ("module github.com/reliant-labs/forge/pkg: no matching versions") says
// nothing about what to do.
func legacyPkgPinErr(projectDir, pinnedVersion string) error {
	target := buildinfo.InstallableVersion()
	if target == "" {
		target = "latest"
	}
	base := cliutil.UserErr("forge generate (forge version compatibility)",
		fmt.Sprintf("the project requires github.com/reliant-labs/forge/pkg %s, a module that no longer "+
			"exists: forge/pkg was merged into github.com/reliant-labs/forge, so the CLI and the runtime "+
			"libraries can never disagree about a version again. Import paths did NOT change — only the "+
			"require line. No files were changed", pinnedVersion),
		"",
		fmt.Sprintf("swap the requirement (in the root module and in gen/ if the project has one):"+
			"\n    go mod edit -droprequire=github.com/reliant-labs/forge/pkg"+
			"\n    go get github.com/reliant-labs/forge@%s && go mod tidy", target))

	return fmt.Errorf("%w\n\n%s", base, toolchainDiagnosis(projectDir))
}
