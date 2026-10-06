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

// AppendWorkloadStanza adds one workload to a project's
// deploy/kcl/workloads.k — its declaration, and its name in the `ALL` list —
// and reports whether it did.
//
// ADDITIVE ONLY. Existing content is never rewritten, reformatted or
// reordered: the file is user-owned, so the only edits forge makes to it are
// the new declaration (above the ALL list's comment block) and the one new
// list entry, written in the style the list already uses. Every other byte,
// comment and line break stays where the user left it.
//
// It returns applied=false, with no error and no write, when the file is
// missing, the workload is already declared, or the ALL list cannot be
// located unambiguously. The caller then PRINTS the stanza for the user to
// paste.
func AppendWorkloadStanza(projectDir, modulePath, projectName string, c config.ComponentConfig) (applied bool, err error) {
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

	updated, ok := addWorkloadDeclaration(content, naming.KCLIdentifier(c.Name), WorkloadStanza(modulePath, projectName, c))
	if !ok {
		return false, nil
	}
	if err := os.WriteFile(path, []byte(updated), 0644); err != nil {
		return false, err
	}
	return true, nil
}

// addWorkloadDeclaration makes the two edits a new workload needs: its
// declaration, and its identifier in the `ALL` list.
//
// BOTH are required: a declaration nothing references is dead code, and a
// workload missing from `ALL` would scaffold cleanly and then silently never
// deploy. If `ALL` cannot be located unambiguously — no list, two of them, or
// one that is never closed — NEITHER edit is made (ok=false) rather than half
// of them: the caller prints the stanza and the user places it, which is the
// contract this file has always had for content forge cannot safely edit.
//
// The list edit touches only the new entry (appendKCLListElement), and is
// skipped when `ALL` already names the identifier, so a workload listed ahead
// of its declaration is not listed twice.
func addWorkloadDeclaration(content, ident, stanza string) (string, bool) {
	heads := allListHead.FindAllStringIndex(content, -1)
	if len(heads) != 1 {
		return content, false
	}
	allStart, open := heads[0][0], heads[0][1]-1
	list, ok := scanKCLList(content, open)
	if !ok {
		return content, false
	}
	updated := content
	if !kclListHasEntry(list, ident) {
		updated, _ = appendKCLListElement(content, open, ident)
	}

	// The declaration goes ABOVE the comment block introducing the ALL list,
	// so `ALL` stays the last thing in the file — it reads as the summary of
	// everything above it, which is the only reason to put a list of names at
	// the bottom of a file of declarations. Walk back over the contiguous `#`
	// lines that document it, so the declaration lands above the prose rather
	// than wedged between it and the list. The list edit above only changed
	// bytes after `open`, so allStart still marks the ALL line.
	at := allStart
	for at > 0 {
		prev := lineStart(updated, at-1)
		if !strings.HasPrefix(strings.TrimSpace(updated[prev:at]), "#") {
			break
		}
		at = prev
	}
	return updated[:at] + stanza + "\n" + updated[at:], true
}

// allListHead matches the head of the `ALL: [fw.Workload] = [` aggregation
// list, up to and including its opening bracket. Anchored on the typed
// declaration at the start of a line rather than a bare `ALL`, so the word
// appearing in prose or a comment cannot be mistaken for it. The list itself
// is read by scanKCLList, not by this pattern: a list is not a regular
// language once comments may hold brackets.
var allListHead = regexp.MustCompile(`(?m)^ALL\s*:\s*\[\s*fw\.Workload\s*\]\s*=\s*\[`)

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
func WorkloadStanzaHint(modulePath, projectName string, c config.ComponentConfig) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Add this workload to %s:\n\n", WorkloadsKCLRelPath)
	b.WriteString(WorkloadStanza(modulePath, projectName, c))
	return b.String()
}
