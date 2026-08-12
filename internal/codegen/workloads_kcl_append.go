package codegen

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/naming"
)

// AppendWorkloadStanza adds one workload declaration to the END of a
// project's deploy/kcl/workloads.k and reports whether it succeeded.
//
// APPEND-ONLY. Existing content is never rewritten, reformatted or reordered:
// the file is user-owned, so the only edit forge makes to it is adding a new
// declaration after everything already there.
//
// The anchor is END OF FILE, and that is a direct consequence of the
// declaration shape. Workloads are NAMED top-level bindings
// (`billing = fw.Workload {...}`), not entries in one list, so there is no
// closing bracket to insert before and no structure to parse: a new
// declaration is simply valid KCL appended after the last one. That is
// strictly safer than the list form it replaced, which had to locate exactly
// one `]` and gave up whenever a user's edits made that ambiguous.
//
// It returns applied=false, with no error and no write, when the file is
// missing or the workload is already declared. The caller then PRINTS the
// stanza for the user to paste.
func AppendWorkloadStanza(projectDir, projectName string, c config.ComponentConfig) (applied bool, err error) {
	path := filepath.Join(projectDir, WorkloadsKCLRelPath)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	content := string(raw)

	// Already declared — by an earlier run, or by hand. Appending a second
	// declaration would shadow the first (KCL takes the last binding) and
	// silently change which workload deploys.
	if workloadDeclaredIn(content, c.Name) {
		return false, nil
	}

	ident := naming.KCLIdentifier(c.Name)

	// The declaration itself goes after everything already in the file, then
	// the `ALL` list is re-emitted with the new name appended.
	//
	// BOTH edits are required, and the second is why this is not a pure
	// append: a declaration nothing references is dead code, and a workload
	// missing from `ALL` would scaffold cleanly and then silently never
	// deploy. If `ALL` cannot be located unambiguously the whole write is
	// abandoned (applied=false) rather than half-applied — the caller prints
	// the stanza and the user places it, which is the same contract the file
	// has always had for content it cannot safely edit.
	loc := allListLine.FindStringSubmatchIndex(content)
	if loc == nil {
		return false, nil
	}
	existing := strings.TrimSpace(content[loc[2]:loc[3]])
	members := ident
	if existing != "" {
		members = existing + ", " + ident
	}
	withAll := content[:loc[2]] + members + content[loc[3]:]

	// Insert the declaration BEFORE the comment block introducing the ALL
	// list, so `ALL` stays the last thing in the file — it reads as the
	// summary of everything above it, which is the only reason to put a list
	// of names at the bottom of a file of declarations.
	//
	// The anchor is the start of the ALL line's own comment block: walk back
	// over the contiguous `#` lines that document it, so the declaration
	// lands above the prose rather than wedged between it and the list.
	lines := strings.Split(withAll, "\n")
	allAt := -1
	for i, ln := range lines {
		if strings.HasPrefix(ln, "ALL:") || strings.HasPrefix(ln, "ALL ") {
			allAt = i
			break
		}
	}
	if allAt < 0 {
		return false, nil
	}
	insertAt := allAt
	for insertAt > 0 && strings.HasPrefix(strings.TrimSpace(lines[insertAt-1]), "#") {
		insertAt--
	}

	stanza := strings.TrimRight(WorkloadStanza(projectName, c), "\n")
	out := append([]string{}, lines[:insertAt]...)
	out = append(out, strings.Split(stanza, "\n")...)
	out = append(out, "")
	out = append(out, lines[insertAt:]...)
	updated := strings.Join(out, "\n")

	if err := os.WriteFile(path, []byte(updated), 0644); err != nil {
		return false, err
	}
	return true, nil
}

// allListLine matches the `ALL: [fw.Workload] = [...]` aggregation list,
// capturing its contents so a new name can be appended. Anchored on the
// typed declaration rather than a bare `ALL` so the word appearing in prose
// or a comment cannot be mistaken for it.
var allListLine = regexp.MustCompile(`(?m)^ALL\s*:\s*\[fw\.Workload\]\s*=\s*\[([^\]]*)\]`)

// workloadDeclaredIn reports whether the KCL source already declares a
// workload called name. It matches BOTH the binding (`billing = fw.Workload`)
// and the `name = "billing"` field, because a user may rename either half:
// the binding is what an env refines, the field is what the manifest is
// called, and a duplicate of either is a real collision.
//
// Docstrings and comments are stripped first: the scaffolded workloads.k
// documents the format with a worked example, and an example is not a
// declaration. Without this, a project whose docs happen to name the
// workload being added would silently skip the append.
func workloadDeclaredIn(content, name string) bool {
	stripped := StripKCLProse(content)
	nameField := regexp.MustCompile(`(?m)^\s*name\s*=\s*"` + regexp.QuoteMeta(name) + `"\s*$`)
	binding := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(naming.KCLIdentifier(name)) + `\s*=`)
	return nameField.MatchString(stripped) || binding.MatchString(stripped)
}

// WorkloadDeclared reports whether workloads.k content declares a workload
// by that name, ignoring prose that merely mentions it.
//
// Exported for callers that need to ask what a project DECLARES rather than
// what it has on disk in some other format — the dev-identity gate asks
// whether `idp-provision` is declared instead of parsing docker-compose.yml
// for an `idp` service (see internal/cli/devidp_gate.go).
func WorkloadDeclared(content, name string) bool {
	return workloadDeclaredIn(content, name)
}

// StripKCLProse removes """docstrings""" and # comments from KCL source, so a
// scan for real declarations is not fooled by prose that illustrates them.
func StripKCLProse(src string) string {
	return kclComment.ReplaceAllString(kclDocstring.ReplaceAllString(src, ""), "")
}

var (
	kclDocstring = regexp.MustCompile(`(?s)""".*?"""`)
	kclComment   = regexp.MustCompile(`(?m)#.*$`)
)

// WorkloadStanzaHint is the message shown when the stanza could not be
// appended automatically: the exact text to paste, and where.
func WorkloadStanzaHint(projectName string, c config.ComponentConfig) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Add this workload to %s:\n\n", WorkloadsKCLRelPath)
	b.WriteString(WorkloadStanza(projectName, c))
	return b.String()
}
