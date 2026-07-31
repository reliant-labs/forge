// Package pkgguard holds the repository-wide proof that forge/pkg — the
// library module that compiles INTO every generated binary — obeys the rules
// forge's own generated projects are held to.
//
// It exists because forge/pkg is a SEPARATE Go module. `go test ./...` and
// `golangci-lint run` at the repo root both operate on the module they are
// invoked in, so for as long as CI only ran them from the root, nothing in
// pkg/ was ever tested or linted. Twelve direct environment reads accumulated
// outside the sanctioned readers — in a library whose shipped lint config
// fails a user's build for the same call.
//
// The scanner lives here (rather than in the test) so its inputs are
// explicit: it derives BOTH the forbidden calls and the exempt packages from
// pkg/.golangci.yml, the file that actually gates CI. Nothing here hardcodes
// a function name or a file list — a rule added to that config is enforced by
// this guard on the next run, and a rule deleted from it stops being
// enforced loudly rather than silently.
package pkgguard

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	yaml "gopkg.in/yaml.v3"
)

// ForbidigoPolicy is the forbidigo half of a golangci-lint config: whether
// the linter GATES (membership in linters.enable, not merely a settings
// block), what it forbids, and which paths are exempted.
type ForbidigoPolicy struct {
	// Enabled reports whether forbidigo is in linters.enable. A settings
	// block alone configures a linter that never runs.
	Enabled bool
	// Forbid are the compiled `forbid[].pattern` regexes, matched against a
	// call expression's rendered form ("os.Getenv") exactly as forbidigo
	// matches them.
	Forbid []*regexp.Regexp
	// Exempt are the compiled `exclusions.rules[].path` regexes of every
	// rule that names forbidigo, matched against the module-relative path.
	Exempt []*regexp.Regexp
}

// Finding is one forbidden call: where it is and what it called.
type Finding struct {
	// Path is module-relative and slash-separated, matching what
	// golangci-lint prints when run inside the module.
	Path string
	Line int
	Call string
}

func (f Finding) String() string { return fmt.Sprintf("%s:%d: %s", f.Path, f.Line, f.Call) }

// yamlConfig is the subset of a golangci-lint v2 config this guard reads.
type yamlConfig struct {
	Linters struct {
		Enable   []string `yaml:"enable"`
		Settings struct {
			Forbidigo struct {
				Forbid []struct {
					Pattern string `yaml:"pattern"`
				} `yaml:"forbid"`
			} `yaml:"forbidigo"`
		} `yaml:"settings"`
		Exclusions struct {
			Rules []struct {
				Path    string   `yaml:"path"`
				Linters []string `yaml:"linters"`
			} `yaml:"rules"`
		} `yaml:"exclusions"`
	} `yaml:"linters"`
}

// LoadForbidigoPolicy parses the golangci-lint config at path.
func LoadForbidigoPolicy(path string) (ForbidigoPolicy, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // a repo-relative config path
	if err != nil {
		return ForbidigoPolicy{}, fmt.Errorf("read %s: %w", path, err)
	}
	var cfg yamlConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return ForbidigoPolicy{}, fmt.Errorf("parse %s: %w", path, err)
	}
	var p ForbidigoPolicy
	for _, l := range cfg.Linters.Enable {
		if l == "forbidigo" {
			p.Enabled = true
		}
	}
	for _, f := range cfg.Linters.Settings.Forbidigo.Forbid {
		re, cerr := regexp.Compile(f.Pattern)
		if cerr != nil {
			return ForbidigoPolicy{}, fmt.Errorf("%s: forbid pattern %q: %w", path, f.Pattern, cerr)
		}
		p.Forbid = append(p.Forbid, re)
	}
	for _, r := range cfg.Linters.Exclusions.Rules {
		named := false
		for _, l := range r.Linters {
			if l == "forbidigo" {
				named = true
			}
		}
		if !named || r.Path == "" {
			continue
		}
		re, cerr := regexp.Compile(r.Path)
		if cerr != nil {
			return ForbidigoPolicy{}, fmt.Errorf("%s: exclusion path %q: %w", path, r.Path, cerr)
		}
		p.Exempt = append(p.Exempt, re)
	}
	return p, nil
}

// Exempted reports whether a module-relative path is allowlisted.
func (p ForbidigoPolicy) Exempted(rel string) bool {
	for _, re := range p.Exempt {
		if re.MatchString(rel) {
			return true
		}
	}
	return false
}

// skipDirs are never scanned: not source, or not this module's code.
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "testdata": true,
	"bin": true, "tmp": true, "dist": true,
}

// Scan walks the module rooted at root and returns every call matching
// policy.Forbid outside an exempt path, plus the number of Go files it
// actually parsed. A caller that gets files == 0 has a broken walk, not a
// clean module — the assertion is only as real as the set it inspected.
func Scan(root string, policy ForbidigoPolicy) (findings []Finding, files int, err error) {
	fset := token.NewFileSet()
	walkErr := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			if rel != "." && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(rel, ".go") {
			return nil
		}
		files++
		if policy.Exempted(rel) {
			return nil
		}
		file, perr := parser.ParseFile(fset, p, nil, 0)
		if perr != nil {
			return fmt.Errorf("parse %s: %w", rel, perr)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := renderCallee(call.Fun)
			if name == "" {
				return true
			}
			for _, re := range policy.Forbid {
				if re.MatchString(name) {
					findings = append(findings, Finding{
						Path: rel,
						Line: fset.Position(call.Lparen).Line,
						Call: name,
					})
					break
				}
			}
			return true
		})
		return nil
	})
	if walkErr != nil {
		return nil, files, walkErr
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Path != findings[j].Path {
			return findings[i].Path < findings[j].Path
		}
		return findings[i].Line < findings[j].Line
	})
	return findings, files, nil
}

// renderCallee renders `pkg.Func` for a qualified call and "" for anything
// else — the same shape forbidigo matches its patterns against. Only
// package-qualified calls can be a direct environment read; a method call on
// a value (x.Getenv()) is not what the patterns describe.
func renderCallee(fn ast.Expr) string {
	sel, ok := fn.(*ast.SelectorExpr)
	if !ok {
		return ""
	}
	ident, ok := sel.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return ident.Name + "." + sel.Sel.Name
}
