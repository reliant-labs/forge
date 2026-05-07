// Package forgeconv implements lint rules that enforce forge codegen
// conventions on proto files. These analyzers exist because forge's
// codegen is annotation-driven (see internal/cli/orm_entity.go) — it does
// NOT auto-detect entity / pk / tenant / timestamp semantics by field name.
// The analyzers here catch the cases that previously failed silently or
// blew up at generate time, surfacing them as actionable lint findings
// with explicit remediation messages before `forge generate` runs.
//
// The full list of rules:
//
//	forgeconv-one-service-per-file   one service per .proto, full stop
//	forgeconv-pk-annotation          fields named `id` need `pk: true` (or message must mark some field PK)
//	forgeconv-timestamps             `*_at` Timestamp fields need entity timestamps:true OR field-level annotation
//	forgeconv-tenant-annotation      tenant-shaped field names need `tenant: true` when entity is tenant-scoped
//
// The package exposes a single LintProtoTree entry point that takes a
// project root (or any directory containing .proto files) and returns a
// Result. Findings are emitted in deterministic order (file, then byte
// position) so output is stable across runs.
package forgeconv

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Severity classifies a finding. Errors fail `forge lint`; warnings are
// printed but don't gate the build.
type Severity string

const (
	SeverityError   Severity = "error"
	SeverityWarning Severity = "warning"
)

// Finding is a single lint diagnostic against a proto file.
type Finding struct {
	Rule        string   `json:"rule"`
	Severity    Severity `json:"severity"`
	File        string   `json:"file"`
	Line        int      `json:"line"`     // 1-indexed; 0 if file-level
	Message     string   `json:"message"`
	Remediation string   `json:"remediation,omitempty"`
}

// Result aggregates findings from a single lint run.
type Result struct {
	Findings []Finding `json:"findings"`
}

// HasErrors returns true if any finding has Severity == SeverityError.
// Used by `forge lint` to decide exit status.
func (r Result) HasErrors() bool {
	for _, f := range r.Findings {
		if f.Severity == SeverityError {
			return true
		}
	}
	return false
}

// FormatText renders findings as a human-readable report. Empty result
// produces an empty string so callers can prefix their own success line.
func (r Result) FormatText() string {
	if len(r.Findings) == 0 {
		return ""
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d forge convention violation(s):\n\n", len(r.Findings)))
	for _, f := range r.Findings {
		icon := "✗"
		if f.Severity == SeverityWarning {
			icon = "⚠"
		}
		if f.Line > 0 {
			sb.WriteString(fmt.Sprintf("  %s [%s] %s:%d\n      %s\n", icon, f.Rule, f.File, f.Line, f.Message))
		} else {
			sb.WriteString(fmt.Sprintf("  %s [%s] %s\n      %s\n", icon, f.Rule, f.File, f.Message))
		}
		if f.Remediation != "" {
			sb.WriteString(fmt.Sprintf("      → %s\n", f.Remediation))
		}
	}
	return sb.String()
}

// LintProtoTree walks rootDir for .proto files and runs every analyzer.
// Files under proto/forge/ (vendored forge annotation protos) are
// skipped — they're external definitions, not user code. Returns a
// deterministic Result ordered by (file, line, rule).
func LintProtoTree(rootDir string) (Result, error) {
	var protoFiles []string
	err := filepath.Walk(rootDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			// Skip vendored forge annotations, the gen output dir, and
			// anything resembling a buf cache. Keeps the linter focused
			// on user-authored proto.
			base := info.Name()
			if base == ".buf" || base == "node_modules" || base == "gen" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".proto") {
			return nil
		}
		// Skip vendored forge annotation file specifically — it's external.
		if strings.Contains(filepath.ToSlash(path), "/proto/forge/") {
			return nil
		}
		protoFiles = append(protoFiles, path)
		return nil
	})
	if err != nil {
		return Result{}, fmt.Errorf("walk %s: %w", rootDir, err)
	}

	sort.Strings(protoFiles)

	var result Result
	for _, file := range protoFiles {
		content, err := os.ReadFile(file)
		if err != nil {
			return Result{}, fmt.Errorf("read %s: %w", file, err)
		}
		// Resolve to a path relative to rootDir for stable output across
		// machines (no /tmp/abc123 prefixes leaking into CI logs).
		rel, relErr := filepath.Rel(rootDir, file)
		if relErr != nil {
			rel = file
		}
		result.Findings = append(result.Findings, lintProtoFile(rel, string(content))...)
	}

	// Stable ordering: by file, then line, then rule.
	sort.SliceStable(result.Findings, func(i, j int) bool {
		if result.Findings[i].File != result.Findings[j].File {
			return result.Findings[i].File < result.Findings[j].File
		}
		if result.Findings[i].Line != result.Findings[j].Line {
			return result.Findings[i].Line < result.Findings[j].Line
		}
		return result.Findings[i].Rule < result.Findings[j].Rule
	})

	return result, nil
}

// lintProtoFile runs every rule against the parsed view of one .proto
// file. Exposed for testability (the test suite renders fake proto
// content via this function rather than going through filesystem walk).
func lintProtoFile(relPath, content string) []Finding {
	var findings []Finding
	pf := parseProtoFile(relPath, content)

	findings = append(findings, checkOneServicePerFile(pf)...)
	findings = append(findings, checkPKAnnotation(pf)...)
	findings = append(findings, checkTimestampAnnotation(pf)...)
	findings = append(findings, checkTenantAnnotation(pf)...)

	return findings
}

// ─── Rule 1: one service per .proto file ─────────────────────────────────────

func checkOneServicePerFile(pf parsedProto) []Finding {
	if len(pf.Services) <= 1 {
		return nil
	}
	// Report on the SECOND and subsequent services — the first one is
	// the canonical one to keep, and that's where we'd point the user
	// to split out additional services.
	var findings []Finding
	for _, svc := range pf.Services[1:] {
		findings = append(findings, Finding{
			Rule:     "forgeconv-one-service-per-file",
			Severity: SeverityError,
			File:     pf.Path,
			Line:     svc.Line,
			Message: fmt.Sprintf(
				"file declares %d services (%s); forge convention is one service per .proto file",
				len(pf.Services), serviceList(pf.Services)),
			Remediation: fmt.Sprintf(
				"split %q into its own .proto file at %s/%s.proto (see the proto-split skill)",
				svc.Name, filepath.Dir(pf.Path), strings.ToLower(svc.Name)),
		})
	}
	return findings
}

func serviceList(services []protoService) string {
	names := make([]string, len(services))
	for i, s := range services {
		names[i] = s.Name
	}
	return strings.Join(names, ", ")
}

// ─── Rule 2: PK annotation required on entity messages ───────────────────────

func checkPKAnnotation(pf parsedProto) []Finding {
	var findings []Finding
	for _, msg := range pf.Messages {
		if !msg.HasEntityAnnotation {
			// Not an entity, not our concern. The pk-annotation rule
			// only fires on messages that ARE entities — pure API
			// request/response messages with an `id` field are fine.
			continue
		}
		if msg.PKField != "" {
			// Some field is annotated `pk: true` — entity is well-formed.
			continue
		}

		// Entity-annotated message with no pk: true field.
		// Find the field named `id` if present, to point the error there;
		// otherwise point at the message header line.
		idField, hasID := findFieldByName(msg, "id")
		var line int
		var msgText, remediation string
		if hasID {
			line = idField.Line
			msgText = fmt.Sprintf(
				"field %q in entity %q is not marked `pk: true`; auto-detection by name was removed in forge v0.6",
				idField.Name, msg.Name)
			remediation = fmt.Sprintf(
				"change `%s %s = %d;` to `%s %s = %d [(forge.v1.field) = { pk: true }];`",
				idField.Type, idField.Name, idField.Number,
				idField.Type, idField.Name, idField.Number)
		} else {
			line = msg.Line
			msgText = fmt.Sprintf(
				"entity %q has no field marked `(forge.v1.field) = { pk: true }`; "+
					"every forge entity must declare a primary key explicitly",
				msg.Name)
			remediation = "add `[(forge.v1.field) = { pk: true }]` to the field that should be the primary key"
		}

		findings = append(findings, Finding{
			Rule:        "forgeconv-pk-annotation",
			Severity:    SeverityError,
			File:        pf.Path,
			Line:        line,
			Message:     msgText,
			Remediation: remediation,
		})
	}
	return findings
}

// ─── Rule 3: timestamp annotation for *_at Timestamp fields ──────────────────

func checkTimestampAnnotation(pf parsedProto) []Finding {
	var findings []Finding
	for _, msg := range pf.Messages {
		if !msg.HasEntityAnnotation {
			continue
		}
		if msg.HasTimestampsTrue {
			// Entity opts into timestamps:true — created_at/updated_at
			// are managed by the ORM. Don't second-guess named fields.
			continue
		}
		for _, field := range msg.Fields {
			if !isTimestampShapedFieldName(field.Name) {
				continue
			}
			if field.Type != "google.protobuf.Timestamp" {
				continue
			}
			// Already-declared `default_value: NOW()` etc. counts as
			// explicit annotation: the user took ownership.
			if field.HasFieldAnnotation {
				continue
			}
			findings = append(findings, Finding{
				Rule:     "forgeconv-timestamps",
				Severity: SeverityError,
				File:     pf.Path,
				Line:     field.Line,
				Message: fmt.Sprintf(
					"timestamp-shaped field %q in entity %q has no explicit annotation and the entity does not set `timestamps: true`",
					field.Name, msg.Name),
				Remediation: fmt.Sprintf(
					"either add `timestamps: true` to `option (forge.v1.entity)` (recommended for created_at/updated_at) "+
						"or annotate the field explicitly: `[(forge.v1.field) = { default_value: \"NOW()\" }]`"),
			})
		}
	}
	return findings
}

// isTimestampShapedFieldName returns true for the conventional
// audit-column names that the old auto-detect code special-cased.
// This is the *target* of the lint rule (not a heuristic that drives
// codegen) — these names trip a warning so the user is forced to
// declare intent.
func isTimestampShapedFieldName(name string) bool {
	return name == "created_at" || name == "updated_at" || name == "deleted_at" ||
		(strings.HasSuffix(name, "_at") && len(name) > len("_at"))
}

// ─── Rule 4: tenant annotation when entity is tenant-scoped ──────────────────

func checkTenantAnnotation(pf parsedProto) []Finding {
	var findings []Finding
	for _, msg := range pf.Messages {
		if !msg.HasEntityAnnotation {
			continue
		}
		// The rule fires only when SOME field in the entity is already
		// marked `tenant: true` — i.e., the entity is a tenant-scoped
		// entity — and a *different* field with a tenant-shaped name
		// is missing the annotation. This catches the foot-gun case
		// where an engineer adds another `*_id` column they think is
		// tenant-scoping but forgot the annotation.
		hasTenantField := false
		for _, f := range msg.Fields {
			if f.HasTenantTrue {
				hasTenantField = true
				break
			}
		}
		if !hasTenantField {
			continue
		}
		for _, field := range msg.Fields {
			if field.HasTenantTrue {
				continue
			}
			if !isTenantShapedFieldName(field.Name) {
				continue
			}
			findings = append(findings, Finding{
				Rule:     "forgeconv-tenant-annotation",
				Severity: SeverityWarning,
				File:     pf.Path,
				Line:     field.Line,
				Message: fmt.Sprintf(
					"field %q in entity %q has a tenant-shaped name but is not marked `tenant: true`; "+
						"the entity already has another tenant-scoped field — confirm this is intentional",
					field.Name, msg.Name),
				Remediation: "if this field is the tenant key, add `[(forge.v1.field) = { tenant: true }]`; " +
					"if it's just a tenant-scoped FK, this warning can be ignored",
			})
		}
	}
	return findings
}

func isTenantShapedFieldName(name string) bool {
	return name == "tenant_id" || name == "org_id" || name == "organization_id" ||
		name == "account_id" || name == "workspace_id"
}

// ─── proto file mini-parser ──────────────────────────────────────────────────
//
// Full proto parsing requires a dependency on a proto AST library; for
// lint-purposes a regex-based line scan is plenty. The parser tracks:
//
//   - service blocks (name + line)
//   - message blocks (name, line, has-entity-annotation, has-timestamps-true)
//   - field declarations inside top-level message blocks
//
// It does NOT parse nested messages, oneofs, options, or imports — those
// don't affect any of the rules above.

type parsedProto struct {
	Path     string
	Services []protoService
	Messages []protoMessage
}

type protoService struct {
	Name string
	Line int
}

type protoMessage struct {
	Name                string
	Line                int
	HasEntityAnnotation bool
	HasTimestampsTrue   bool
	HasSoftDeleteTrue   bool
	PKField             string // proto field name of the field marked pk: true (empty if none)
	Fields              []protoField
}

type protoField struct {
	Name               string
	Type               string
	Number             int
	Line               int
	HasFieldAnnotation bool // any (forge.v1.field) annotation present
	HasPKTrue          bool
	HasTenantTrue      bool
	HasTimestampTrue   bool
}

func findFieldByName(msg protoMessage, name string) (protoField, bool) {
	for _, f := range msg.Fields {
		if f.Name == name {
			return f, true
		}
	}
	return protoField{}, false
}

var (
	// reService matches `service Foo {`. Not anchored to ^ so a single
	// line carrying two service decls (which is itself a violation that
	// rule #1 catches) reports both names.
	reService = regexp.MustCompile(`(?:^|\W)service\s+(\w+)\s*\{`)
	reMessage = regexp.MustCompile(`^\s*message\s+(\w+)\s*\{`)
	// Field declaration: optional `optional`/`repeated` qualifier, type,
	// name, `=`, number. Trailing `[ ... ]` annotation block (if any) is
	// captured separately by the line-aggregation logic below.
	reField        = regexp.MustCompile(`^\s*(?:(?:optional|repeated)\s+)?([\w.]+)\s+(\w+)\s*=\s*(\d+)`)
	rePKTrue       = regexp.MustCompile(`\bpk\s*:\s*true\b`)
	reTenantTrue   = regexp.MustCompile(`\btenant\s*:\s*true\b`)
	reTimestampTrue = regexp.MustCompile(`\btimestamp\s*:\s*true\b`)
	reTimestampsTrue = regexp.MustCompile(`\btimestamps\s*:\s*true\b`)
	reSoftDeleteTrue = regexp.MustCompile(`\bsoft_delete\s*:\s*true\b`)
	reEntityOpt    = regexp.MustCompile(`\(forge\.v1\.entity\)`)
	reFieldOpt     = regexp.MustCompile(`\(forge\.v1\.field\)`)
)

// parseProtoFile is a forgiving line-and-brace-counting scanner. It
// produces a parsedProto struct with everything the lint rules need,
// without taking on a real proto AST dependency. The parser must
// handle:
//
//   - multi-line option blocks (`option (forge.v1.entity) = { ... };`)
//   - field annotations that span multiple lines:
//       string id = 1 [(forge.v1.field) = {
//         pk: true
//       }];
//   - nested braces (entity options blocks contain `indexes: [{...}]`)
//
// We track `messageDepth`, `optionDepth` (depth INSIDE a message option
// block — i.e. inside `option (forge.v1.entity) = { ... };`), and
// `fieldAnnoDepth` (inside a field's `[ ... ]` annotation). A field's
// annotation block can contain nested braces (`{ pk: true }`), so we
// follow `[` / `]` to know when we leave it.
func parseProtoFile(path, content string) parsedProto {
	pf := parsedProto{Path: path}

	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	var (
		lineNum            int
		braceDepth         int
		inMessage          bool
		currentMessage     *protoMessage
		messageBraceDepth  int
		inEntityOpts       bool
		entityOptsDepth    int
		// pendingField holds a field-in-progress when the field's annotation
		// (the [...] part) spans multiple lines.
		pendingField     *protoField
		pendingBracketDepth int
	)

	for scanner.Scan() {
		lineNum++
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") || trimmed == "" {
			// Still count the braces inside multi-line options blocks
			// in case someone puts braces inside a comment — but
			// realistically not worth it. Skip the line entirely.
			continue
		}
		// Comment-stripping: drop everything after `//` so we don't
		// false-positive on annotation strings inside comments.
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
			trimmed = strings.TrimSpace(line)
			if trimmed == "" {
				continue
			}
		}

		// Detect service blocks at top level. Multiple service decls on
		// one line (`service Foo {} service Bar {}`) are rare in
		// practice but a common bad-fixture shape, and the smoke test
		// in `forge lint`'s docs explicitly does this. Use FindAll so
		// each service is recorded.
		if !inMessage && braceDepth == 0 {
			matches := reService.FindAllStringSubmatch(trimmed, -1)
			if len(matches) > 0 {
				for _, m := range matches {
					pf.Services = append(pf.Services, protoService{Name: m[1], Line: lineNum})
				}
				braceDepth += strings.Count(line, "{")
				braceDepth -= strings.Count(line, "}")
				continue
			}
		}

		// Detect message blocks (top-level only — nested messages don't
		// participate in entity annotations in practice).
		if !inMessage && braceDepth == 0 {
			if m := reMessage.FindStringSubmatch(trimmed); m != nil {
				newMsg := protoMessage{Name: m[1], Line: lineNum}
				pf.Messages = append(pf.Messages, newMsg)
				currentMessage = &pf.Messages[len(pf.Messages)-1]
				inMessage = true
				messageBraceDepth = 1
				continue
			}
			// Track top-level braces (e.g. enum blocks) so we don't
			// accidentally pick up fields inside non-message decls.
			braceDepth += strings.Count(line, "{")
			braceDepth -= strings.Count(line, "}")
			continue
		}

		if !inMessage {
			braceDepth += strings.Count(line, "{")
			braceDepth -= strings.Count(line, "}")
			continue
		}

		// We're inside a message block. Update brace depth.
		opens := strings.Count(line, "{")
		closes := strings.Count(line, "}")
		messageBraceDepth += opens - closes

		// Detect (and remain inside) entity options blocks.
		if reEntityOpt.MatchString(line) {
			currentMessage.HasEntityAnnotation = true
			inEntityOpts = true
			entityOptsDepth = 0
		}
		if inEntityOpts {
			entityOptsDepth += opens - closes
			if reTimestampsTrue.MatchString(line) {
				currentMessage.HasTimestampsTrue = true
			}
			if reSoftDeleteTrue.MatchString(line) {
				currentMessage.HasSoftDeleteTrue = true
			}
			// Exit the options block once we've returned to message-
			// surface depth. Use `<= 0` because the same line may both
			// open and close the options block in single-line forms.
			if entityOptsDepth <= 0 && (strings.Contains(line, "}") || strings.Contains(line, ";")) {
				inEntityOpts = false
			}
		}

		// Continue accumulating multi-line field annotations.
		if pendingField != nil {
			pendingBracketDepth += strings.Count(line, "[") - strings.Count(line, "]")
			if rePKTrue.MatchString(line) {
				pendingField.HasPKTrue = true
			}
			if reTenantTrue.MatchString(line) {
				pendingField.HasTenantTrue = true
			}
			if reTimestampTrue.MatchString(line) {
				pendingField.HasTimestampTrue = true
			}
			if pendingBracketDepth <= 0 {
				// Done — flush.
				if pendingField.HasPKTrue && currentMessage.PKField == "" {
					currentMessage.PKField = pendingField.Name
				}
				currentMessage.Fields = append(currentMessage.Fields, *pendingField)
				pendingField = nil
				pendingBracketDepth = 0
			}
			// Whether we closed it or not, we've consumed this line as
			// part of the field's annotation. Skip the close-message
			// check for this line (the field's `];` doesn't close the
			// message).
			if messageBraceDepth <= 0 {
				inMessage = false
				currentMessage = nil
			}
			continue
		}

		// Try to detect a new field declaration.
		if messageBraceDepth >= 1 && !inEntityOpts {
			if m := reField.FindStringSubmatch(trimmed); m != nil {
				// Skip lines that are option declarations dressed up
				// like fields (e.g. `option foo = bar;`).
				if strings.HasPrefix(trimmed, "option ") || strings.HasPrefix(trimmed, "reserved ") {
					// Fall through — not a field.
				} else {
					num := 0
					_, _ = fmt.Sscanf(m[3], "%d", &num)
					field := protoField{
						Name:   m[2],
						Type:   m[1],
						Number: num,
						Line:   lineNum,
					}
					if reFieldOpt.MatchString(line) {
						field.HasFieldAnnotation = true
					}
					if rePKTrue.MatchString(line) {
						field.HasPKTrue = true
					}
					if reTenantTrue.MatchString(line) {
						field.HasTenantTrue = true
					}
					if reTimestampTrue.MatchString(line) {
						field.HasTimestampTrue = true
					}

					// Multi-line annotation: opens `[` without closing `]`.
					openBrackets := strings.Count(line, "[")
					closeBrackets := strings.Count(line, "]")
					if openBrackets > closeBrackets {
						pendingField = &field
						pendingBracketDepth = openBrackets - closeBrackets
					} else {
						if field.HasPKTrue && currentMessage.PKField == "" {
							currentMessage.PKField = field.Name
						}
						currentMessage.Fields = append(currentMessage.Fields, field)
					}
				}
			}
		}

		if messageBraceDepth <= 0 {
			inMessage = false
			currentMessage = nil
			messageBraceDepth = 0
			inEntityOpts = false
			entityOptsDepth = 0
		}
	}

	return pf
}
