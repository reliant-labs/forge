package codegen

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Binding rules for (forge.v1.binary_config).
//
// A binary-bound config message is the whole config surface of one process:
// forge generates its loader, its KCL schema and its env projection, and the
// binary's Deployment carries exactly those env vars. That only means
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

// BinaryConfigBindingProblem is one refused binding.
type BinaryConfigBindingProblem struct {
	// Message is the config message carrying the annotation.
	Message string
	// Where is "proto/<file>:<line>", or "" when the descriptor carries no
	// source position.
	Where string
	// Binary is the value the annotation named.
	Binary string
	// Detail explains this one offender: what is wrong and, where forge can
	// tell, what was probably meant.
	Detail string
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

// ValidateBinaryConfigBindings refuses every (forge.v1.binary_config)
// binding that cannot mean what it says:
//
//   - binary names no cmd/<name> directory. binaries is the set
//     ProjectBinaries returned; nil skips this check (see ProjectBinaries).
//   - two messages bind the same binary. Each binary has ONE config surface
//     — one Load<Msg>, one KCL schema its workload's env is projected from —
//     so a second message for the same binary has no defined meaning, and
//     the projection silently used whichever came first.
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
	byBinary := map[string][]*ConfigMessage{}
	var order []string
	for i := range messages {
		m := &messages[i]
		if m.Binary == "" {
			continue
		}
		if _, seen := byBinary[m.Binary]; !seen {
			order = append(order, m.Binary)
		}
		byBinary[m.Binary] = append(byBinary[m.Binary], m)

		if binaries != nil && !known[m.Binary] {
			problems = append(problems, BinaryConfigBindingProblem{
				Message: m.Name,
				Where:   configMessageWhere(*m),
				Binary:  m.Binary,
				Detail:  unknownBinaryDetail(m.Binary, binaries),
			})
		}
	}

	for _, b := range order {
		group := byBinary[b]
		if len(group) < 2 {
			continue
		}
		for _, m := range group[1:] {
			problems = append(problems, BinaryConfigBindingProblem{
				Message: m.Name,
				Where:   configMessageWhere(*m),
				Binary:  b,
				Detail: fmt.Sprintf(
					"binary %q is already bound by %s. A binary has exactly ONE config surface (one Load<Msg>, one KCL schema, one env projection), so a second message for it has no defined meaning. Merge the two messages, or compose the fields they share as a block on one of them.",
					b, group[0].Name),
			})
		}
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("%s", formatBinaryConfigBindingProblems(problems, binaries))
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
	if strings.ContainsAny(binary, " \t/") {
		b.WriteString(" `binary` is the cmd/<name> directory leaf, not a command line or a path:" +
			" a cobra subcommand of a binary is not a binary of its own.")
	} else if near := nearestBinary(binary, binaries); near != "" {
		fmt.Fprintf(&b, " Did you mean %q?", near)
	}
	return b.String()
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
		b.WriteString("a config message's (forge.v1.binary_config) binding names no process that loads it\n\n")
	} else {
		fmt.Fprintf(&b, "%d config messages' (forge.v1.binary_config) bindings name no process that loads them\n\n", len(problems))
	}
	for _, p := range problems {
		b.WriteString("  ")
		if p.Where != "" {
			b.WriteString(p.Where + ": ")
		}
		fmt.Fprintf(&b, "message %s: binary_config.binary = %q\n", p.Message, p.Binary)
		fmt.Fprintf(&b, "    %s\n", p.Detail)
	}

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
	b.WriteString("(`forge scaffold binary <name>`).")
	return b.String()
}
