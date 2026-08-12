// File: internal/cli/lint/lint_vendored_protos.go
//
// vendored-protos — `forge lint --vendored-protos`.
//
// forge COPIES two proto files into every project at scaffold time and
// then never looks at them again:
//
//	proto/forge/v1/forge.proto      — the annotation definitions every
//	                                  project's protos `import`
//	proto/buf/validate/validate.proto — protovalidate's field rules,
//	                                  auto-vendored on the first run that
//	                                  imports buf.validate
//
// Neither is a template (nothing renders them per-project), neither is
// Tier-1 (no self-certifying `forge:hash` header), and neither appears in
// .forge/hashes.json. So forge's whole upgrade path is BLIND to them: a
// project scaffolded on an old forge keeps that vintage's copy forever,
// and no command in forge notices or offers to update it.
//
// WHAT THAT COSTS, from the real case this check exists to prevent: a
// project's vendored forge.proto silently diverged for months, carrying
// method-option fields (required_roles / authz_public / authz_custom /
// default_roles) on field numbers upstream had since RESERVED. The
// project's own protos annotated RPCs with them, `buf` compiled the
// annotations without complaint because the LOCAL forge.proto still
// declared the fields — and forge, reading its own compiled descriptor,
// found nothing there. Every one of those authz declarations was inert.
// 104 annotations across 14 service protos had to be found and fixed by
// hand. Nothing anywhere had reported a problem.
//
// The check compares each vendored file on disk against the copy embedded
// in the RUNNING forge binary (internal/assets) and reports the specific
// difference.
//
// ── Severity ──────────────────────────────────────────────────────────
//
// forge.proto is an ERROR. A stale copy is not a style question: it is
// how an annotation gets written, compiled and read by nothing, and the
// field-number case is a genuine correctness bug (the project's number
// means one thing locally and is RESERVED upstream — the two can never
// agree). The fix is mechanical (`forge project upgrade`), which is what
// makes an error fair rather than merely loud.
//
// validate.proto is a WARNING. forge does not read its contents — it is a
// buf COMPILE input, and the runtime library supplies the extension types
// — so a stale copy costs you newer field rules, it does not silently
// misread what you wrote. Same class, lower stakes, so it reports without
// gating.
//
// ── False positives this deliberately avoids ──────────────────────────
//
//   - THE go_package REWRITE. Scaffold does not copy forge.proto
//     verbatim: assets.WriteForgeV1Proto rewrites its `option go_package`
//     to point at forge/pkg/forgepb. Comparing raw bytes would therefore
//     flag EVERY project in existence, forever, on a line forge itself
//     changed on purpose. Both sides are normalized (normalizeVendored)
//     before comparison.
//   - A file that is ABSENT is not drift. Plenty of projects never import
//     buf.validate, and forge vendors validate.proto lazily.
//   - No proto tree at all (CLI / library projects) yields nothing.
//   - DELIBERATE CUSTOMIZATION has the escape hatch forge already uses for
//     exactly this — `forge project disown <path>` — and a disowned
//     vendored file is skipped entirely. Ownership transfer is forge's
//     established one-way door for "I know, it's mine now", so this check
//     reuses it rather than inventing a second opt-out vocabulary.
//   - Line-ending and trailing-whitespace differences are normalized away;
//     a CRLF checkout is not a divergence.

package lint

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/assets"
	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/cliutil"
)

// Severities a vendored-proto finding can carry. Spelled as the JSON
// contract spells them so the mapper needs no translation table.
const (
	vendoredSeverityError   = "error"
	vendoredSeverityWarning = "warning"
)

// vendoredDiffMaxLines bounds the rendered difference. A project that
// reformatted the whole file should get a usable report, not the file
// itself echoed back at them.
const vendoredDiffMaxLines = 12

// vendoredProtoFinding is one vendored file whose on-disk content differs
// from the copy embedded in the running forge binary. File is
// project-relative.
type vendoredProtoFinding struct {
	File     string
	Severity string
	// Diff is the bounded, human-readable difference: `-` lines are
	// forge's (expected), `+` lines are the project's. This is what makes
	// the finding actionable rather than a bare "differs" — the real case
	// needed to know WHICH option fields were missing.
	Diff string
}

// vendoredProtoSpec describes one file forge copies rather than renders.
//
// Keeping the set as data — and deriving `Want` from the same
// internal/assets accessors the SCAFFOLD path writes from — is what stops
// this check from drifting out of date the way the vendored files
// themselves did. A third vendored file added to assets that is not
// listed here is a gap, so the package test pins the list against the
// embedded FS.
type vendoredProtoSpec struct {
	// RelPath is the project-relative slash path of the vendored copy.
	RelPath string
	// Want returns the embedded bytes, normalized for comparison.
	Want func() ([]byte, error)
	// Severity is the finding severity for drift in this file.
	Severity string
}

// vendoredProtoSpecs is the complete set of files forge COPIES into a
// project (as opposed to rendering from a template). It mirrors the
// embed directives in internal/assets/embedded.go.
func vendoredProtoSpecs() []vendoredProtoSpec {
	return []vendoredProtoSpec{
		{
			RelPath:  assets.ForgeProtoVendorRelPath,
			Want:     assets.GetForgeV1Proto,
			Severity: vendoredSeverityError,
		},
		{
			RelPath:  assets.ValidateProtoVendorRelPath,
			Want:     assets.GetValidateProto,
			Severity: vendoredSeverityWarning,
		},
	}
}

// vendoredProtoFixHint renders the remediation. It is a runbook: the
// literal command that re-copies the file, what to do when the project
// meant to customize it, and — for forge.proto — why a stale copy is a
// correctness problem rather than cosmetic drift.
func vendoredProtoFixHint(f vendoredProtoFinding) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s is a COPY of forge's own file, not a template — forge's upgrade path does not track it, "+
		"so this divergence is invisible to every other command. ", f.File)
	if f.Severity == vendoredSeverityError {
		b.WriteString("An annotation you write against a field this copy declares but forge has RESERVED compiles " +
			"under buf and is read by NOTHING. ")
	}
	fmt.Fprintf(&b, "Fix: run `forge project upgrade` to re-copy it from this forge binary "+
		"(or `rm %s && forge generate`). ", f.File)
	fmt.Fprintf(&b, "If this project customized the file ON PURPOSE, take ownership so forge stops reporting it: "+
		"`forge project disown %s --reason \"<why>\"`.", f.File)
	return b.String()
}

// runVendoredProtosLint is the text-mode entry point.
func runVendoredProtosLint(projectDir string) error {
	fmt.Println("Running vendored-protos lint...")
	findings, err := collectVendoredProtoFindings(projectDir)
	if err != nil {
		return err
	}
	formatVendoredProtos(os.Stdout, findings)
	if !vendoredProtosGate(findings) {
		return nil
	}
	return cliutil.UserErr("forge lint --vendored-protos",
		fmt.Sprintf("%d vendored proto(s) drifted from the copy embedded in this forge binary", len(findings)),
		"",
		"run `forge project upgrade` to re-copy them, or `forge project disown <path> --reason \"<why>\"` to keep a deliberate customization")
}

// vendoredProtosGate reports whether any finding is severity error — the
// condition under which text mode exits non-zero.
func vendoredProtosGate(findings []vendoredProtoFinding) bool {
	for _, f := range findings {
		if f.Severity == vendoredSeverityError {
			return true
		}
	}
	return false
}

// formatVendoredProtos writes the human report.
func formatVendoredProtos(w io.Writer, findings []vendoredProtoFinding) {
	if len(findings) == 0 {
		_, _ = fmt.Fprintln(w, "  vendored-protos clean — every vendored proto matches the copy embedded in this forge binary")
		return
	}
	for _, f := range findings {
		icon := "⚠"
		if f.Severity == vendoredSeverityError {
			icon = "✗"
		}
		_, _ = fmt.Fprintf(w, "  %s [forge-vendored-proto-drift] %s\n", icon, f.File)
		_, _ = fmt.Fprintf(w, "      differs from the copy embedded in this forge binary:\n")
		for _, line := range strings.Split(strings.TrimRight(f.Diff, "\n"), "\n") {
			_, _ = fmt.Fprintf(w, "        %s\n", line)
		}
		_, _ = fmt.Fprintf(w, "      → %s\n", vendoredProtoFixHint(f))
	}
	_, _ = fmt.Fprintf(w, "\n%d vendored proto(s) drifted from this forge binary.\n", len(findings))
	if !vendoredProtosGate(findings) {
		_, _ = fmt.Fprintln(w, "(warnings only — not failing the build)")
	}
}

// collectVendoredProtoFindings is the shared engine behind text mode and
// `forge lint --json`. Findings come back in vendoredProtoSpecs order, so
// output is deterministic.
//
// A missing file, a missing proto tree, and a disowned path all yield no
// finding — see the file header for why each is deliberate.
func collectVendoredProtoFindings(projectDir string) ([]vendoredProtoFinding, error) {
	cs, err := checksums.Load(projectDir)
	if err != nil {
		// Ownership state that cannot be read must not silently DISABLE
		// the check (that would reintroduce the silence), nor fail the
		// lint on an unrelated problem. Treat it as "nothing disowned"
		// and let the drift report stand.
		cs = nil
	}

	var findings []vendoredProtoFinding
	for _, spec := range vendoredProtoSpecs() {
		if cs.IsDisowned(spec.RelPath) {
			continue
		}
		path := filepath.Join(projectDir, filepath.FromSlash(spec.RelPath))
		got, readErr := os.ReadFile(path)
		if readErr != nil {
			if os.IsNotExist(readErr) {
				continue // not vendored here; not drift
			}
			return nil, fmt.Errorf("read %s: %w", spec.RelPath, readErr)
		}
		want, wantErr := spec.Want()
		if wantErr != nil {
			return nil, fmt.Errorf("read embedded %s: %w", spec.RelPath, wantErr)
		}

		wantNorm := normalizeVendored(string(want))
		gotNorm := normalizeVendored(string(got))
		if wantNorm == gotNorm {
			continue
		}
		findings = append(findings, vendoredProtoFinding{
			File:     filepath.FromSlash(spec.RelPath),
			Severity: spec.Severity,
			Diff:     vendoredProtoDiff(wantNorm, gotNorm),
		})
	}
	return findings, nil
}

// normalizeVendored canonicalizes a vendored proto for comparison.
//
// The go_package rewrite is the load-bearing part: scaffold REWRITES that
// one line (assets.WriteForgeV1Proto) to point at forge/pkg/forgepb, so
// it differs by design in every project and comparing it would make this
// check fire universally. It is dropped from both sides rather than
// pattern-matched to a specific value, because the value forge writes has
// itself changed across versions and the check must not encode a vintage.
//
// CRLF and trailing whitespace are normalized for the same reason: a
// checkout artifact is not a divergence in what the file DECLARES.
func normalizeVendored(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimRight(line, " \t")
		if strings.HasPrefix(strings.TrimSpace(trimmed), "option go_package") {
			continue
		}
		out = append(out, trimmed)
	}
	// Trailing blank lines carry no meaning in proto.
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

// vendoredProtoDiff renders a bounded line difference between forge's
// copy (want) and the project's (got).
//
// This is a SET difference over lines, not a positional diff: the useful
// question for a vendored annotation file is "which declarations does one
// side have that the other doesn't" — a missing `frontend_config = 50202`
// or a stale `authz_public = 7` — and a set difference answers exactly
// that without a positional algorithm's noise, where inserting one line
// near the top re-aligns everything after it.
func vendoredProtoDiff(want, got string) string {
	gotSet := map[string]int{}
	for _, l := range strings.Split(got, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			gotSet[t]++
		}
	}
	wantSet := map[string]int{}
	for _, l := range strings.Split(want, "\n") {
		if t := strings.TrimSpace(l); t != "" {
			wantSet[t]++
		}
	}

	var missing, extra []string
	for _, l := range strings.Split(want, "\n") {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		if gotSet[t] > 0 {
			gotSet[t]--
			continue
		}
		missing = append(missing, "- "+t)
	}
	for _, l := range strings.Split(got, "\n") {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		if wantSet[t] > 0 {
			wantSet[t]--
			continue
		}
		extra = append(extra, "+ "+t)
	}

	var b strings.Builder
	b.WriteString("(- forge's copy, + this project's)\n")
	shown, elided := 0, 0
	for _, group := range [][]string{missing, extra} {
		for _, line := range group {
			if shown >= vendoredDiffMaxLines {
				elided++
				continue
			}
			b.WriteString(line)
			b.WriteByte('\n')
			shown++
		}
	}
	if elided > 0 {
		fmt.Fprintf(&b, "… and %d more differing line(s)\n", elided)
	}
	return b.String()
}
