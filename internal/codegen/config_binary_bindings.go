package codegen

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Binding rules for (forge.v1.binary_config).
//
// A binary-bound config message is the whole config surface of one process:
// forge generates its loader, its KCL schema and its env projection, and the
// process's Deployment carries exactly those env vars. That only means
// something when the binding names a process that exists. A binding to a
// binary nothing builds still generates all of it, cleanly, and nothing ever
// reads any of it — so a typo, a renamed cmd/ directory, or a subcommand line
// written where a binary name belongs (`binary: "x server api"`) used to
// generate green and leave a config that no process loads.
//
// forge therefore refuses the binding at GENERATE time, before any artifact
// is written: the same rule (forge.v1.frontend_config) enforces for a
// frontend forge.yaml does not declare. A lint warning would arrive after the
// projection had already been wired into an environment.
//
// The `command` half is validated for SHAPE only. Whether a cobra command
// path exists is not knowable from the source without guessing (see
// BinaryConfigOptions.command in forge.proto); the generated loader checks it
// exactly, at runtime, against the real tree.

// bindingProblemKind classifies a refused binding, so the runbook's footer
// says only what applies to the problems actually found.
type bindingProblemKind int

const (
	bindingUnknownBinary bindingProblemKind = iota
	bindingBadCommand
	bindingDuplicate
	bindingProcessCollision
)

// BinaryConfigBindingProblem is one refused binding.
type BinaryConfigBindingProblem struct {
	// Message is the config message carrying the annotation.
	Message string
	// Where is "proto/<file>:<line>", or "" when the descriptor carries no
	// source position.
	Where string
	// Binding renders the annotation as written, e.g.
	// `{binary: "reliant", command: ["server", "api"]}`.
	Binding string
	// Detail explains this one offender: what is wrong and, where forge can
	// tell, what was probably meant.
	Detail string

	kind  bindingProblemKind
	order int // the message's declaration index
}

// ProjectBinaries lists the binaries a (forge.v1.binary_config).binary may
// name: the directory leaves under projectDir/cmd.
//
// A project with no cmd/ directory has NO binaries (an empty, non-nil
// slice), so a binding there is refused. nil means UNKNOWN — cmd/ exists but
// could not be read — and the existence check is then skipped rather than
// reporting every binding as dangling: "forge could not tell" must never
// render as "it does not exist".
func ProjectBinaries(projectDir string) []string {
	entries, err := os.ReadDir(filepath.Join(projectDir, "cmd"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []string{}
		}
		return nil
	}
	out := []string{}
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// ProcessName renders the process a binding names, as it is typed at a
// shell: "reliant" for a root config, "reliant server api" for a command
// config. Empty for a message bound to no binary.
func (m ConfigMessage) ProcessName() string {
	if m.Binary == "" {
		return ""
	}
	return strings.TrimSpace(m.Binary + " " + strings.Join(m.Command, " "))
}

// ValidateBinaryConfigBindings refuses every (forge.v1.binary_config)
// binding that cannot mean what it says:
//
//   - binary names no cmd/<name> directory. binaries is the set
//     ProjectBinaries returned; nil skips this check (see ProjectBinaries).
//   - command is set without a binary, or an element is not ONE command
//     name (empty, contains whitespace, or is a flag).
//   - two messages bind the same (binary, command). A process has ONE
//     config at each level — one Load<Msg>, one KCL schema its env is
//     projected from — so a second message for it has no defined meaning,
//     and the projection silently used whichever came first.
//   - a binary's root config and one of its command configs declare the same
//     env var or flag. The process running that command loads both, so it
//     would read one value from two declarations that can drift, and its
//     env projection would carry the key twice.
//
// It reports EVERY offender rather than the first, for the reason
// ValidateFrontendConfigs does: a renamed binary takes several messages with
// it, and fixing them one generate-run at a time is miserable.
func ValidateBinaryConfigBindings(messages []ConfigMessage, binaries []string) error {
	known := map[string]bool{}
	for _, b := range binaries {
		known[b] = true
	}

	var problems []BinaryConfigBindingProblem
	index := map[*ConfigMessage]int{}
	refuse := func(m *ConfigMessage, kind bindingProblemKind, detail string) {
		problems = append(problems, BinaryConfigBindingProblem{
			Message: m.Name,
			Where:   configMessageWhere(*m),
			Binding: bindingLiteral(*m),
			Detail:  detail,
			kind:    kind,
			order:   index[m],
		})
	}

	// Bindings that name a real process, grouped by it. A binding already
	// refused for its own shape names no process, so it joins no group:
	// reporting it again as a duplicate of something it only resembles
	// would be noise.
	byProcess := map[string][]*ConfigMessage{}
	var order []string
	for i := range messages {
		m := &messages[i]
		index[m] = i
		if m.Binary == "" {
			if len(m.Command) > 0 {
				refuse(m, bindingBadCommand,
					"`command` names a subcommand path BELOW a binary, and no `binary` is set. Add the binary whose subcommand this is.")
			}
			continue
		}
		valid := true
		if detail := commandShapeProblem(m.Command); detail != "" {
			refuse(m, bindingBadCommand, detail)
			valid = false
		}
		if binaries != nil && !known[m.Binary] {
			refuse(m, bindingUnknownBinary, unknownBinaryDetail(m.Binary, binaries))
			valid = false
		}
		if !valid {
			continue
		}
		key := processKey(m.Binary, m.Command)
		if _, seen := byProcess[key]; !seen {
			order = append(order, key)
		}
		byProcess[key] = append(byProcess[key], m)
	}

	for _, key := range order {
		group := byProcess[key]
		for _, m := range group[1:] {
			what := fmt.Sprintf("binary %q's root config", m.Binary)
			if len(m.Command) > 0 {
				what = fmt.Sprintf("%#q", m.ProcessName())
			}
			refuse(m, bindingDuplicate, fmt.Sprintf(
				"%s is already bound by %s. A process has ONE config at each level (one Load<Msg>, one KCL schema, one env projection), so a second message for it has no defined meaning. Merge the two messages, or compose the fields they share as a block on one of them.",
				what, group[0].Name))
		}
	}

	for _, c := range processCollisions(messages, byProcess) {
		refuse(c.cmd, bindingProcessCollision, c.detail())
	}

	if len(problems) == 0 {
		return nil
	}
	// In declaration order, so the runbook reads top to bottom like the
	// proto it points into.
	sort.SliceStable(problems, func(i, j int) bool { return problems[i].order < problems[j].order })
	return fmt.Errorf("%s", formatBinaryConfigBindingProblems(problems, binaries))
}

// processKey identifies the process a binding names. It is NOT the
// space-joined ProcessName: `{binary: "x server"}` and `{binary: "x",
// command: ["server"]}` print alike but are different bindings.
func processKey(binary string, command []string) string {
	return binary + "\x00" + strings.Join(command, "\x00")
}

// commandShapeProblem explains why a command path is not one, or "" when
// every element is a single command name. Element by element, because the
// common mistake — `command: ["server api"]` — is a whole line in one slot.
func commandShapeProblem(command []string) string {
	for _, c := range command {
		switch {
		case strings.TrimSpace(c) == "":
			return "`command` has an empty element. Each element is one cobra command name: [\"server\", \"api\"]."
		case strings.ContainsAny(c, " \t\n"):
			return fmt.Sprintf("`command` element %q contains whitespace. Each element is ONE cobra command name: %s.",
				c, quotedList(strings.Fields(c)))
		case strings.HasPrefix(c, "-"):
			return fmt.Sprintf("`command` element %q is a flag. `command` is the subcommand PATH a process runs; flags are config fields.", c)
		}
	}
	return ""
}

// processCollision is one leaf a binary's root config and one of its command
// configs both declare.
type processCollision struct {
	root, cmd *ConfigMessage
	what      string // "env var LOG_LEVEL" / "flag --log-level"
	rootPath  string // the leaf on the root config, e.g. "log.level"
	cmdPath   string
}

func (c processCollision) detail() string {
	return fmt.Sprintf(
		"%s (%s) is also declared by %s (%s), the binary's root config. The %#q process loads both, so it would read one value from two declarations that can drift, and its env projection would carry the key twice. Declare it once: on %s if every %s process reads it, otherwise on the command configs alone.",
		c.what, c.cmdPath, c.root.Name, c.rootPath, c.cmd.ProcessName(), c.root.Name, c.root.Binary)
}

// processCollisions finds the leaves a binary's root config shares with one
// of its command configs. Composed blocks count: they are leaves of the
// process exactly as declared ones are. Only the FIRST message per process
// is considered on either side — a second is already refused as a duplicate.
func processCollisions(messages []ConfigMessage, byProcess map[string][]*ConfigMessage) []processCollision {
	var out []processCollision
	for i := range messages {
		cmd := &messages[i]
		if cmd.Binary == "" || len(cmd.Command) == 0 {
			continue
		}
		if group := byProcess[processKey(cmd.Binary, cmd.Command)]; len(group) == 0 || group[0] != cmd {
			continue // refused for its shape or binary, or a duplicate
		}
		roots := byProcess[processKey(cmd.Binary, nil)]
		if len(roots) == 0 {
			continue
		}
		root := roots[0]

		rootEnv, rootFlag := map[string]string{}, map[string]string{}
		for _, f := range FlattenFieldsWithBlocks(root.Fields, messages) {
			if f.EnvVar != "" {
				rootEnv[f.EnvVar] = f.KCLPath()
			}
			if f.Flag != "" && !f.Sensitive {
				rootFlag[f.Flag] = f.KCLPath()
			}
		}
		for _, f := range FlattenFieldsWithBlocks(cmd.Fields, messages) {
			if p, dup := rootEnv[f.EnvVar]; dup && f.EnvVar != "" {
				out = append(out, processCollision{root: root, cmd: cmd,
					what: "env var " + f.EnvVar, rootPath: p, cmdPath: f.KCLPath()})
				continue
			}
			if p, dup := rootFlag[f.Flag]; dup && f.Flag != "" && !f.Sensitive {
				out = append(out, processCollision{root: root, cmd: cmd,
					what: "flag --" + f.Flag, rootPath: p, cmdPath: f.KCLPath()})
			}
		}
	}
	return out
}

// bindingLiteral renders a message's binding as the annotation spells it.
func bindingLiteral(m ConfigMessage) string {
	parts := []string{}
	if m.Binary != "" {
		parts = append(parts, "binary: "+strconv.Quote(m.Binary))
	}
	if len(m.Command) > 0 {
		parts = append(parts, "command: "+quotedList(m.Command))
	}
	return "{" + strings.Join(parts, ", ") + "}"
}

// quotedList renders a string list as a proto/KCL list literal.
func quotedList(items []string) string {
	q := make([]string, len(items))
	for i, s := range items {
		q[i] = strconv.Quote(s)
	}
	return "[" + strings.Join(q, ", ") + "]"
}

// configMessageWhere renders a message's source position as the
// project-relative "proto/<file>:<line>" an editor can jump to. The
// descriptor records files relative to the buf module root (proto/), and
// config protos are always read from proto/config. It degrades rather than
// lying: no file renders "", no line renders the file alone.
func configMessageWhere(m ConfigMessage) string {
	if m.ProtoFile == "" {
		return ""
	}
	file := filepath.ToSlash(m.ProtoFile)
	if !strings.HasPrefix(file, "proto/") {
		file = "proto/" + file
	}
	if m.Line <= 0 {
		return file
	}
	return fmt.Sprintf("%s:%d", file, m.Line)
}

// unknownBinaryDetail explains why binary names no binary, and what was
// probably meant.
func unknownBinaryDetail(binary string, binaries []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "there is no cmd/%s/ directory.", binary)
	if words := strings.Fields(binary); len(words) > 1 || strings.Contains(binary, "/") {
		b.WriteString(" `binary` is the cmd/<name> directory leaf, not a command line or a path.")
		// `x server api`: the leading word is a real binary, and the rest
		// is the subcommand path `command` exists to carry.
		if len(words) > 1 && containsString(binaries, words[0]) {
			fmt.Fprintf(&b, " To scope this config to that subcommand, write {binary: %q, command: %s}.",
				words[0], quotedList(words[1:]))
		}
	} else if near := nearestBinary(binary, binaries); near != "" {
		fmt.Fprintf(&b, " Did you mean %q?", near)
	}
	return b.String()
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// nearestBinary returns the binary whose name matches binary once case and
// the -/_ spelling are ignored — the difference between a binary's name and
// its cmd/ package directory (`forge scaffold binary workspace-proxy` writes
// cmd/workspace_proxy). "" when nothing matches that way.
func nearestBinary(binary string, binaries []string) string {
	norm := func(s string) string {
		return strings.ToLower(strings.NewReplacer("-", "", "_", "").Replace(s))
	}
	want := norm(binary)
	for _, b := range binaries {
		if norm(b) == want {
			return b
		}
	}
	return ""
}

// formatBinaryConfigBindingProblems renders the refusal as a runbook. Kept
// separate so the wording is testable without a project on disk.
func formatBinaryConfigBindingProblems(problems []BinaryConfigBindingProblem, binaries []string) string {
	var b strings.Builder
	if len(problems) == 1 {
		b.WriteString("a (forge.v1.binary_config) binding was refused\n\n")
	} else {
		fmt.Fprintf(&b, "%d (forge.v1.binary_config) bindings were refused\n\n", len(problems))
	}
	kinds := map[bindingProblemKind]bool{}
	for _, p := range problems {
		kinds[p.kind] = true
		b.WriteString("  ")
		if p.Where != "" {
			b.WriteString(p.Where + ": ")
		}
		fmt.Fprintf(&b, "message %s: (forge.v1.binary_config) = %s\n", p.Message, p.Binding)
		fmt.Fprintf(&b, "    %s\n", p.Detail)
	}

	if kinds[bindingUnknownBinary] {
		b.WriteString("\n")
		switch {
		case binaries == nil:
		case len(binaries) == 0:
			b.WriteString("This project has no cmd/ binaries.\n")
		default:
			fmt.Fprintf(&b, "Binaries in this project (cmd/<name>): %s\n", strings.Join(binaries, ", "))
		}
		b.WriteString("\nA config bound to a binary nothing builds still generates a loader, a KCL\n")
		b.WriteString("schema and an env projection, and no process ever reads any of them.\n")
		b.WriteString("Fix: point `binary` at a directory under cmd/, or create the binary first\n")
		b.WriteString("(`forge scaffold binary <name>`). A subcommand of a binary is not a\n")
		b.WriteString("binary: scope a config to one with `command: [\"server\", \"api\"]`.")
	}
	return strings.TrimRight(b.String(), "\n")
}
