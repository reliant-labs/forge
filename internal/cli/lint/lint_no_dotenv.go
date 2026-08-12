package lint

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// forge projects do not use .env files.
//
// A dotenv was handed to every host service WHOLESALE, so a value added to
// it went live in every process without ever being declared in KCL. That
// made "append a line to the untracked file" strictly cheaper than
// declaring the value, and the predictable result was non-secret config —
// service URLs, client IDs, issuer names — drifting out of version control
// into a file nobody could review or reproduce.
//
// The replacement is `forge.FileSecrets`: a single gitignored YAML file,
// populated with `forge secret set`, injected ONLY into the services that
// declare each key. This check keeps the old pattern from
// creeping back — including via a stray file a developer copies from an
// older project.
//
// Skipped directories mirror the rest of the lint sweep: vendored trees
// and build output are not the project's own source.
var dotenvSkipDirs = map[string]bool{
	"node_modules": true,
	".git":         true,
	"vendor":       true,
	"dist":         true,
	"build":        true,
	".next":        true,
	"target":       true,
	".forge":       true,
}

// dotenvFinding is one offending file, relative to the project root.
type dotenvFinding struct {
	Path string
	Why  string
}

// findDotenvFiles walks root and returns every .env* file that is not
// explicitly allowed.
//
// `.env.example` is NOT allowed either: its whole purpose was to document
// the keys of a file that should no longer exist, and `forge secret ensure`
// now reports exactly which secrets an env needs — from the KCL
// declarations, so it cannot drift.
func findDotenvFiles(root string) ([]dotenvFinding, error) {
	var found []dotenvFinding

	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// An unreadable subtree is not a lint failure; skip it.
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if dotenvSkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if name != ".env" && !strings.HasPrefix(name, ".env.") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			rel = path
		}
		found = append(found, dotenvFinding{
			Path: rel,
			Why:  classifyDotenv(name),
		})
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(found, func(i, j int) bool { return found[i].Path < found[j].Path })
	return found, nil
}

// classifyDotenv gives the file-specific reason + fix, so the message is
// actionable rather than a blanket "delete this".
func classifyDotenv(name string) string {
	switch {
	case strings.HasSuffix(name, ".example"):
		return "documents keys that should be declared in KCL; `forge secret ensure <env>` lists them from the declarations instead"
	case strings.Contains(name, "secret"):
		return "secret values belong in the secret store: `forge secret migrate <env>`"
	default:
		return "config belongs in deploy/kcl/<env>/config.k; secrets in the secret store (`forge secret migrate <env>`)"
	}
}

// runNoDotenvLint fails when any .env* file exists in the project.
func runNoDotenvLint(root string) error {
	if root == "" {
		return nil
	}
	found, err := findDotenvFiles(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("scan for .env files: %w", err)
	}
	if len(found) == 0 {
		return nil
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d .env file(s) found — forge projects do not use them:\n", len(found))
	for _, f := range found {
		fmt.Fprintf(&b, "    %-28s %s\n", f.Path, f.Why)
	}
	b.WriteString("\nA dotenv is injected into every host service wholesale, so its values\n")
	b.WriteString("never have to be declared in KCL — which is how config stops being\n")
	b.WriteString("reproducible. Declare secrets with `EnvVar.secret_ref` and store the\n")
	b.WriteString("values with `forge secret set <env> <KEY>`.")
	return errors.New(b.String())
}

// collectNoDotenvJSON is the JSON-mode view of the same check.
func collectNoDotenvJSON(root string) ([]lintJSONFinding, error) {
	if root == "" {
		return nil, nil
	}
	found, err := findDotenvFiles(root)
	if err != nil {
		return nil, err
	}
	out := make([]lintJSONFinding, 0, len(found))
	for _, f := range found {
		out = append(out, lintJSONFinding{
			Rule:     "no-dotenv",
			Severity: "error",
			File:     f.Path,
			Message:  fmt.Sprintf(".env files are not used by forge projects: %s", f.Why),
		})
	}
	return out, nil
}
