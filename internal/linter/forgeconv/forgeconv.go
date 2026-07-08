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
//	forgeconv-method-auth-annotation each RPC declares its auth posture via (forge.v1.method); auth-by-omission is a security hazard
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

	"github.com/reliant-labs/forge/internal/linter/finding"
)

// Severity and Finding now live in the shared internal/linter/finding
// package — these aliases keep the historical forgeconv.* spellings
// working for callers and tests while the underlying vocabulary is
// single-sourced. forgeconv findings populate
// Rule/Severity/File/Line/Message/Remediation.
type (
	// Severity is the shared finding severity vocabulary, re-exported
	// under the historical forgeconv spelling.
	Severity = finding.Severity
	// Finding is the shared linter finding shape, re-exported under the
	// historical forgeconv spelling. forgeconv findings populate
	// Rule/Severity/File/Line/Message/Remediation.
	Finding = finding.Finding
)

// Severity enum values (aliases onto the canonical single-spelling set).
const (
	SeverityError   = finding.SeverityError
	SeverityWarning = finding.SeverityWarning
)

// Result aggregates findings from a single lint run. It is a distinct
// type (not an alias) so forgeconv can hang its own FormatText rendering
// on it; the finding vocabulary inside is the shared one.
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
	fmt.Fprintf(&sb, "Found %d forge convention violation(s):\n\n", len(r.Findings))
	for _, f := range r.Findings {
		icon := "✗"
		if f.Severity == SeverityWarning {
			icon = "⚠"
		}
		if f.Line > 0 {
			fmt.Fprintf(&sb, "  %s [%s] %s:%d\n      %s\n", icon, f.Rule, f.File, f.Line, f.Message)
		} else {
			fmt.Fprintf(&sb, "  %s [%s] %s\n      %s\n", icon, f.Rule, f.File, f.Message)
		}
		if f.Remediation != "" {
			fmt.Fprintf(&sb, "      → %s\n", f.Remediation)
		}
	}
	return sb.String()
}

// LintOptions tunes the proto convention analyzers. The zero value is
// the default (advisory) posture; callers opt into stricter gating.
type LintOptions struct {
	// Strict escalates advisory security findings to errors. Today this
	// flips forgeconv-method-auth-annotation from warning to error so a
	// missing `(forge.v1.method)` annotation fails `forge lint --strict`
	// (and, by extension, CI). The default keeps it a warning so the rule
	// can land without breaking existing trees on day one — see
	// FORGE_SHAPE_REDESIGN §7e (auth-by-omission is a security hazard;
	// the long-term intent is default-deny / required annotation).
	Strict bool
}

// LintProtoTree walks rootDir for .proto files and runs every analyzer
// in the default (advisory) posture. Thin wrapper over LintProtoTreeOpts
// kept for the existing call sites + tests.
func LintProtoTree(rootDir string) (Result, error) {
	return LintProtoTreeOpts(rootDir, LintOptions{})
}

// LintProtoTreeOpts walks rootDir for .proto files and runs every
// analyzer with the supplied options. Files under proto/forge/ (vendored
// forge annotation protos) are skipped — they're external definitions,
// not user code. Returns a deterministic Result ordered by
// (file, line, rule).
func LintProtoTreeOpts(rootDir string, opts LintOptions) (Result, error) {
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
		result.Findings = append(result.Findings, lintProtoFile(rel, string(content), opts)...)
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
func lintProtoFile(relPath, content string, opts LintOptions) []Finding {
	var findings []Finding
	pf := parseProtoFile(relPath, content)

	findings = append(findings, checkOneServicePerFile(pf)...)
	findings = append(findings, checkPKAnnotation(pf)...)
	findings = append(findings, checkTimestampAnnotation(pf)...)
	findings = append(findings, checkTenantAnnotation(pf)...)
	findings = append(findings, checkMethodAuthAnnotation(pf, opts)...)

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
					"either add `timestamps: true` to `option (forge.v1.entity)` (recommended for created_at/updated_at) " +
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

// ─── Rule 5: every RPC declares its auth posture ─────────────────────────────
//
// Auth-by-omission is a security hazard (FORGE_SHAPE_REDESIGN §7e): when
// an RPC carries no `(forge.v1.method)` annotation, the auth posture is
// implicit. forge's descriptor parser defaults unannotated methods to
// fail-closed (auth_required = true), so the *runtime* default is safe —
// but the proto then SAYS NOTHING about intent, and a reviewer can't tell
// "deliberately authenticated" from "nobody thought about it." A PUBLIC
// endpoint shipped by forgetting the annotation is the dangerous case the
// inverse can't catch: the proto reads identically whether the author
// meant public-by-mistake or authenticated-by-default.
//
// The rule therefore requires EVERY RPC to declare the annotation
// explicitly — `auth_required: true` for authenticated, `false` for
// deliberately public. Default severity is warning so the rule can land
// without breaking sparsely-annotated trees (control-plane today); strict
// mode (`forge lint --strict`) escalates to error to gate CI on the
// default-deny intent.
func checkMethodAuthAnnotation(pf parsedProto, opts LintOptions) []Finding {
	severity := SeverityWarning
	if opts.Strict {
		severity = SeverityError
	}
	var findings []Finding
	for _, svc := range pf.Services {
		for _, m := range svc.Methods {
			if m.HasMethodAnnotation {
				continue
			}
			findings = append(findings, Finding{
				Rule:     "forgeconv-method-auth-annotation",
				Severity: severity,
				File:     pf.Path,
				Line:     m.Line,
				Message: fmt.Sprintf(
					"RPC %q in service %q declares no `(forge.v1.method)` annotation; its auth posture is implicit (forge defaults to auth-required, but the proto records no intent — auth-by-omission is a security hazard)",
					m.Name, svc.Name),
				Remediation: "declare the posture explicitly: `option (forge.v1.method) = { auth_required: true };` " +
					"for an authenticated RPC, or `{ auth_required: false }` for a deliberately public one",
			})
		}
	}
	return findings
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
	// Methods lists the RPC declarations inside this service block, with
	// per-method auth-annotation presence. Used by the method-auth rule.
	Methods []protoMethod
}

// protoMethod is one `rpc Name(Req) returns (Resp)` declaration. The
// linter tracks only whether a `(forge.v1.method)` option block is
// present — auth posture (auth_required true/false) is read from the
// real descriptor at generate time; the lint only guards against the
// annotation being ABSENT entirely (auth-by-omission).
type protoMethod struct {
	Name string
	Line int
	// HasMethodAnnotation is true when the RPC body declares
	// `option (forge.v1.method) = { ... }` (the auth-posture marker).
	HasMethodAnnotation bool
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
	reField = regexp.MustCompile(`^\s*(?:(?:optional|repeated)\s+)?([\w.]+)\s+(\w+)\s*=\s*(\d+)`)
	// reRPC matches `rpc MethodName(` at the start of an RPC declaration.
	// The remainder (request/response/options block) is handled by the
	// brace/annotation scanner so multi-line RPC bodies are supported.
	reRPC            = regexp.MustCompile(`^\s*rpc\s+(\w+)\s*\(`)
	reMethodOpt      = regexp.MustCompile(`\(forge\.v1\.method\)`)
	rePKTrue         = regexp.MustCompile(`\bpk\s*:\s*true\b`)
	reTenantTrue     = regexp.MustCompile(`\btenant\s*:\s*true\b`)
	reTimestampTrue  = regexp.MustCompile(`\btimestamp\s*:\s*true\b`)
	reTimestampsTrue = regexp.MustCompile(`\btimestamps\s*:\s*true\b`)
	reSoftDeleteTrue = regexp.MustCompile(`\bsoft_delete\s*:\s*true\b`)
	reEntityOpt      = regexp.MustCompile(`\(forge\.v1\.entity\)`)
	reFieldOpt       = regexp.MustCompile(`\(forge\.v1\.field\)`)
)

// parseProtoFile is a forgiving line-and-brace-counting scanner. It
// produces a parsedProto struct with everything the lint rules need,
// without taking on a real proto AST dependency. The parser must
// handle:
//
//   - multi-line option blocks (`option (forge.v1.entity) = { ... };`)
//   - field annotations that span multiple lines:
//     string id = 1 [(forge.v1.field) = {
//     pk: true
//     }];
//   - nested braces (entity options blocks contain `indexes: [{...}]`)
//
// We track `messageDepth`, `optionDepth` (depth INSIDE a message option
// block — i.e. inside `option (forge.v1.entity) = { ... };`), and
// `fieldAnnoDepth` (inside a field's `[ ... ]` annotation). A field's
// annotation block can contain nested braces (`{ pk: true }`), so we
// follow `[` / `]` to know when we leave it.
// protoScanState holds the mutable state threaded through the line-based
// proto scanner. The scanning logic used to live in one large function;
// it is now split across focused per-construct helpers that operate on
// this state. Behavior (parse results, ordering of emitted entities, and
// every edge case) is identical to the original single-function form.
type protoScanState struct {
	pf parsedProto

	lineNum    int
	braceDepth int

	// Message-block tracking.
	inMessage         bool
	currentMessage    *protoMessage
	messageBraceDepth int
	inEntityOpts      bool
	entityOptsDepth   int

	// Service-block tracking: when inside a `service Foo { ... }` block
	// we scan for `rpc` declarations and per-RPC `(forge.v1.method)`
	// annotations so the method-auth rule can flag auth-by-omission.
	inService         bool
	currentService    *protoService
	serviceBraceDepth int
	// pendingRPC holds an RPC-in-progress whose options block may span
	// multiple lines (`rpc X(..) returns (..) { option (...) = {..}; }`).
	// We keep accumulating annotation presence until the RPC body
	// closes (or, for single-line `rpc X(..) returns (..);`, immediately).
	pendingRPC   *protoMethod
	rpcBodyDepth int

	// pendingField holds a field-in-progress when the field's annotation
	// (the [...] part) spans multiple lines.
	pendingField        *protoField
	pendingBracketDepth int
}

func parseProtoFile(path, content string) parsedProto {
	s := &protoScanState{pf: parsedProto{Path: path}}

	scanner := bufio.NewScanner(strings.NewReader(content))
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)

	for scanner.Scan() {
		s.lineNum++
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

		if s.maybeStartService(line, trimmed) {
			continue
		}
		if s.inService {
			s.scanServiceLine(line, trimmed)
			continue
		}
		if s.handleOutsideMessage(line, trimmed) {
			continue
		}
		s.scanMessageBodyLine(line, trimmed)
	}

	return s.pf
}

// maybeStartService handles a top-level `service Foo { ... }` opener and
// reports whether the line was fully consumed (caller should `continue`).
//
// Multiple service decls on one line (`service Foo {} service Bar {}`)
// are rare in practice but a common bad-fixture shape, and the smoke
// test in `forge lint`'s docs explicitly does this. Use FindAll so each
// service is recorded.
func (s *protoScanState) maybeStartService(line, trimmed string) bool {
	if s.inMessage || s.inService || s.braceDepth != 0 {
		return false
	}
	matches := reService.FindAllStringSubmatch(trimmed, -1)
	if len(matches) == 0 {
		return false
	}
	for _, m := range matches {
		s.pf.Services = append(s.pf.Services, protoService{Name: m[1], Line: s.lineNum})
	}
	netBraces := strings.Count(line, "{") - strings.Count(line, "}")
	// A single-line `service Foo {}` opens and closes on the same line —
	// don't enter the block. Only the canonical multi-line form (net
	// brace depth > 0) becomes the active service we scan RPCs inside;
	// multi-service-per-line is a rule-1 violation anyway, so scan only
	// the first.
	if netBraces > 0 && len(matches) == 1 {
		s.inService = true
		s.currentService = &s.pf.Services[len(s.pf.Services)-1]
		s.serviceBraceDepth = netBraces
	} else {
		s.braceDepth += netBraces
	}
	return true
}

// scanServiceLine scans one line inside an open service block, tracking
// RPC declarations and per-RPC (forge.v1.method) annotations. RPC bodies
// may be single-line (`rpc X(..) returns (..);`) or span multiple lines
// with an `{ option (forge.v1.method) = {..}; }` body.
func (s *protoScanState) scanServiceLine(line, trimmed string) {
	netBraces := strings.Count(line, "{") - strings.Count(line, "}")
	s.serviceBraceDepth += netBraces
	if s.pendingRPC != nil {
		s.advancePendingRPC(line, netBraces)
	} else if m := reRPC.FindStringSubmatch(trimmed); m != nil {
		s.startRPC(line, m, netBraces)
	}
	// Leaving the service block.
	if s.serviceBraceDepth <= 0 {
		if s.pendingRPC != nil {
			s.currentService.Methods = append(s.currentService.Methods, *s.pendingRPC)
			s.pendingRPC = nil
		}
		s.inService = false
		s.currentService = nil
		s.serviceBraceDepth = 0
		s.rpcBodyDepth = 0
	}
}

// advancePendingRPC continues accumulating an open RPC body's annotation
// presence and flushes the RPC once its body closes.
func (s *protoScanState) advancePendingRPC(line string, netBraces int) {
	if reMethodOpt.MatchString(line) {
		s.pendingRPC.HasMethodAnnotation = true
	}
	s.rpcBodyDepth += netBraces
	if s.rpcBodyDepth <= 0 {
		s.currentService.Methods = append(s.currentService.Methods, *s.pendingRPC)
		s.pendingRPC = nil
		s.rpcBodyDepth = 0
	}
}

// startRPC records a newly-detected RPC declaration, either flushing it
// immediately (single-line form) or holding it open (multi-line body).
func (s *protoScanState) startRPC(line string, m []string, netBraces int) {
	method := protoMethod{Name: m[1], Line: s.lineNum}
	if reMethodOpt.MatchString(line) {
		method.HasMethodAnnotation = true
	}
	// Count only braces that open the RPC body `{ ... }`, not the
	// `( ... )` request/response parens. If the line has a net-positive
	// brace depth the body is open across lines.
	if netBraces > 0 {
		s.pendingRPC = &method
		s.rpcBodyDepth = netBraces
	} else {
		s.currentService.Methods = append(s.currentService.Methods, method)
	}
}

// handleOutsideMessage handles lines seen while not inside a message
// block: it opens a new top-level message or tracks brace depth for
// non-message decls (e.g. enum blocks) so we don't pick up their fields.
// It reports whether the line was consumed (caller should `continue`);
// it returns false only when we are inside a message and the line needs
// field-level scanning.
func (s *protoScanState) handleOutsideMessage(line, trimmed string) bool {
	// Detect message blocks (top-level only — nested messages don't
	// participate in entity annotations in practice).
	if !s.inMessage && s.braceDepth == 0 {
		if m := reMessage.FindStringSubmatch(trimmed); m != nil {
			s.pf.Messages = append(s.pf.Messages, protoMessage{Name: m[1], Line: s.lineNum})
			s.currentMessage = &s.pf.Messages[len(s.pf.Messages)-1]
			s.inMessage = true
			s.messageBraceDepth = 1
			return true
		}
		s.braceDepth += strings.Count(line, "{")
		s.braceDepth -= strings.Count(line, "}")
		return true
	}
	if !s.inMessage {
		s.braceDepth += strings.Count(line, "{")
		s.braceDepth -= strings.Count(line, "}")
		return true
	}
	return false
}

// scanMessageBodyLine scans one line inside an open message block: it
// updates brace depth, tracks entity option blocks, accumulates multi-
// line field annotations, and detects new field declarations, closing
// the message when its brace depth returns to zero.
func (s *protoScanState) scanMessageBodyLine(line, trimmed string) {
	// We're inside a message block. Update brace depth.
	opens := strings.Count(line, "{")
	closes := strings.Count(line, "}")
	s.messageBraceDepth += opens - closes

	s.trackEntityOpts(line, opens, closes)

	// Continue accumulating multi-line field annotations.
	if s.pendingField != nil {
		s.advancePendingField(line)
		return
	}

	s.maybeDetectField(line, trimmed)

	if s.messageBraceDepth <= 0 {
		s.inMessage = false
		s.currentMessage = nil
		s.messageBraceDepth = 0
		s.inEntityOpts = false
		s.entityOptsDepth = 0
	}
}

// trackEntityOpts detects and remains inside `option (forge.v1.entity)`
// blocks, recording the entity-level flags they carry.
func (s *protoScanState) trackEntityOpts(line string, opens, closes int) {
	if reEntityOpt.MatchString(line) {
		s.currentMessage.HasEntityAnnotation = true
		s.inEntityOpts = true
		s.entityOptsDepth = 0
	}
	if !s.inEntityOpts {
		return
	}
	s.entityOptsDepth += opens - closes
	if reTimestampsTrue.MatchString(line) {
		s.currentMessage.HasTimestampsTrue = true
	}
	if reSoftDeleteTrue.MatchString(line) {
		s.currentMessage.HasSoftDeleteTrue = true
	}
	// Exit the options block once we've returned to message-surface
	// depth. Use `<= 0` because the same line may both open and close the
	// options block in single-line forms.
	if s.entityOptsDepth <= 0 && (strings.Contains(line, "}") || strings.Contains(line, ";")) {
		s.inEntityOpts = false
	}
}

// advancePendingField accumulates a field's multi-line `[ ... ]`
// annotation and flushes the field once the annotation closes. The line
// is consumed as part of the field annotation, so the field's `];` does
// not itself close the message — but we still honor a message that
// closed on the same line.
func (s *protoScanState) advancePendingField(line string) {
	s.pendingBracketDepth += strings.Count(line, "[") - strings.Count(line, "]")
	if rePKTrue.MatchString(line) {
		s.pendingField.HasPKTrue = true
	}
	if reTenantTrue.MatchString(line) {
		s.pendingField.HasTenantTrue = true
	}
	if reTimestampTrue.MatchString(line) {
		s.pendingField.HasTimestampTrue = true
	}
	if s.pendingBracketDepth <= 0 {
		// Done — flush.
		if s.pendingField.HasPKTrue && s.currentMessage.PKField == "" {
			s.currentMessage.PKField = s.pendingField.Name
		}
		s.currentMessage.Fields = append(s.currentMessage.Fields, *s.pendingField)
		s.pendingField = nil
		s.pendingBracketDepth = 0
	}
	if s.messageBraceDepth <= 0 {
		s.inMessage = false
		s.currentMessage = nil
	}
}

// maybeDetectField tries to detect a new field declaration on this line,
// either flushing it immediately or holding it open when its annotation
// spans multiple lines (opens `[` without closing `]`).
func (s *protoScanState) maybeDetectField(line, trimmed string) {
	if s.messageBraceDepth < 1 || s.inEntityOpts {
		return
	}
	m := reField.FindStringSubmatch(trimmed)
	if m == nil || isProtoOptionLine(trimmed) {
		return
	}
	field := parseProtoFieldLine(m, line, s.lineNum)

	// Multi-line annotation: opens `[` without closing `]`.
	openBrackets := strings.Count(line, "[")
	closeBrackets := strings.Count(line, "]")
	if openBrackets > closeBrackets {
		s.pendingField = &field
		s.pendingBracketDepth = openBrackets - closeBrackets
		return
	}
	if field.HasPKTrue && s.currentMessage.PKField == "" {
		s.currentMessage.PKField = field.Name
	}
	s.currentMessage.Fields = append(s.currentMessage.Fields, field)
}

// isProtoOptionLine reports whether a trimmed proto line is an option
// or reserved declaration dressed up to look like a field.
func isProtoOptionLine(trimmed string) bool {
	return strings.HasPrefix(trimmed, "option ") || strings.HasPrefix(trimmed, "reserved ")
}

// parseProtoFieldLine builds a protoField from a regex match plus the
// raw line content. Inline forge annotations (pk, tenant, timestamp
// flags) are extracted by separate regexes against the full line so
// the parser stays line-based and tolerates ordering quirks.
func parseProtoFieldLine(m []string, line string, lineNum int) protoField {
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
	return field
}
