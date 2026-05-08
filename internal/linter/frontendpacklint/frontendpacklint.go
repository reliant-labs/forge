// Package frontendpacklint provides a soft-rule analyzer that flags
// frontend pack templates importing third-party UI libraries directly
// instead of wrapping the forge base component library.
//
// The convention enforced here:
//
//   - Layer 1 (base library): forge/components/components/ui/ — primitives
//     installed by every frontend at scaffold time.
//   - Layer 2 (forge-aware primitives): hook-aware components that ship
//     unconditionally with the scaffold.
//   - Layer 3 (domain packs): opt-in installs under
//     frontends/<name>/src/components/<pack>/.
//
// Frontend packs MUST import from layers 1-2; pulling a third-party UI
// component in directly bypasses the layered design. Some packs
// legitimately need third-party deps (a chart pack wrapping `recharts`,
// a maps pack wrapping `@react-google-maps/api`) — those packs declare
// their exceptions in `pack.yaml` under `allowed_third_party:`.
//
// Findings are warnings (severity warn). The analyzer never gates the
// build; it surfaces convention drift so authors can either add the
// import to `allowed_third_party:` or wrap the third-party API behind a
// base library primitive.
package frontendpacklint

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Severity indicates how important a finding is. The frontendpacklint
// analyzer only emits warnings — it is intentionally non-blocking.
type Severity string

const (
	SeverityWarning Severity = "warn"
)

// Finding describes a single import that violates the soft rule.
type Finding struct {
	Pack     string   `json:"pack"`
	File     string   `json:"file"`
	Line     int      `json:"line"`
	Import   string   `json:"import"`
	Severity Severity `json:"severity"`
	Rule     string   `json:"rule"`
	Message  string   `json:"message"`
}

// Result aggregates findings.
type Result struct {
	Findings []Finding `json:"findings"`
}

// HasWarnings reports whether any finding was emitted.
func (r Result) HasWarnings() bool { return len(r.Findings) > 0 }

// FormatText renders the findings as human-readable output.
func (r Result) FormatText() string {
	if len(r.Findings) == 0 {
		return "✓ No frontend pack convention warnings.\n"
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "Found %d frontend pack convention warning(s):\n\n", len(r.Findings))
	for _, f := range r.Findings {
		fmt.Fprintf(&sb, "  ⚠️  %s:%d [%s] %s: %s\n", f.File, f.Line, f.Rule, f.Pack, f.Message)
	}
	sb.WriteString("\nFix: import the equivalent primitive from @/components/ui/* (the forge base library).\n")
	sb.WriteString("Or, if the third-party lib is genuinely required (e.g. a charts pack), add it to\n")
	sb.WriteString("`allowed_third_party:` in the pack's pack.yaml manifest.\n")
	return sb.String()
}

// suspiciousUIPrefixes matches package specifiers that the convention
// considers third-party UI/component libraries — anything in this set
// either has a base library equivalent forge already ships, or should
// be wrapped behind one before a pack pulls it in. Order doesn't
// matter; matching is by HasPrefix.
//
// `react`, `next/*`, `@bufbuild/*`, `@connectrpc/*`, etc. are NOT in
// this list — they are framework deps every forge frontend uses.
var suspiciousUIPrefixes = []string{
	"@radix-ui/",
	"@headlessui/",
	"@chakra-ui/",
	"@mui/",
	"@material-ui/",
	"@mantine/",
	"@nextui-org/",
	"antd",
	"@ant-design/",
	"react-bootstrap",
	"reactstrap",
	"@tremor/",
	"@tanstack/react-table", // headless, but consumers should wrap not duplicate
	"@tanstack/table-core",
	"recharts",
	"victory",
	"chart.js",
	"react-chartjs-2",
}

// importLineRE captures the package specifier in `import ... from "<spec>"`.
// It deliberately ignores type-only imports (`import type X from ...`) and
// dynamic imports (`import("...")`). Multi-line imports are matched per-line.
var importLineRE = regexp.MustCompile(`(?m)^\s*import\s+(?:type\s+)?[^"']*?\s*from\s*['"]([^'"]+)['"]`)

// LintPackDir runs the analyzer on a single pack directory. Returns no
// findings (and no error) when the pack is not a frontend pack or has no
// templates.
func LintPackDir(packDir string) (Result, error) {
	manifestPath := filepath.Join(packDir, "pack.yaml")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		if os.IsNotExist(err) {
			return Result{}, nil
		}
		return Result{}, fmt.Errorf("read pack.yaml: %w", err)
	}

	var manifest struct {
		Name              string   `yaml:"name"`
		Kind              string   `yaml:"kind"`
		AllowedThirdParty []string `yaml:"allowed_third_party"`
	}
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		return Result{}, fmt.Errorf("parse pack.yaml: %w", err)
	}

	// Only frontend packs are subject to this rule.
	if !strings.EqualFold(strings.TrimSpace(manifest.Kind), "frontend") {
		return Result{}, nil
	}

	templatesDir := filepath.Join(packDir, "templates")
	if _, err := os.Stat(templatesDir); os.IsNotExist(err) {
		return Result{}, nil
	}

	var findings []Finding
	walkErr := filepath.Walk(templatesDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		// Only scan TypeScript/TSX templates. .ts.tmpl and .tsx.tmpl, plus
		// raw .ts/.tsx that are sometimes shipped untemplated.
		base := info.Name()
		if !(strings.HasSuffix(base, ".ts.tmpl") ||
			strings.HasSuffix(base, ".tsx.tmpl") ||
			strings.HasSuffix(base, ".ts") ||
			strings.HasSuffix(base, ".tsx")) {
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		fileFindings := scanFile(string(body), path, manifest.Name, manifest.AllowedThirdParty)
		findings = append(findings, fileFindings...)
		return nil
	})
	if walkErr != nil {
		return Result{}, walkErr
	}

	sort.SliceStable(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		return findings[i].Line < findings[j].Line
	})

	return Result{Findings: findings}, nil
}

// LintPacksRoot scans every pack under root (e.g. internal/packs/) and
// returns the union of findings.
func LintPacksRoot(root string) (Result, error) {
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return Result{}, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return Result{}, fmt.Errorf("read packs root: %w", err)
	}
	var all []Finding
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		res, err := LintPackDir(filepath.Join(root, e.Name()))
		if err != nil {
			return Result{}, fmt.Errorf("lint pack %s: %w", e.Name(), err)
		}
		all = append(all, res.Findings...)
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].File != all[j].File {
			return all[i].File < all[j].File
		}
		return all[i].Line < all[j].Line
	})
	return Result{Findings: all}, nil
}

// scanFile returns one finding per third-party UI import that doesn't
// match the pack's allowlist. The pack name + relative file path are
// embedded for actionable output.
func scanFile(src, path, packName string, allowed []string) []Finding {
	var findings []Finding
	matches := importLineRE.FindAllStringSubmatchIndex(src, -1)
	for _, m := range matches {
		spec := src[m[2]:m[3]]
		prefix, hit := matchSuspicious(spec)
		if !hit {
			continue
		}
		if isAllowed(spec, allowed) {
			continue
		}
		line := lineNumber(src, m[2])
		findings = append(findings, Finding{
			Pack:     packName,
			File:     path,
			Line:     line,
			Import:   spec,
			Severity: SeverityWarning,
			Rule:     "frontendpack-third-party-ui",
			Message: fmt.Sprintf(
				"imports %q (matches %q) — frontend packs should wrap base library primitives instead of importing third-party UI directly. "+
					"If this dependency is genuinely required, add %q to `allowed_third_party:` in pack.yaml.",
				spec, prefix, spec,
			),
		})
	}
	return findings
}

// matchSuspicious returns the prefix that matched and whether a match
// was found. Specifiers like `react`, `@/components/ui/button`, `next/link`
// do not match.
func matchSuspicious(spec string) (string, bool) {
	for _, p := range suspiciousUIPrefixes {
		// Exact match for non-wildcard prefixes ("antd"), or HasPrefix for
		// scoped prefixes ending in "/".
		if strings.HasSuffix(p, "/") {
			if strings.HasPrefix(spec, p) {
				return p, true
			}
		} else {
			if spec == p || strings.HasPrefix(spec, p+"/") {
				return p, true
			}
		}
	}
	return "", false
}

// isAllowed checks the pack's allowlist. An entry of "@tanstack/react-table"
// permits any subpath under it (`@tanstack/react-table`, `@tanstack/react-table/foo`).
// An entry of "@radix-ui/" permits the whole scope.
func isAllowed(spec string, allowed []string) bool {
	for _, a := range allowed {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if strings.HasSuffix(a, "/") {
			if strings.HasPrefix(spec, a) {
				return true
			}
		} else {
			if spec == a || strings.HasPrefix(spec, a+"/") {
				return true
			}
		}
	}
	return false
}

// lineNumber returns the 1-based line number for the given byte offset.
func lineNumber(src string, off int) int {
	if off < 0 {
		off = 0
	}
	if off > len(src) {
		off = len(src)
	}
	return strings.Count(src[:off], "\n") + 1
}
