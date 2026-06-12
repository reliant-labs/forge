// skills_validation_test.go — fact-checks the shipped SKILL.md files
// against the forge binary they ship inside.
//
// Skills are LLM guidance; their value collapses when they reference
// commands or generated-file paths that the CURRENT forge no longer has.
// Three validators run over every embedded SKILL.md:
//
//  1. `forge <subcommand>` references (in inline code spans and fenced
//     code blocks) must resolve against the real registered cobra
//     command tree (cli.NewRootCmd()).
//  2. Path-like references with repo-shape prefixes (pkg/app/*,
//     handlers/<svc>/*_gen.go, .forge/*) must correspond to something
//     forge actually scaffolds (a real ProjectGenerator run) or a known
//     codegen output.
//  3. `(forge.v1.<ext>) = { ... }` annotation literals (and dotted
//     `(forge.v1.<ext>).<field>` references) must use field names that
//     exist on the real annotation payload messages. Truth source: the
//     protoreflect descriptors of internal/gen/forge/v1 (generated from
//     internal/assets/proto/forge/v1/forge.proto), so a schema change
//     that strands the skills fails here. Enum-valued fields (e.g.
//     `store:`) must use enum value names, bool fields must get
//     true/false, and message-valued fields (e.g. `validate:`) must get
//     `{ ... }` literals rather than scalars. This validator also covers
//     reliant.md.tmpl, which ships the same annotation guidance.
//
// Legitimate exceptions live in testdata/skills_validation_allowlist.txt
// (one per line: "<skill rel path>|<claim>|<justification>"). The goal is
// that NEW drift fails CI with a message naming the skill file and the
// stale claim.
package templates_test

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/spf13/cobra"
	"google.golang.org/protobuf/reflect/protoreflect"

	"github.com/reliant-labs/forge/internal/cli"
	forgev1 "github.com/reliant-labs/forge/internal/gen/forge/v1"
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
	// checksums.json is the DEAD legacy manifest — still referenced by
	// migration docs (forge reads + deletes it during the one-time
	// migration), so it stays a known entry.
	"checksums.json": true,
	"disowned.json":  true,
	"hashes.json":    true,
	"render":         true,
	"friction.jsonl": true,
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

// ─── validator 3: forge.v1 annotation field references ──────────────────

// annotationDescriptors maps the extension names skills use —
// (forge.v1.entity) etc. — to the protoreflect descriptors of their real
// payload messages. These descriptors come from the generated package
// internal/gen/forge/v1, itself generated from
// internal/assets/proto/forge/v1/forge.proto, so the schema is the truth:
// renaming/removing a proto field makes any skill that still documents
// the old name fail here.
func annotationDescriptors() map[string]protoreflect.MessageDescriptor {
	return map[string]protoreflect.MessageDescriptor{
		"entity":  (&forgev1.EntityOptions{}).ProtoReflect().Descriptor(),
		"field":   (&forgev1.FieldOptions{}).ProtoReflect().Descriptor(),
		"service": (&forgev1.ServiceOptions{}).ProtoReflect().Descriptor(),
		"method":  (&forgev1.MethodOptions{}).ProtoReflect().Descriptor(),
		"config":  (&forgev1.ConfigFieldOptions{}).ProtoReflect().Descriptor(),
	}
}

// annotationRegions returns each fenced code block as ONE string (so
// multi-line annotation literals stay intact) plus every inline code
// span. Compare codeRegions, which is line-oriented.
func annotationRegions(md string) []string {
	var regions []string
	var fence []string
	inFence := false
	for _, line := range strings.Split(md, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "```") {
			if inFence && len(fence) > 0 {
				regions = append(regions, strings.Join(fence, "\n"))
				fence = nil
			}
			inFence = !inFence
			continue
		}
		if inFence {
			fence = append(fence, line)
			continue
		}
		for _, m := range inlineCodeRE.FindAllStringSubmatch(line, -1) {
			regions = append(regions, m[1])
		}
	}
	if inFence && len(fence) > 0 {
		regions = append(regions, strings.Join(fence, "\n"))
	}
	return regions
}

// annotationLiteralRE matches the head of an annotation literal:
// "(forge.v1.entity) = {". The trailing brace index anchors the parser.
var annotationLiteralRE = regexp.MustCompile(`\(forge\.v1\.([a-z_]+)\)\s*=\s*\{`)

// annotationDottedRE matches dotted field access like
// "(forge.v1.method).auth_required".
var annotationDottedRE = regexp.MustCompile(`\(forge\.v1\.([a-z_]+)\)((?:\.[A-Za-z_][A-Za-z0-9_]*)+)`)

type annViolation struct {
	claim  string // e.g. "(forge.v1.entity).table_name" — the allowlist key
	reason string
}

func joinFieldPath(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

func fieldNames(desc protoreflect.MessageDescriptor) string {
	fields := desc.Fields()
	names := make([]string, 0, fields.Len())
	for i := 0; i < fields.Len(); i++ {
		names = append(names, string(fields.Get(i).Name()))
	}
	return strings.Join(names, ", ")
}

func enumValueNames(ed protoreflect.EnumDescriptor) string {
	vals := ed.Values()
	names := make([]string, 0, vals.Len())
	for i := 0; i < vals.Len(); i++ {
		names = append(names, string(vals.Get(i).Name()))
	}
	return strings.Join(names, ", ")
}

// annLitParser walks a proto-text-format-ish `{ key: value ... }` literal
// (nesting via {} and [], `//` and `#` comments, optional commas, colon
// optional before message values) and validates every field key against
// the descriptor of its enclosing message. It is deliberately small and
// lenient: anything it can't tokenize (e.g. a `...` ellipsis in a doc)
// aborts validation of that literal rather than guessing.
type annLitParser struct {
	ext string
	src string
	pos int
	out []annViolation
}

func (p *annLitParser) violation(path, reason string) {
	p.out = append(p.out, annViolation{claim: "(forge.v1." + p.ext + ")." + path, reason: reason})
}

// skipSep skips whitespace, pair separators (commas/semicolons), and
// comments — the stuff allowed BETWEEN key/value pairs.
func (p *annLitParser) skipSep() {
	for p.pos < len(p.src) {
		switch c := p.src[p.pos]; {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == ',' || c == ';':
			p.pos++
		case c == '#' || (c == '/' && p.pos+1 < len(p.src) && p.src[p.pos+1] == '/'):
			for p.pos < len(p.src) && p.src[p.pos] != '\n' {
				p.pos++
			}
		default:
			return
		}
	}
}

// skipSpace skips only horizontal whitespace — used between a field name
// and its (potential) value, where a comma/newline instead means the doc
// mentioned the field name without a value (shorthand like
// `{ sensitive, category }`), which is fine.
func (p *annLitParser) skipSpace() {
	for p.pos < len(p.src) && (p.src[p.pos] == ' ' || p.src[p.pos] == '\t') {
		p.pos++
	}
}

func (p *annLitParser) ident() string {
	start := p.pos
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			p.pos++
			continue
		}
		break
	}
	return p.src[start:p.pos]
}

// scalarToken consumes one scalar value: a quoted string or a bare
// identifier/number token. Returns the token and whether it was quoted.
// Consumes one character on unparseable input so callers always progress.
func (p *annLitParser) scalarToken() (tok string, quoted bool) {
	if p.pos >= len(p.src) {
		return "", false
	}
	if q := p.src[p.pos]; q == '"' || q == '\'' {
		p.pos++
		start := p.pos
		for p.pos < len(p.src) && p.src[p.pos] != q {
			if p.src[p.pos] == '\\' {
				p.pos++
			}
			p.pos++
		}
		tok = p.src[start:min(p.pos, len(p.src))]
		if p.pos < len(p.src) {
			p.pos++ // closing quote
		}
		return tok, true
	}
	start := p.pos
	for p.pos < len(p.src) {
		c := p.src[p.pos]
		if c == '_' || c == '.' || c == '+' || c == '-' ||
			(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			p.pos++
			continue
		}
		break
	}
	if p.pos == start {
		p.pos++ // unparseable char — consume so the caller progresses
		return "", false
	}
	return p.src[start:p.pos], false
}

// parseMessage validates the `{ ... }` literal at p.pos against desc.
// A nil desc means "unknown message" (the field key was already flagged);
// structure is consumed but keys aren't checked.
func (p *annLitParser) parseMessage(desc protoreflect.MessageDescriptor, prefix string) {
	if p.pos >= len(p.src) || p.src[p.pos] != '{' {
		return
	}
	p.pos++
	for {
		p.skipSep()
		if p.pos >= len(p.src) {
			return // unterminated literal (doc elided the rest) — lenient
		}
		if p.src[p.pos] == '}' {
			p.pos++
			return
		}
		name := p.ident()
		if name == "" {
			// Unparseable content (e.g. a "..." ellipsis). Stop validating
			// this literal rather than guessing.
			p.pos = len(p.src)
			return
		}
		path := joinFieldPath(prefix, name)
		var fd protoreflect.FieldDescriptor
		if desc != nil {
			fd = desc.Fields().ByName(protoreflect.Name(name))
			if fd == nil {
				p.violation(path, fmt.Sprintf("forge.v1.%s has no field %q (fields: %s)", desc.Name(), name, fieldNames(desc)))
			}
		}
		p.skipSpace()
		switch {
		case p.pos < len(p.src) && p.src[p.pos] == ':':
			p.pos++
			p.skipSep()
			p.parseValue(fd, path)
		case p.pos < len(p.src) && (p.src[p.pos] == '{' || p.src[p.pos] == '['):
			// text format allows `auth { ... }` with no colon
			p.parseValue(fd, path)
		default:
			// Name-only mention (doc shorthand) — no value to validate.
		}
	}
}

func (p *annLitParser) parseValue(fd protoreflect.FieldDescriptor, path string) {
	if p.pos >= len(p.src) {
		return
	}
	switch p.src[p.pos] {
	case '{':
		var sub protoreflect.MessageDescriptor
		if fd != nil {
			if fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind {
				sub = fd.Message()
			} else {
				p.violation(path, fmt.Sprintf("%s is a %s field — it does not take a { ... } message literal", path, fd.Kind()))
			}
		}
		p.parseMessage(sub, path)
	case '[':
		p.pos++
		for {
			p.skipSep()
			if p.pos >= len(p.src) {
				return
			}
			if p.src[p.pos] == ']' {
				p.pos++
				return
			}
			p.parseValue(fd, path)
		}
	default:
		tok, quoted := p.scalarToken()
		p.checkScalar(fd, path, tok, quoted)
	}
}

func (p *annLitParser) checkScalar(fd protoreflect.FieldDescriptor, path, tok string, quoted bool) {
	if fd == nil || (tok == "" && !quoted) {
		return
	}
	switch fd.Kind() {
	case protoreflect.MessageKind, protoreflect.GroupKind:
		p.violation(path, fmt.Sprintf("%s is a message (%s) — use %s: { ... } with fields %s, not a scalar value",
			path, fd.Message().FullName(), path, fieldNames(fd.Message())))
	case protoreflect.EnumKind:
		if quoted || fd.Enum().Values().ByName(protoreflect.Name(tok)) == nil {
			p.violation(path, fmt.Sprintf("%s is enum %s — value must be one of: %s (got %q)",
				path, fd.Enum().Name(), enumValueNames(fd.Enum()), tok))
		}
	case protoreflect.BoolKind:
		if tok != "true" && tok != "false" {
			p.violation(path, fmt.Sprintf("%s is a bool field — value must be true or false (got %q)", path, tok))
		}
	}
}

// scanAnnotationRegion finds every (forge.v1.<ext>) reference in one code
// region and returns the violations.
func scanAnnotationRegion(region string, descs map[string]protoreflect.MessageDescriptor) []annViolation {
	var out []annViolation
	for _, m := range annotationLiteralRE.FindAllStringSubmatchIndex(region, -1) {
		ext := region[m[2]:m[3]]
		desc, ok := descs[ext]
		if !ok {
			out = append(out, annViolation{
				claim:  "(forge.v1." + ext + ")",
				reason: "no such forge.v1 extension (valid: entity, field, service, method, config)",
			})
			continue
		}
		p := &annLitParser{ext: ext, src: region, pos: m[1] - 1} // m[1]-1 = the '{'
		p.parseMessage(desc, "")
		out = append(out, p.out...)
	}
	for _, m := range annotationDottedRE.FindAllStringSubmatch(region, -1) {
		ext, chain := m[1], m[2]
		desc, ok := descs[ext]
		if !ok {
			out = append(out, annViolation{
				claim:  "(forge.v1." + ext + ")",
				reason: "no such forge.v1 extension (valid: entity, field, service, method, config)",
			})
			continue
		}
		cur, path := desc, ""
		for _, seg := range strings.Split(strings.TrimPrefix(chain, "."), ".") {
			if cur == nil {
				break
			}
			path = joinFieldPath(path, seg)
			fd := cur.Fields().ByName(protoreflect.Name(seg))
			if fd == nil {
				out = append(out, annViolation{
					claim:  "(forge.v1." + ext + ")." + path,
					reason: fmt.Sprintf("forge.v1.%s has no field %q (fields: %s)", cur.Name(), seg, fieldNames(cur)),
				})
				break
			}
			if fd.Kind() == protoreflect.MessageKind {
				cur = fd.Message()
			} else {
				cur = nil
			}
		}
	}
	return out
}

// annotationDocs returns the docs this validator covers: every shipped
// SKILL.md (keyed as in the allowlist) plus reliant.md.tmpl, which ships
// the same annotation guidance.
func annotationDocs(t *testing.T) map[string]string {
	t.Helper()
	docs := map[string]string{}
	for rel, content := range shippedSkills(t) {
		docs[rel] = content
	}
	content, err := templates.ProjectTemplates().Get("reliant.md.tmpl")
	if err != nil {
		t.Fatalf("read reliant.md.tmpl: %v", err)
	}
	docs["reliant.md.tmpl"] = string(content)
	return docs
}

func TestSkillsAnnotationFieldReferencesExist(t *testing.T) {
	docs := annotationDocs(t)
	allow := allowlist(t)
	descs := annotationDescriptors()

	for rel, content := range docs {
		seen := map[string]bool{}
		for _, region := range annotationRegions(content) {
			for _, v := range scanAnnotationRegion(region, descs) {
				key := v.claim + "|" + v.reason
				if seen[key] {
					continue
				}
				seen[key] = true
				if allowed(allow, rel, v.claim) {
					continue
				}
				t.Errorf("%s: invalid forge.v1 annotation reference %q — %s\n  (schema truth: internal/assets/proto/forge/v1/forge.proto; fix the doc, or add to internal/templates/testdata/skills_validation_allowlist.txt with a justification)",
					rel, v.claim, v.reason)
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

	// Annotation validator: known-bad literals must be flagged.
	descs := annotationDescriptors()
	badMD := "```proto\n" +
		"message Task {\n" +
		"  option (forge.v1.entity) = {\n" +
		"    table_name: \"tasks\"    // no such field (real name: table)\n" +
		"    timestamps: true\n" +
		"  };\n" +
		"  string title = 1 [(forge.v1.field) = { validate: \"url\" }];\n" + // message field given a scalar
		"  string email = 2 [(forge.v1.field) = { store: true }];\n" + // enum field given a bool
		"}\n" +
		"```\n"
	var claims []string
	for _, region := range annotationRegions(badMD) {
		for _, v := range scanAnnotationRegion(region, descs) {
			claims = append(claims, v.claim)
		}
	}
	for _, want := range []string{
		"(forge.v1.entity).table_name",
		"(forge.v1.field).validate",
		"(forge.v1.field).store",
	} {
		if !slices.Contains(claims, want) {
			t.Errorf("annotation validator missed known-bad claim %s (got %v)", want, claims)
		}
	}
	if len(claims) != 3 {
		t.Errorf("annotation validator reported unexpected extra violations: %v", claims)
	}

	// Correct literals — including nested messages, lists, enum values,
	// colon-less message fields, and dotted references — must pass.
	goodMD := "```proto\n" +
		"message Task {\n" +
		"  option (forge.v1.entity) = {\n" +
		"    table: \"tasks\"\n" +
		"    soft_delete: true\n" +
		"    indexes: [{ name: \"by_org\", fields: [\"org_id\", \"status\"], unique: true }]\n" +
		"    middleware: [\"tracing\", \"metrics\"]\n" +
		"  };\n" +
		"  string url = 1 [(forge.v1.field) = { store: STORE_AS_JSONB, validate: { format: \"url\", min_length: 3 } }];\n" +
		"}\n" +
		"```\n" +
		"and `(forge.v1.service) = { name: \"tasks\" version: \"v1\" auth { auth_required: true } }`\n" +
		"and `(forge.v1.method) = { timeout: { seconds: 30 }, errors: [\"NotFound\"] }`\n" +
		"and `(forge.v1.method).auth_required = false` plus shorthand `(forge.v1.config) = { sensitive, category }`\n"
	for _, region := range annotationRegions(goodMD) {
		for _, v := range scanAnnotationRegion(region, descs) {
			t.Errorf("annotation validator flagged a correct reference: %s — %s", v.claim, v.reason)
		}
	}

	// Dotted references to nonexistent fields must be flagged.
	if vs := scanAnnotationRegion("(forge.v1.method).required_roles", descs); len(vs) == 0 {
		t.Error("annotation validator accepted dotted reference to nonexistent field (forge.v1.method).required_roles")
	}
}

// TestSkillsAllowlistEntriesStillNeeded keeps the allowlist from rotting:
// an entry whose claim no longer appears in the named skill (or whose
// skill no longer exists) must be removed.
func TestSkillsAllowlistEntriesStillNeeded(t *testing.T) {
	docs := annotationDocs(t) // every SKILL.md + reliant.md.tmpl
	allow := allowlist(t)
	for skillRel, claims := range allow {
		if skillRel == "*" {
			continue
		}
		content, ok := docs[skillRel]
		if !ok {
			t.Errorf("allowlist names skill %q which no longer ships", skillRel)
			continue
		}
		for claim := range claims {
			// The claim text is a substring of the skill in all three
			// validators' extraction paths. Annotation claims look like
			// "(forge.v1.entity).table_name" — the doc contains the final
			// field name, not the synthesized dotted form.
			needle := strings.TrimPrefix(claim, "forge ")
			if strings.HasPrefix(claim, "(forge.v1.") {
				if i := strings.LastIndex(claim, "."); i >= 0 {
					needle = claim[i+1:]
				}
			}
			if !strings.Contains(content, needle) {
				t.Errorf("allowlist entry %q|%q no longer matches anything in the skill — remove it", skillRel, claim)
			}
		}
	}
}
