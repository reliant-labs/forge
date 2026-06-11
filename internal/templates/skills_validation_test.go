// skills_validation_test.go — fact-checks the shipped SKILL.md files
// against the forge binary they ship inside.
//
// Skills are LLM guidance; their value collapses when they reference
// commands or generated-file paths that the CURRENT forge no longer has.
// Two validators run over every embedded SKILL.md:
//
//  1. `forge <subcommand>` references (in inline code spans and fenced
//     code blocks) must resolve against the real registered cobra
//     command tree (cli.NewRootCmd()).
//  2. Path-like references with repo-shape prefixes (pkg/app/*,
//     handlers/<svc>/*_gen.go, .forge/*) must correspond to something
//     forge actually scaffolds (a real ProjectGenerator run) or a known
//     codegen output.
//
// Legitimate exceptions live in testdata/skills_validation_allowlist.txt
// (one per line: "<skill rel path>|<claim>|<justification>"). The goal is
// that NEW drift fails CI with a message naming the skill file and the
// stale claim.
package templates_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cli"
	"github.com/reliant-labs/forge/internal/generator"
	"github.com/reliant-labs/forge/internal/templates"
)

// ─── shared fixtures ────────────────────────────────────────────────────

// shippedSkills returns rel-path → content for every embedded SKILL.md.
func shippedSkills(t *testing.T) map[string]string {
	t.Helper()
	files, err := templates.ProjectTemplates().List("skills")
	if err != nil {
		t.Fatalf("list skills: %v", err)
	}
	out := map[string]string{}
	for _, rel := range files {
		if filepath.Base(rel) != "SKILL.md" {
			continue
		}
		content, err := templates.ProjectTemplates().Get("skills/" + rel)
		if err != nil {
			t.Fatalf("read skill %s: %v", rel, err)
		}
		out[rel] = string(content)
	}
	if len(out) == 0 {
		t.Fatal("no shipped SKILL.md files found")
	}
	return out
}

// allowlist returns skillRel → set of allowlisted claims.
func allowlist(t *testing.T) map[string]map[string]bool {
	t.Helper()
	out := map[string]map[string]bool{}
	data, err := os.ReadFile(filepath.Join("testdata", "skills_validation_allowlist.txt"))
	if err != nil {
		if os.IsNotExist(err) {
			return out
		}
		t.Fatalf("read allowlist: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, "|", 3)
		if len(parts) < 3 || strings.TrimSpace(parts[2]) == "" {
			t.Fatalf("allowlist line needs '<skill>|<claim>|<justification>': %q", line)
		}
		skill, claim := strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
		if out[skill] == nil {
			out[skill] = map[string]bool{}
		}
		out[skill][claim] = true
	}
	return out
}

func allowed(allow map[string]map[string]bool, skillRel, claim string) bool {
	return allow[skillRel][claim] || allow["*"][claim]
}

// scaffoldTree generates a full-featured project once and returns the set
// of slash-separated relative paths it produces (files AND directories).
var scaffoldTreeOnce struct {
	sync.Once
	paths map[string]bool
	err   error
}

func scaffoldTree(t *testing.T) map[string]bool {
	t.Helper()
	scaffoldTreeOnce.Do(func() {
		root, err := os.MkdirTemp("", "forge-skill-validate-*")
		if err != nil {
			scaffoldTreeOnce.err = err
			return
		}
		// NOTE: intentionally not removed on test exit via t.Cleanup —
		// the tree is shared across tests via sync.Once. It lives in the
		// OS temp dir and is tiny.
		gen := generator.NewProjectGenerator("demo", root, "example.com/demo")
		gen.ServiceName = "users"
		gen.FrontendName = "web"
		if err := gen.Generate(); err != nil {
			scaffoldTreeOnce.err = err
			return
		}
		paths := map[string]bool{}
		_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			rel, rerr := filepath.Rel(root, p)
			if rerr != nil || rel == "." {
				return nil
			}
			paths[filepath.ToSlash(rel)] = true
			return nil
		})
		scaffoldTreeOnce.paths = paths
	})
	if scaffoldTreeOnce.err != nil {
		t.Fatalf("scaffold demo project: %v", scaffoldTreeOnce.err)
	}
	return scaffoldTreeOnce.paths
}

// ─── markdown extraction ────────────────────────────────────────────────

var inlineCodeRE = regexp.MustCompile("`([^`\n]+)`")

// codeRegions returns the inline code spans and fenced-code-block lines
// of a markdown document — the places where `forge <cmd>` references are
// commands rather than prose.
func codeRegions(md string) []string {
	var regions []string
	inFence := false
	for _, line := range strings.Split(md, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
			continue
		}
		if inFence {
			regions = append(regions, line)
			continue
		}
		for _, m := range inlineCodeRE.FindAllStringSubmatch(line, -1) {
			regions = append(regions, m[1])
		}
	}
	return regions
}

// ─── validator 1: forge subcommand references ───────────────────────────

// forgeCmdChainRE captures the lowercase word chain following "forge ".
var forgeCmdChainRE = regexp.MustCompile(`(^|[^./\w-])forge\s+([a-z][a-z0-9_-]*(?:\s+[a-z][a-z0-9_-]*)*)`)

// versionWordRE matches version-ish tokens ("v0", "v1") that follow
// "forge" in prose like "forge v0.2" — not command references.
var versionWordRE = regexp.MustCompile(`^v\d`)

func findChild(cmd *cobra.Command, token string) *cobra.Command {
	for _, c := range cmd.Commands() {
		if c.Name() == token || c.HasAlias(token) {
			return c
		}
	}
	return nil
}

// validateCommandChain walks tokens down the cobra tree. Returns "" when
// the chain is plausible, else a human-readable reason.
func validateCommandChain(root *cobra.Command, tokens []string) string {
	cur := root
	for i, tok := range tokens {
		child := findChild(cur, tok)
		if child != nil {
			cur = child
			continue
		}
		if i == 0 {
			return "no such forge subcommand: " + tok
		}
		// Token didn't match a child. If the current command is a pure
		// group (not runnable), the token MUST have been a subcommand —
		// stale reference. If it's runnable, the token is a positional
		// arg (e.g. `forge skill load db`) — fine.
		if !cur.Runnable() && cur.HasAvailableSubCommands() {
			return tok + " is not a subcommand of 'forge " + strings.Join(tokens[:i], " ") + "'"
		}
		break
	}
	return ""
}

func TestSkillsForgeCommandReferencesExist(t *testing.T) {
	skills := shippedSkills(t)
	allow := allowlist(t)
	root := cli.NewRootCmd()

	for rel, content := range skills {
		seen := map[string]bool{}
		for _, region := range codeRegions(content) {
			for _, m := range forgeCmdChainRE.FindAllStringSubmatch(region, -1) {
				chain := m[2]
				tokens := strings.Fields(chain)
				if len(tokens) == 0 || versionWordRE.MatchString(tokens[0]) {
					continue
				}
				claim := "forge " + chain
				if seen[claim] {
					continue
				}
				seen[claim] = true
				if reason := validateCommandChain(root, tokens); reason != "" {
					if allowed(allow, rel, claim) {
						continue
					}
					t.Errorf("skills/%s: stale command reference %q — %s\n  (fix the skill, or add to internal/templates/testdata/skills_validation_allowlist.txt with a justification)",
						rel, claim, reason)
				}
			}
		}
	}
}

// ─── validator 2: repo-shape path references ────────────────────────────

var pathRefRE = regexp.MustCompile(`(?:pkg/app|handlers|\.forge)/[A-Za-z0-9_\-./<>{}*]*`)

// knownDotForgeEntries are the .forge/ children forge actually writes
// (harvested from filepath.Join(".forge", ...) call sites).
var knownDotForgeEntries = map[string]bool{
	"checksums.json":        true,
	"friction.jsonl":        true,
	"skills":                true,
	"state":                 true,
	"debug":                 true,
	"debug-session.json":    true,
	"migrations.json":       true,
	"forge.lock":            true,
	".scaffold-in-progress": true,
	".next":                 true,
}

// knownGeneratedHandlerFiles are the per-service generated files codegen
// emits into handlers/<svc>/ (beyond what the scaffold itself writes).
var knownGeneratedHandlerFiles = map[string]bool{
	"handlers_crud_ops_gen.go":  true,
	"handlers_crud_gen.go":      true, // legacy pre-split file; still named in legacy/historical context
	"handlers_crud_gen_test.go": true,
	"handlers_gen.go":           true,
	"authorizer_gen.go":         true,
	"webhook_routes_gen.go":     true,
}

// knownCodegenPkgAppFiles are pkg/app/ files written by `forge generate`
// emitters rather than the initial scaffold.
var knownCodegenPkgAppFiles = map[string]bool{
	"wire_gen.go":        true,
	"app_gen.go":         true,
	"diagnostics_gen.go": true,
	"migrate.go":         true,
	"setup.go":           true,
	"bootstrap.go":       true,
	"testing.go":         true,
	"app_extras.go":      true,
}

// trimPathRef strips trailing punctuation a markdown sentence glues onto
// a path reference.
func trimPathRef(ref string) string {
	return strings.TrimRight(ref, ".,:;)('\"")
}

// segmentsMatch reports whether ref (with <placeholder>/{placeholder}/*
// segments treated as single-segment wildcards) matches any path in tree.
func segmentsMatch(tree map[string]bool, ref string) bool {
	refSegs := strings.Split(strings.Trim(ref, "/"), "/")
	wild := func(s string) bool {
		return s == "*" || strings.HasPrefix(s, "<") || strings.HasPrefix(s, "{")
	}
outer:
	for p := range tree {
		segs := strings.Split(p, "/")
		if len(segs) != len(refSegs) {
			continue
		}
		for i := range segs {
			if wild(refSegs[i]) {
				continue
			}
			if segs[i] != refSegs[i] {
				continue outer
			}
		}
		return true
	}
	return false
}

// validatePathRef returns "" when the reference is plausible, else a
// reason. Validation is deliberately scoped to claims that are cheap and
// unambiguous to check:
//   - .forge/<entry>: entry must be something forge writes.
//   - pkg/app/<file>: forge owns pkg/app — the file must be scaffolded
//     or a known codegen output.
//   - handlers/.../<x>_gen.go: generated-file names must match a real
//     emitter (the rest of handlers/<svc>/ is mixed user territory, so
//     non-_gen references are treated as examples, not claims).
func validatePathRef(tree map[string]bool, ref string) string {
	switch {
	case strings.HasPrefix(ref, ".forge/"):
		rest := strings.TrimPrefix(ref, ".forge/")
		first := strings.SplitN(rest, "/", 2)[0]
		if first == "" || strings.HasPrefix(first, "<") || strings.HasPrefix(first, "{") || first == "*" {
			return ""
		}
		if !knownDotForgeEntries[first] {
			return ".forge/" + first + " is not something forge writes"
		}
	case strings.HasPrefix(ref, "pkg/app/"):
		rest := strings.TrimPrefix(ref, "pkg/app/")
		if rest == "" || !strings.Contains(rest, ".") {
			return "" // bare directory reference
		}
		if strings.ContainsAny(rest, "<{*") {
			return "" // placeholder file reference, can't check precisely
		}
		if knownCodegenPkgAppFiles[rest] {
			return ""
		}
		if !segmentsMatch(tree, "pkg/app/"+rest) {
			return "pkg/app/" + rest + " is not scaffolded or emitted by forge"
		}
	case strings.HasPrefix(ref, "handlers/"):
		base := filepath.Base(strings.TrimRight(ref, "/"))
		if !strings.HasSuffix(base, "_gen.go") && !strings.HasSuffix(base, "_gen_test.go") {
			return "" // user-territory example file or directory — not a codegen claim
		}
		if strings.ContainsAny(base, "<{*") {
			return ""
		}
		if !knownGeneratedHandlerFiles[base] {
			return "handlers/.../" + base + " is not a file forge generates"
		}
	}
	return ""
}

func TestSkillsPathReferencesExist(t *testing.T) {
	skills := shippedSkills(t)
	allow := allowlist(t)
	tree := scaffoldTree(t)

	for rel, content := range skills {
		seen := map[string]bool{}
		for _, raw := range pathRefRE.FindAllString(content, -1) {
			ref := trimPathRef(raw)
			if ref == "" || seen[ref] {
				continue
			}
			seen[ref] = true
			if reason := validatePathRef(tree, ref); reason != "" {
				if allowed(allow, rel, ref) {
					continue
				}
				t.Errorf("skills/%s: stale path reference %q — %s\n  (fix the skill, or add to internal/templates/testdata/skills_validation_allowlist.txt with a justification)",
					rel, ref, reason)
			}
		}
	}
}

// TestSkillsValidatorsCatchKnownBadClaims is a self-test: if the
// extraction or validation logic regresses to a no-op, this trips before
// the suite silently stops catching drift.
func TestSkillsValidatorsCatchKnownBadClaims(t *testing.T) {
	root := cli.NewRootCmd()
	tree := scaffoldTree(t)

	// Command validator.
	if reason := validateCommandChain(root, []string{"frobnicate"}); reason == "" {
		t.Error("validateCommandChain accepted a nonexistent top-level subcommand")
	}
	if reason := validateCommandChain(root, []string{"skill", "frobnicate"}); reason == "" {
		t.Error("validateCommandChain accepted a nonexistent subcommand of a pure group")
	}
	if reason := validateCommandChain(root, []string{"generate"}); reason != "" {
		t.Errorf("validateCommandChain rejected `forge generate`: %s", reason)
	}
	if reason := validateCommandChain(root, []string{"skill", "load", "db"}); reason != "" {
		t.Errorf("validateCommandChain rejected positional arg after runnable cmd: %s", reason)
	}

	// Extraction: a fenced block and an inline span must both surface.
	md := "prose\n```bash\nforge frobnicate now\n```\nand `forge bogus-cmd` inline\n"
	var found []string
	for _, region := range codeRegions(md) {
		for _, m := range forgeCmdChainRE.FindAllStringSubmatch(region, -1) {
			found = append(found, m[2])
		}
	}
	if len(found) != 2 {
		t.Errorf("codeRegions+regex extracted %d command refs from synthetic doc, want 2: %v", len(found), found)
	}

	// Path validator.
	if reason := validatePathRef(tree, "pkg/app/no_such_file_ever.go"); reason == "" {
		t.Error("validatePathRef accepted a nonexistent pkg/app file")
	}
	if reason := validatePathRef(tree, ".forge/not-a-real-thing.json"); reason == "" {
		t.Error("validatePathRef accepted a nonexistent .forge entry")
	}
	if reason := validatePathRef(tree, "handlers/users/imaginary_gen.go"); reason == "" {
		t.Error("validatePathRef accepted a nonexistent generated handler file")
	}
	if reason := validatePathRef(tree, "pkg/app/bootstrap.go"); reason != "" {
		t.Errorf("validatePathRef rejected pkg/app/bootstrap.go: %s", reason)
	}
	if reason := validatePathRef(tree, "handlers/<svc>/handlers_crud_gen.go"); reason != "" {
		t.Errorf("validatePathRef rejected placeholder crud-gen ref: %s", reason)
	}
}

// TestSkillsAllowlistEntriesStillNeeded keeps the allowlist from rotting:
// an entry whose claim no longer appears in the named skill (or whose
// skill no longer exists) must be removed.
func TestSkillsAllowlistEntriesStillNeeded(t *testing.T) {
	skills := shippedSkills(t)
	allow := allowlist(t)
	for skillRel, claims := range allow {
		if skillRel == "*" {
			continue
		}
		content, ok := skills[skillRel]
		if !ok {
			t.Errorf("allowlist names skill %q which no longer ships", skillRel)
			continue
		}
		for claim := range claims {
			// The claim text is a substring of the skill in both
			// validators' extraction paths.
			needle := strings.TrimPrefix(claim, "forge ")
			if !strings.Contains(content, needle) {
				t.Errorf("allowlist entry %q|%q no longer matches anything in the skill — remove it", skillRel, claim)
			}
		}
	}
}
