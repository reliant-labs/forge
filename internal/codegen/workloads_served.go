package codegen

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/naming"
)

// DeclaredWorkload is what one `<ident> = fw.Workload {...}` literal in
// deploy/kcl/workloads.k RUNS: its name, the cmd of its GoBuild (empty when
// it builds nothing forge compiles), its args, and the components it states
// it serves.
//
// It is read from the source rather than evaluated, because its readers —
// `forge lint` and `forge scaffold` — must work on a checkout that has never
// rendered, and must not need kcl on PATH to do it.
type DeclaredWorkload struct {
	Name     string
	BuildCmd string
	Args     []string
	// Serves lists the components this workload's process runs besides one
	// named for itself: the `serves` field, for a subcommand that mounts
	// several components (control-plane's `admin-api`).
	Serves []string
}

var (
	// workloadLiteral matches the opening of a top-level workload binding.
	workloadLiteral = regexp.MustCompile(`(?m)^[A-Za-z_][A-Za-z0-9_]*\s*=\s*fw\.Workload\s*\{`)
	// workloadNameField matches a `name = "<x>"` field on a line of its own.
	workloadNameField = regexp.MustCompile(`(?m)^\s*name\s*=\s*"([^"]+)"\s*$`)
	// goBuildCmd captures the cmd of a GoBuild literal.
	goBuildCmd = regexp.MustCompile(`GoBuild\s*\{[^}]*\bcmd\s*=\s*"([^"]+)"`)
	// buildRef captures a build bound to a variable: `build = _cp_build`.
	buildRef = regexp.MustCompile(`(?m)^\s*build\s*=\s*([A-Za-z_][A-Za-z0-9_]*)\s*$`)
	// kclString matches one double-quoted KCL string.
	kclString = regexp.MustCompile(`"((?:[^"\\]|\\.)*)"`)
)

// DeclaredWorkloads parses each workload literal in a workloads.k, keyed by
// its declared name.
//
// Docstrings and comments are stripped first (StripKCLProse), because the
// scaffolded workloads.k DOCUMENTS the shape it expects, including worked
// examples, and an example is not a declaration. A literal this cannot read
// is simply absent.
//
// A build bound to a top-level variable (`build = _cp_build`, where
// `_cp_build = forge.GoBuild {...}`) is resolved, because stating the one
// project build once and naming it from every workload is how a project
// with several workloads on one binary writes it.
func DeclaredWorkloads(src string) map[string]DeclaredWorkload {
	src = StripKCLProse(src)
	out := map[string]DeclaredWorkload{}
	for _, loc := range workloadLiteral.FindAllStringIndex(src, -1) {
		body, ok := kclBlockBody(src, loc[1])
		if !ok {
			continue
		}
		m := workloadNameField.FindStringSubmatch(body)
		if m == nil {
			continue
		}
		w := DeclaredWorkload{
			Name:   m[1],
			Args:   kclStringListField(body, "args"),
			Serves: kclStringListField(body, "serves"),
		}
		if b := goBuildCmd.FindStringSubmatch(body); b != nil {
			w.BuildCmd = b[1]
		} else if ref := buildRef.FindStringSubmatch(body); ref != nil {
			bound := regexp.MustCompile(`(?m)^` + regexp.QuoteMeta(ref[1]) + `\s*=\s*forge\.GoBuild\s*\{[^}]*\bcmd\s*=\s*"([^"]+)"`)
			if b := bound.FindStringSubmatch(src); b != nil {
				w.BuildCmd = b[1]
			}
		}
		out[w.Name] = w
	}
	return out
}

// kclStringListField returns the strings of a `<field> = [...]` list in a
// literal body, or nil when the field is absent. The list is delimited by
// scanKCLList, not a pattern, so a bracket inside a string cannot end it.
func kclStringListField(body, field string) []string {
	head := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(field) + `\s*=\s*\[`).FindStringIndex(body)
	if head == nil {
		return nil
	}
	list, ok := scanKCLList(body, head[1]-1)
	if !ok {
		return nil
	}
	// Non-nil even when empty: `serves = []` is still a statement.
	out := []string{}
	for _, s := range kclString.FindAllStringSubmatch(list.code, -1) {
		out = append(out, s[1])
	}
	return out
}

// kclBlockBody returns the text between the `{` that ends at open and its
// matching `}`, skipping braces inside string literals.
func kclBlockBody(src string, open int) (string, bool) {
	depth := 1
	inString := false
	for i := open; i < len(src); i++ {
		c := src[i]
		switch {
		case inString:
			if c == '\\' {
				i++
			} else if c == '"' {
				inString = false
			}
		case c == '"':
			inString = true
		case c == '{':
			depth++
		case c == '}':
			depth--
			if depth == 0 {
				return src[open:i], true
			}
		}
	}
	return "", false
}

// ServingWorkload names the declared workload whose process ALREADY runs
// component c, when that workload is not c's own declaration. Two cases, and
// both are facts forge can read rather than guess:
//
//   - A workload that lists c in `serves`. A subcommand that mounts several
//     components (control-plane's `admin-api` mounts six admin services) is
//     ONE workload; `serves` is how it says which components it runs. forge
//     cannot see that from Go — the mount set lives in the project's own
//     code — so the declaration states it.
//   - A workload running the project binary's `server` command, which runs
//     every service, worker and operator in one process (cmd-tree-server's
//     MountAll + AllWorkers + AllOperators). A secondary binary is its own
//     program and is never served by it.
//
// Such a component needs no workload of its own: declaring one would deploy
// it a second time, as its own Deployment, beside the process that already
// serves it.
func ServingWorkload(declared map[string]DeclaredWorkload, projectName string, c config.ComponentConfig) (string, bool) {
	names := make([]string, 0, len(declared))
	for name := range declared {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if declared[name].ServesComponent(c.Name) {
			return name, true
		}
	}
	if c.EffectiveKind() == config.ComponentKindBinary {
		return "", false
	}
	for _, name := range names {
		w := declared[name]
		if ProjectBinaryRuns(projectName, w.BuildCmd) && len(w.Args) > 0 && w.Args[0] == "server" {
			return w.Name, true
		}
	}
	return "", false
}

// DeclaresServes reports whether any workload in the file declares `serves`
// — the project's own statement that it runs several components through one
// subcommand. In such a project a new component may belong to one of those
// processes, which forge cannot see, so `forge scaffold` shows the choice
// instead of declaring a workload the project most likely does not want.
func DeclaresServes(declared map[string]DeclaredWorkload) bool {
	for _, w := range declared {
		if w.Serves != nil {
			return true
		}
	}
	return false
}

// ServesComponent reports whether w lists the component named name in its
// `serves`. Names compare by KCL identifier, so `overview-admin` and
// `overview_admin` are the same component, as they are everywhere else in
// this file.
func (w DeclaredWorkload) ServesComponent(name string) bool {
	ident := naming.KCLIdentifier(name)
	for _, s := range w.Serves {
		if naming.KCLIdentifier(s) == ident {
			return true
		}
	}
	return false
}

// ServesHint is the one sentence `forge lint` and `forge scaffold` both use to
// say how a component served by another workload's process is marked as such.
func ServesHint(componentName string) string {
	return fmt.Sprintf("If a workload that already runs serves it (a subcommand that mounts several components), "+
		"add %q to that workload's `serves` list in %s instead of declaring a new workload.",
		componentName, WorkloadsKCLRelPath)
}

// ServesChoiceHint is what `forge scaffold` prints, in place of writing,
// when the project declares `serves` and the new component is in none.
func ServesChoiceHint(modulePath, projectName string, c config.ComponentConfig) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s groups components into shared processes (`serves`), so forge cannot tell\n", WorkloadsKCLRelPath)
	fmt.Fprintf(&b, "   which workload, if any, should run %s %q. Either:\n\n", c.EffectiveKind(), c.Name)
	fmt.Fprintf(&b, "     - add %q to the `serves` list of the workload whose subcommand mounts it, or\n", c.Name)
	b.WriteString("     - give it a workload of its own: add this, and its name to ALL:\n\n")
	b.WriteString(WorkloadStanza(modulePath, projectName, c))
	return b.String()
}
