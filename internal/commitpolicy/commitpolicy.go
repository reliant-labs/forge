// Package commitpolicy checks a forge project's git state against forge's
// commit policy:
//
//   - Generated code is COMMITTED. gen/, every forge-generated file and every
//     buf-generated stub must be tracked, so a checkout builds as-cloned and
//     `forge ci verify-generated` can see drift in it.
//   - Machine-local materializations are NEVER committed: the legacy
//     .forge-kcl/ module copy, and a web frontend's dev runtime document
//     public/config.js, whose values resolve per machine.
//
// # Why this is a check and not just a template
//
// The scaffold .gitignore encodes the policy, but a project keeps the
// .gitignore it was born with forever — forge never rewrites a file the user
// owns. A project scaffolded while the template ignored gen/*, *_gen.go and
// the mocks carries those rules indefinitely, and nothing notices: its CI
// fails on `undefined:` symbols that live in ignored files, while `forge ci
// verify-generated`, which diffs with `git status --porcelain`, is blind to
// ignored paths by construction and reports green. That is the exact state a
// scaffolded project reached (its CI went red on every compiling job) before
// this check existed. It runs in `forge lint` and after verify-generated's
// regenerate, so every project that lints or runs CI is held to the policy
// whatever its .gitignore says.
package commitpolicy

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// Rule ids, stable for `forge lint --json` consumers.
const (
	// RuleGeneratedIgnored: a generated file the .gitignore excludes.
	RuleGeneratedIgnored = "generated-code-ignored"
	// RuleLocalStateTracked: a machine-local materialization in git.
	RuleLocalStateTracked = "machine-local-state-tracked"
)

// Violation is one breach of the policy.
type Violation struct {
	Rule string
	// Path is project-relative, slash-separated. For an ignored generated
	// file it is the file; for tracked local state it is the tracked path
	// (a directory for .forge-kcl/).
	Path string
	// Why says what the path is and why the policy applies to it.
	Why string
	// Fix is the literal command (or edit) that resolves it.
	Fix string
}

// legacyKCLDir is the project-local KCL module copy older forge versions
// materialized (and committed).
const legacyKCLDir = ".forge-kcl"

// devRuntimeConfig is a web frontend's dev runtime document, relative to the
// frontend directory.
const devRuntimeConfig = "public/config.js"

// skipDirs are never walked: dependencies, build output, VCS and forge state.
// None of them hold project source, and several are huge.
var skipDirs = map[string]bool{
	".git":           true,
	".forge":         true,
	legacyKCLDir:     true,
	"node_modules":   true,
	"vendor":         true,
	".next":          true,
	".next-prod":     true,
	"dist":           true,
	"build":          true,
	"out":            true,
	".turbo":         true,
	".expo":          true,
	"testdata":       true,
	".scratch":       true,
	".forge-link":    true,
	".forge-pkg":     true,
	".cloud-dev":     true,
	".firebase":      true,
	".firebase-dist": true,
}

// Check inspects the git work tree rooted at projectDir. It returns nil,
// nil outside a git work tree (or without git): there is no commit state to
// judge, and guessing would be worse than silence.
func Check(projectDir string) ([]Violation, error) {
	if !inWorkTree(projectDir) {
		return nil, nil
	}
	var out []Violation

	ignored, err := ignoredGeneratedFiles(projectDir)
	if err != nil {
		return nil, err
	}
	out = append(out, ignored...)

	tracked, err := trackedLocalState(projectDir)
	if err != nil {
		return nil, err
	}
	out = append(out, tracked...)

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Rule != out[j].Rule {
			return out[i].Rule < out[j].Rule
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}

func inWorkTree(dir string) bool {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--is-inside-work-tree").Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// ignoredGeneratedFiles walks the project for generated files and reports the
// ones git ignores.
func ignoredGeneratedFiles(projectDir string) ([]Violation, error) {
	var candidates []string
	err := filepath.WalkDir(projectDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p != projectDir && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil // symlinks (the dev web-runtime bridge) are not source
		}
		rel, rerr := filepath.Rel(projectDir, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		if isDevRuntimeConfig(rel) {
			return nil // machine-local by policy; ignoring it is correct
		}
		if isGenerated(p, rel) {
			candidates = append(candidates, rel)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan for generated files: %w", err)
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	ignored, err := checkIgnore(projectDir, candidates)
	if err != nil {
		return nil, err
	}
	var out []Violation
	for _, rel := range candidates {
		rule, ok := ignored[rel]
		if !ok {
			continue
		}
		out = append(out, Violation{
			Rule: RuleGeneratedIgnored,
			Path: rel,
			Why: fmt.Sprintf("generated code, but excluded from git by %s — a fresh clone or CI checkout will not have it, "+
				"and `forge ci verify-generated` cannot see drift in an ignored file", rule),
			Fix: fmt.Sprintf("remove the ignore rule (%s), then `git add %s` — forge's policy is that generated code is committed and a checkout builds as-cloned", rule, rel),
		})
	}
	return out, nil
}

// isGenerated reports whether the file is generated code this policy
// requires to be committed: forge's own output (the DO-NOT-EDIT banner or an
// embedded forge:hash marker), buf's stubs (Go and TypeScript), and the
// descriptor forge extracts into gen/.
func isGenerated(abs, rel string) bool {
	switch {
	case rel == "gen/forge_descriptor.json":
		return true
	case strings.HasSuffix(rel, ".pb.go"), strings.HasSuffix(rel, ".connect.go"), strings.HasSuffix(rel, "_pb.ts"), strings.HasSuffix(rel, "_connect.ts"):
		return true
	}
	return hasGeneratedHeader(abs)
}

// headerProbeBytes bounds the header read: banners and markers sit in the
// first few lines.
const headerProbeBytes = 2048

// hasGeneratedHeader reports whether the file's head carries forge's banner
// or certification marker, or the Go-convention generated banner.
func hasGeneratedHeader(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, headerProbeBytes)
	n, _ := io.ReadFull(f, buf)
	head := buf[:n]
	if bytes.Contains(head, []byte("Code generated by forge. DO NOT EDIT.")) {
		return true
	}
	if i := bytes.Index(head, []byte("forge:hash=")); i >= 0 {
		// Require a real hash value, so prose that merely mentions the
		// marker key is not mistaken for a stamped file.
		v := head[i+len("forge:hash="):]
		return len(v) >= 16 && isHex(v[:16])
	}
	// Go's convention: `^// Code generated .* DO NOT EDIT\.$` on its own line.
	sc := bufio.NewScanner(bytes.NewReader(head))
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "// Code generated ") && strings.HasSuffix(line, " DO NOT EDIT.") {
			return true
		}
	}
	return false
}

func isHex(b []byte) bool {
	for _, c := range b {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// isDevRuntimeConfig reports whether rel is a frontend's dev runtime document
// (frontends/<name>/public/config.js).
func isDevRuntimeConfig(rel string) bool {
	return strings.HasPrefix(rel, "frontends/") && strings.HasSuffix(rel, "/"+devRuntimeConfig) &&
		strings.Count(rel, "/") == 3
}

// checkIgnore returns, for each of paths git ignores, the matching rule as
// "<source>:<line>:<pattern>".
func checkIgnore(dir string, paths []string) (map[string]string, error) {
	cmd := exec.Command("git", "-C", dir, "check-ignore", "-v", "--stdin", "-z")
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\x00") + "\x00")
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if !errors.As(err, &ee) || ee.ExitCode() != 1 { // 1 = nothing ignored
			return nil, fmt.Errorf("git check-ignore: %w", err)
		}
	}
	// -z -v output: <source> NUL <linenum> NUL <pattern> NUL <pathname> NUL
	fields := strings.Split(string(out), "\x00")
	got := map[string]string{}
	for i := 0; i+3 < len(fields); i += 4 {
		source, line, pattern, path := fields[i], fields[i+1], fields[i+2], fields[i+3]
		if strings.HasPrefix(pattern, "!") {
			continue // matched a negation: NOT ignored
		}
		got[path] = fmt.Sprintf("%s:%s:%s", source, line, pattern)
	}
	return got, nil
}

// trackedLocalState reports machine-local materializations that are in git.
func trackedLocalState(projectDir string) ([]Violation, error) {
	out, err := exec.Command("git", "-C", projectDir, "ls-files", "-z", "--",
		legacyKCLDir, "frontends/*/"+devRuntimeConfig).Output()
	if err != nil {
		return nil, fmt.Errorf("git ls-files: %w", err)
	}
	var vs []Violation
	kclTracked := false
	for _, p := range strings.Split(string(out), "\x00") {
		switch {
		case p == "":
		case strings.HasPrefix(p, legacyKCLDir+"/"):
			kclTracked = true
		case isDevRuntimeConfig(p):
			vs = append(vs, Violation{
				Rule: RuleLocalStateTracked,
				Path: p,
				Why: "the DEV runtime config document — rendered from deploy/kcl/dev/config.k, whose ports resolve per machine, " +
					"so a committed copy drifts on every machine that regenerates. `forge generate`, `forge build` and `forge env up` " +
					"materialize it; a deploy writes each environment's own copy beside the bundle",
				Fix: fmt.Sprintf("git rm --cached %s && echo %s >> %s/.gitignore", p, devRuntimeConfig, strings.TrimSuffix(p, "/"+devRuntimeConfig)),
			})
		}
	}
	if kclTracked {
		vs = append(vs, Violation{
			Rule: RuleLocalStateTracked,
			Path: legacyKCLDir + "/",
			Why: "a copy of the forge KCL module, which forge now supplies from the binary that renders — a committed copy pins one " +
				"forge build's schemas into every clone and CI checkout, and fights every other build",
			Fix: fmt.Sprintf("forge generate && git rm -r --cached %s && echo %s/ >> .gitignore", legacyKCLDir, legacyKCLDir),
		})
	}
	return vs, nil
}

// Format renders violations as the multi-line block `forge lint` and `forge
// ci verify-generated` print.
func Format(vs []Violation) string {
	var b strings.Builder
	for _, v := range vs {
		fmt.Fprintf(&b, "  ❌ %s — %s\n", v.Path, v.Why)
		fmt.Fprintf(&b, "      ↪ %s\n", v.Fix)
	}
	return b.String()
}
