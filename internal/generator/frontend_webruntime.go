// Package generator — @reliantlabs/forge-web-runtime dependency resolution for
// generated frontends.
//
// Generated frontends import @reliantlabs/forge-web-runtime (the web twin of
// forge/pkg): connect.ts builds its interceptor stack from it, providers.tsx
// mounts RuntimeShell, the CRUD list pages render <Resource>. It is therefore
// a REAL dependency of shipped application code and belongs in
// "dependencies", never "devDependencies" — a production install
// (`npm ci --omit=dev`, the Next.js standalone Docker image) has to resolve
// it or `next build` fails.
//
// Which specifier to write is the whole job here, and it is the npm mirror of
// what project_pkgdep.go decides for forge/pkg:
//
//   - RELEASE flow: an ordinary semver range (webRuntimePublishedRange).
//     A released forge scaffolds a project that resolves the package from the
//     registry like any other dependency. A release build never emits a
//     `file:` specifier — the path would be meaningless on the user's disk.
//
//   - DEV flow: `file:<path-to-the-running-forge's-web-runtime>`. npm's
//     file: protocol symlinks the directory into node_modules, so a dev
//     forge's frontend resolves the package straight out of the checkout —
//     edits to web-runtime/src land in the project with nothing published,
//     nothing pushed and no reinstall. Because the dependency is DECLARED,
//     `npm install` maintains the link instead of pruning it, which a bare
//     symlink into node_modules could never survive.
//
// The dev path is written RELATIVE ("../../../../forge/web-runtime") or
// home-anchored ("~/src/forge/web-runtime") — never absolute. An absolute
// path carries the maintainer's home directory and username into a committed
// file, and npm resolves both of the other forms natively. When neither form
// can be expressed without embedding one, forge writes no dev specifier at
// all and says why.
package generator

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/reliant-labs/forge/internal/buildinfo"
)

// WebRuntimePackage is the npm package name of forge's frontend runtime
// library.
const WebRuntimePackage = "@reliantlabs/forge-web-runtime"

// webRuntimePublishedRange is the semver range a scaffold declares when it is
// not bridged to a local checkout. Keep the major.minor pointed at
// web-runtime/package.json's version — TestWebRuntimePublishedRangeTracksPackage
// fails the build when the two drift apart.
const webRuntimePublishedRange = "^0.3.0"

// webRuntimeDepRe matches the package's entry wherever it already appears in a
// package.json, capturing everything up to (but not including) the value so a
// rewrite preserves the surrounding formatting byte for byte. The package name
// is unique in the file, so a single match is the entry.
var webRuntimeDepRe = regexp.MustCompile(`"` + regexp.QuoteMeta(WebRuntimePackage) + `"\s*:\s*("[^"]*")`)

// dependenciesOpenRe matches the opening brace of the top-level
// "dependencies" object, the insertion point for a frontend that predates the
// declared dependency.
var dependenciesOpenRe = regexp.MustCompile(`"dependencies"\s*:\s*\{`)

// webRuntimeDecision is what forge wants a frontend's package.json to say
// about the runtime package.
type webRuntimeDecision struct {
	// spec is the desired specifier, e.g. "^0.1.0" or "file:../../forge/web-runtime".
	spec string
	// authoritative reports whether forge should overwrite a DIFFERENT value
	// that is already there. False means "add the entry if it is missing, but
	// leave an existing one alone": a dev build that cannot locate its own
	// source tree has no business replacing a bridge somebody else set up.
	authoritative bool
	// hint is a one-line explanation printed when forge declines to bridge.
	hint string
}

// decideWebRuntimeDependency resolves the specifier for the frontend rooted at
// feAbs (an absolute path to frontends/<name>).
func decideWebRuntimeDependency(feAbs string) webRuntimeDecision {
	if !buildinfo.IsDevBuild() {
		// Released binary: an ordinary range, publish-ready, no local paths.
		return webRuntimeDecision{spec: webRuntimePublishedRange, authoritative: true}
	}

	// Prefer the explicitly stamped ldflag; otherwise recover the source root
	// from this binary's own compiled file paths, which is what makes the
	// bridge work when forge runs EMBEDDED in a host binary that never
	// stamped DevForgeRoot.
	root := buildinfo.DevForgeRoot
	if root == "" {
		root = buildinfo.DiscoverDevForgeRootFromSource()
	}
	if root == "" {
		return webRuntimeDecision{
			spec: webRuntimePublishedRange,
			hint: fmt.Sprintf("dev forge build without a discoverable source root: %s is left unbridged. "+
				"Rebuild forge with `make dev` (injects DevForgeRoot) to enable the bridge.", WebRuntimePackage),
		}
	}

	target := filepath.Join(root, "web-runtime")
	if _, err := os.Stat(filepath.Join(target, "package.json")); err != nil {
		// A forge checkout that does not carry the package. Declare the
		// published range if nothing is declared, but never overwrite.
		return webRuntimeDecision{spec: webRuntimePublishedRange}
	}

	home, _ := os.UserHomeDir()
	spec := webRuntimeFileSpec(resolvePath(feAbs), resolvePath(target), resolvePath(home))
	if spec == "" {
		return webRuntimeDecision{
			spec: webRuntimePublishedRange,
			hint: fmt.Sprintf("%s at %s cannot be reached by a relative or ~-anchored path from this project; "+
				"leaving the declared version range in place (forge never writes an absolute path into package.json).",
				WebRuntimePackage, target),
		}
	}
	return webRuntimeDecision{spec: spec, authoritative: true}
}

// webRuntimeFileSpec builds the npm `file:` specifier a frontend at feAbs uses
// to reach the web-runtime directory at targetAbs.
//
// It prefers a plain relative path: npm records that verbatim in
// package-lock.json too, so neither committed file learns anything about the
// machine. A relative path is only usable when it does not have to climb ABOVE
// the home directory and descend back through it — "../../../Users/<name>/..."
// re-embeds the username, which is exactly what must never be written. In that
// case it falls back to npm's "~/"-anchored form. Returning "" means neither
// form is expressible and the caller must not write a dev specifier.
func webRuntimeFileSpec(feAbs, targetAbs, home string) string {
	if rel, err := filepath.Rel(feAbs, targetAbs); err == nil && !pathLeaksHome(rel, feAbs, targetAbs, home) {
		return "file:" + filepath.ToSlash(rel)
	}
	if home != "" && isUnder(targetAbs, home) {
		if rel, err := filepath.Rel(home, targetAbs); err == nil {
			return "file:~/" + filepath.ToSlash(rel)
		}
	}
	return ""
}

// pathLeaksHome reports whether the relative path from feAbs to targetAbs
// spells out part of the home directory — the username above all.
//
// Structurally: a relative path is "up to the common ancestor, then down to
// the target". The up half is only "..". The down half is the target's own
// segments below that ancestor. So the path can only name the home directory
// when the target lives under home AND the common ancestor sits STRICTLY above
// home — then the descent must pass through home's own name.
//
// The literal-substring checks that follow are a belt-and-braces guard for the
// odd tree that repeats the username outside $HOME; a false positive there
// costs nothing but the ~-anchored form, which is equally correct.
func pathLeaksHome(rel, feAbs, targetAbs, home string) bool {
	if home == "" {
		return false
	}
	if ancestor := commonAncestor(feAbs, targetAbs); isUnder(targetAbs, home) && isUnder(home, ancestor) && home != ancestor {
		return true
	}
	if strings.Contains(rel, home) {
		return true
	}
	user := filepath.Base(home)
	for _, seg := range strings.Split(filepath.ToSlash(rel), "/") {
		if seg == user {
			return true
		}
	}
	return false
}

// resolvePath makes p absolute and resolves symlinks, best-effort: the
// comparisons in webRuntimeFileSpec only mean anything on canonical paths
// (macOS's /tmp is a symlink to /private/tmp, so an unresolved temp project
// would compute a relative path that npm then resolves differently).
func resolvePath(p string) string {
	if p == "" {
		return ""
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

// isUnder reports whether p is dir or lives beneath it.
func isUnder(p, dir string) bool {
	if p == "" || dir == "" {
		return false
	}
	if p == dir {
		return true
	}
	return strings.HasPrefix(p, strings.TrimSuffix(dir, string(filepath.Separator))+string(filepath.Separator))
}

// commonAncestor returns the deepest directory that contains both paths.
func commonAncestor(a, b string) string {
	as := strings.Split(filepath.Clean(a), string(filepath.Separator))
	bs := strings.Split(filepath.Clean(b), string(filepath.Separator))
	n := len(as)
	if len(bs) < n {
		n = len(bs)
	}
	i := 0
	for i < n && as[i] == bs[i] {
		i++
	}
	joined := strings.Join(as[:i], string(filepath.Separator))
	if joined == "" {
		return string(filepath.Separator)
	}
	return joined
}

// EnsureWebRuntimeDependency reconciles the @reliantlabs/forge-web-runtime entry in
// frontends/<name>/package.json so that npm — not forge — owns the link under
// node_modules.
//
// It runs at scaffold time (before the first `npm install`, so the very first
// install resolves the bridge) and again on every `forge generate`, which is
// what keeps it honest when a project is moved on disk or picked up by a
// differently-built forge. The edit is surgical: the entry's VALUE is replaced
// in place, so a hand-formatted, hand-extended package.json survives
// untouched, and a run that changes nothing writes nothing.
//
// Every failure is a warning. A frontend whose package.json forge cannot parse
// is the user's file to fix, and no dependency bookkeeping justifies failing
// their build.
func EnsureWebRuntimeDependency(projectDir, feRelDir, feName string) {
	ensureWebRuntimeDependency(projectDir, feRelDir, "frontend "+feName)
}

// EnsureWorkspaceHooksWebRuntimeDependency reconciles the same entry in
// packages/hooks/package.json — the workspace-mode home of the generated
// hooks. Those hooks type every error as ConnectClientError, imported from the
// runtime package, and a type-only import still has to RESOLVE: under pnpm's
// strict node_modules an undeclared package is simply not there. Declaring it
// is what makes the workspace layout typecheck.
//
// No-op when the manifest does not exist, so a non-workspace project calls it
// harmlessly.
func EnsureWorkspaceHooksWebRuntimeDependency(projectDir string) {
	ensureWebRuntimeDependency(projectDir, filepath.Join("packages", "hooks"), "packages/hooks")
}

// ensureWebRuntimeDependency is the shared body. `label` names the manifest's
// package in the one line this prints when it writes a dev bridge.
func ensureWebRuntimeDependency(projectDir, relDir, label string) {
	pkgPath := filepath.Join(projectDir, relDir, "package.json")
	info, err := os.Stat(pkgPath)
	if err != nil {
		return // no manifest here — nothing to reconcile
	}

	decision := decideWebRuntimeDependency(filepath.Join(projectDir, relDir))

	original, err := os.ReadFile(pkgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not read %s: %v\n", pkgPath, err)
		return
	}
	text := string(original)

	current, found := currentWebRuntimeSpec(text)
	switch {
	case found && current == decision.spec:
		return // already correct — idempotent and silent
	case found && !decision.authoritative:
		if decision.hint != "" {
			fmt.Fprintf(os.Stderr, "ℹ️  %s\n", decision.hint)
		}
		return // declared, and forge has no better answer than what is there
	case found:
		// Splice over the quoted value only; a literal splice keeps `$` in a
		// path from being read as a regexp expansion.
		at := webRuntimeDepRe.FindStringSubmatchIndex(text)
		text = text[:at[2]] + jsonString(decision.spec) + text[at[3]:]
	default:
		text, err = insertWebRuntimeDep(text, decision.spec)
		if err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not declare %s in %s: %v\n", WebRuntimePackage, pkgPath, err)
			return
		}
		if decision.hint != "" {
			fmt.Fprintf(os.Stderr, "ℹ️  %s\n", decision.hint)
		}
	}

	// Never hand back a package.json that npm would refuse to read.
	if !json.Valid([]byte(text)) {
		fmt.Fprintf(os.Stderr, "warning: declaring %s in %s would have produced invalid JSON; left unchanged\n",
			WebRuntimePackage, pkgPath)
		return
	}
	if err := os.WriteFile(pkgPath, []byte(text), info.Mode().Perm()); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not write %s: %v\n", pkgPath, err)
		return
	}
	if strings.HasPrefix(decision.spec, "file:") {
		fmt.Printf("🔗 Dev forge build: %s depends on %s at %s (npm links it; live edits, nothing published)\n",
			label, WebRuntimePackage, strings.TrimPrefix(decision.spec, "file:"))
	}
}

// currentWebRuntimeSpec returns the specifier the manifest declares today.
func currentWebRuntimeSpec(text string) (string, bool) {
	m := webRuntimeDepRe.FindStringSubmatch(text)
	if m == nil {
		return "", false
	}
	var spec string
	if err := json.Unmarshal([]byte(m[1]), &spec); err != nil {
		return "", false
	}
	return spec, true
}

// insertWebRuntimeDep adds the entry to the top-level "dependencies" object,
// matching the surrounding indentation.
func insertWebRuntimeDep(text, spec string) (string, error) {
	loc := dependenciesOpenRe.FindStringIndex(text)
	if loc == nil {
		return "", fmt.Errorf(`no "dependencies" object`)
	}
	lineStart := strings.LastIndexByte(text[:loc[0]], '\n') + 1
	keyIndent := text[lineStart:loc[0]]
	if strings.TrimSpace(keyIndent) != "" {
		keyIndent = "  " // "dependencies" did not start its own line
	}
	entryIndent := keyIndent + "  "
	entry := jsonString(WebRuntimePackage) + ": " + jsonString(spec)

	after := loc[1]
	rest := text[after:]
	trimmed := strings.TrimLeft(rest, " \t\r\n")
	if strings.HasPrefix(trimmed, "}") {
		// Empty object: no trailing comma, and re-indent the closing brace.
		skipped := len(rest) - len(trimmed)
		return text[:after] + "\n" + entryIndent + entry + "\n" + keyIndent + text[after+skipped:], nil
	}
	return text[:after] + "\n" + entryIndent + entry + "," + text[after:], nil
}

// jsonString quotes s as a JSON string literal.
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}
