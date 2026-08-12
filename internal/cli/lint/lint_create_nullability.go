// File: internal/cli/lint/lint_create_nullability.go
//
// create-nullability — `forge lint --create-nullability`.
//
// A field's `optional` label is its PRESENCE contract, and
// Create<Entity>Request is the one CRUD envelope that FLATTENS the entity
// rather than nesting it. Flattening re-declares every field, which means
// the label has to be re-declared too — and a label that is dropped in the
// re-declaration fails in a way nothing else in the stack notices.
//
// Concretely: `optional string source_lead_id` on the entity generates Go
// `*string`, so absent is nil and distinguishable from "". Re-declared on
// the Create request WITHOUT the label it generates a plain `string`, and
// the two states collapse. The generated create then carries "" where the
// caller sent nothing, the insert writes "" into a nullable FK column, and
// postgres answers with a foreign-key violation at runtime — a failure
// whose message names a constraint, not the proto line that caused it.
// Worse, the honest-looking `if req.SourceLeadId != nil` guard in the
// generated op is dead code against a non-pointer: it reads as though
// presence were handled.
//
// The inverse direction is a bug too, and quieter: a field authored WITHOUT
// the label but declared `optional` on the Create request lets a caller
// omit a value the entity says is always present, and the zero value is
// written with no complaint.
//
// ── Why lint, and not (only) the birth path ───────────────────────────────
//
// Birth already gets this right: protoFieldDecl renders the label into the
// field's projected type, so a born Create request reproduces it
// (TestCreateRequestPreservesOptionalLabel pins the output). But birth is
// ONE-TIME. After it, the proto is the author's file — they add fields,
// change labels, hand-edit envelopes, and paste messages between services,
// and forge never reconciles the result. That is the intended contract, and
// it is exactly why the invariant needs a checker that runs continuously
// over what is on disk rather than only at the moment of writing.
//
// So this check is the safety net for two distinct populations: proto edits
// made after birth, and any future regression in the birth path itself.
//
// ── What it checks ────────────────────────────────────────────────────────
//
// For each message that has a sibling `Create<Entity>Request`, every field
// present in BOTH must agree on `optional`. The comparison is over the raw
// scan's parsed labels, not text, so spacing and comments do not matter.
//
// FALSE POSITIVES the check deliberately avoids:
//
//   - A read-only / computed field is ABSENT from the Create request by
//     design (the marker's whole purpose). Absent is not disagreement, and
//     the check only ever compares fields present on both sides — so the
//     omission is invisible to it rather than specially cased.
//   - A field the author deliberately left off the Create request (a
//     server-assigned value, a field write-able only through a custom RPC)
//     is likewise absent, and likewise not compared. The check never
//     demands that Create carry a field; it only demands that a field it
//     DOES carry agree about presence.
//   - `repeated` is not `optional` and is never compared against it: proto3
//     forbids `optional repeated`, so the labels are mutually exclusive and
//     a repeated field on both sides agrees with itself.
//   - Managed lifecycle fields (id / created_at / updated_at / deleted_at)
//     are server-owned and never on a write envelope; they are skipped
//     explicitly so a project that DOES list one cannot produce a finding
//     the author is not allowed to act on.
//   - A message with no Create request at all (a filter, a nested value
//     type, an envelope) is not an entity in this sense and is skipped.
//
// Severity is ERROR, unlike the advisory proto lints. The three siblings
// that warn — proto-markers, proto-options, column-markers — all warn
// because their subject is ambiguous: an unknown marker might be a future
// forge's, an unknown option might be a project's own extension. There is
// no such ambiguity here. Two declarations of the same field disagreeing
// about presence is not a style the author might have chosen; it silently
// corrupts writes, and it has exactly one correct resolution.

package lint

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/codegen"
)

// createNullabilityFinding is one field whose `optional` label disagrees
// between an entity message and its Create request. Line points at the
// offending line in the CREATE request — the side that is almost always
// wrong, since the entity is where the author declared their intent.
type createNullabilityFinding struct {
	File   string
	Line   int
	Entity string
	Field  string
	// EntityOptional is the label as authored on the entity message;
	// CreateOptional is the label on the Create request. They always
	// differ (that is what makes it a finding), so one bool would do —
	// both are carried so the message can state each side explicitly
	// rather than making the reader infer the other from a negation.
	EntityOptional bool
	CreateOptional bool
}

// createNullabilityFixHint renders the remediation: which line to change,
// and what breaks if it is not changed. The two directions fail
// differently, so they get different explanations — a hint that described
// only the common direction would misdescribe the other half.
func createNullabilityFixHint(f createNullabilityFinding) string {
	if f.EntityOptional {
		return fmt.Sprintf(
			"%s.%s is `optional` on the entity but not on Create%sRequest. The flattened field "+
				"generates a non-pointer Go field there, so absent and empty-string become "+
				"indistinguishable and the create writes the zero value into a nullable column "+
				"(a nullable FK then fails with a foreign-key violation at runtime). "+
				"Add `optional` to the Create request field: `optional <type> %s = N;`",
			f.Entity, f.Field, f.Entity, f.Field)
	}
	return fmt.Sprintf(
		"%s.%s is `optional` on Create%sRequest but not on the entity. The entity says this "+
			"field is always present, so a caller that omits it has the zero value written with "+
			"no complaint. Drop `optional` from the Create request field, or add it to the "+
			"entity if the value really is absent sometimes.",
		f.Entity, f.Field, f.Entity)
}

// runCreateNullabilityLint is the text-mode entry point.
func runCreateNullabilityLint(protoDir string) error {
	fmt.Println("Running create-nullability lint...")
	findings, err := collectCreateNullabilityFindings(protoDir)
	if err != nil {
		return err
	}
	formatCreateNullability(os.Stdout, findings)
	if len(findings) > 0 {
		return fmt.Errorf("%d create-request nullability mismatch(es)", len(findings))
	}
	return nil
}

// formatCreateNullability writes the human report.
func formatCreateNullability(w io.Writer, findings []createNullabilityFinding) {
	if len(findings) == 0 {
		_, _ = fmt.Fprintln(w, "  create-nullability clean — every Create request agrees with its entity on field presence")
		return
	}
	for _, f := range findings {
		_, _ = fmt.Fprintf(w, "  ✖ [forgeconv-create-request-nullability] %s:%d\n", f.File, f.Line)
		_, _ = fmt.Fprintf(w, "      → %s\n", createNullabilityFixHint(f))
	}
	_, _ = fmt.Fprintf(w, "\n%d create-request nullability mismatch(es).\n", len(findings))
}

// managedLifecycleFields are server-owned and never appear on a write
// envelope. Listed so a project that DOES declare one on its Create
// request cannot produce a finding whose fix forge would itself undo.
var managedLifecycleFields = map[string]bool{
	"id": true, "created_at": true, "updated_at": true, "deleted_at": true,
}

// collectCreateNullabilityFindings is the shared engine behind text mode
// and `forge lint --json`. A missing proto directory is not an error —
// CLI and library projects have no proto tree — it just yields nothing.
//
// The scan is per-DIRECTORY because that is the unit ScanRawProtoDir
// resolves type names within, and because an entity and its Create request
// are in the same proto package by construction (forge never emits a
// cross-package envelope, and a hand-written one could not resolve the
// entity type anyway).
func collectCreateNullabilityFindings(protoDir string) ([]createNullabilityFinding, error) {
	if _, err := os.Stat(protoDir); os.IsNotExist(err) {
		return nil, nil
	}
	dirs, err := protoSubdirsWithFiles(protoDir)
	if err != nil {
		return nil, err
	}
	var findings []createNullabilityFinding
	for _, dir := range dirs {
		scan, err := codegen.ScanRawProtoDir(dir)
		if err != nil {
			// A directory forge's own scanner cannot parse is not this
			// check's problem to report — buf lint and `forge generate`
			// both fail on it far more clearly. Skipping keeps one
			// malformed file from masking findings everywhere else.
			continue
		}
		findings = append(findings, createNullabilityFindingsIn(scan)...)
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].File != findings[j].File {
			return findings[i].File < findings[j].File
		}
		return findings[i].Line < findings[j].Line
	})
	return findings, nil
}

// createNullabilityFindingsIn compares every entity/Create-request pair in
// one scanned directory.
func createNullabilityFindingsIn(scan *codegen.RawProtoScan) []createNullabilityFinding {
	var findings []createNullabilityFinding
	for _, entity := range scan.Messages {
		create, ok := scan.MessageByName("Create" + entity.Name + "Request")
		if !ok {
			continue // not an entity in the CRUD sense
		}
		createFields := make(map[string]codegen.SchemaFieldDef, len(create.Fields))
		for _, f := range create.Fields {
			createFields[f.Name] = f
		}
		for _, ef := range entity.Fields {
			if managedLifecycleFields[ef.Name] {
				continue
			}
			cf, present := createFields[ef.Name]
			if !present {
				// Absent by design (read-only/computed, or an author's
				// deliberate omission). The check never demands a field
				// be carried — only that a carried one agree.
				continue
			}
			// repeated and optional are mutually exclusive in proto3, so
			// a repeated field on either side has nothing to disagree
			// about; comparing them would report every repeated field.
			if ef.Repeated || cf.Repeated {
				continue
			}
			if ef.Optional == cf.Optional {
				continue
			}
			findings = append(findings, createNullabilityFinding{
				File:           create.File,
				Line:           fieldLineIn(create, ef.Name),
				Entity:         entity.Name,
				Field:          ef.Name,
				EntityOptional: ef.Optional,
				CreateOptional: cf.Optional,
			})
		}
	}
	return findings
}

// fieldLineIn locates the 1-indexed line of a field declaration within a
// message's body, so the finding points at the line to edit rather than at
// the file. Falls back to the message's own opening line when the field
// cannot be located textually (the scan parsed it, so it is there — but a
// pathological spelling should degrade to a usable location, never to a
// crash or a zero).
func fieldLineIn(msg codegen.RawProtoMessage, field string) int {
	data, err := os.ReadFile(msg.File)
	if err != nil {
		return 1
	}
	content := string(data)
	declRE := regexp.MustCompile(`(?m)^\s*(?:optional\s+|repeated\s+)?[\w.]+\s+` + regexp.QuoteMeta(field) + `\s*=\s*\d+`)
	for _, loc := range declRE.FindAllStringIndex(content, -1) {
		if loc[0] >= msg.BodyOpen && loc[0] <= msg.BodyClose {
			return strings.Count(content[:loc[0]], "\n") + 1
		}
	}
	return strings.Count(content[:msg.BodyOpen], "\n") + 1
}

// protoSubdirsWithFiles returns every directory under root that directly
// contains at least one .proto file, root included. ScanRawProtoDir walks
// recursively and resolves type names against everything it finds, so
// handing it one directory per proto PACKAGE keeps the resolution scope
// matching the package boundary rather than merging every service's types
// into one namespace.
func protoSubdirsWithFiles(root string) ([]string, error) {
	seen := map[string]bool{}
	if err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".proto") {
			seen[filepath.Dir(path)] = true
		}
		return nil
	}); err != nil {
		return nil, fmt.Errorf("walk %s: %w", root, err)
	}
	dirs := make([]string, 0, len(seen))
	for d := range seen {
		dirs = append(dirs, d)
	}
	sort.Strings(dirs)
	return dirs, nil
}
