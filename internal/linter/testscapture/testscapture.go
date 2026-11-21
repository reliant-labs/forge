// Package testscapture is the `tests-capture` lint rule: a Taskfile test lane
// that runs `go test` without `forge test capture`, so nothing it runs is
// recorded — no results for `forge test report`, no coverage, no flake
// history (and, once the release gates on the record, nothing behind it).
//
// A project scaffolded since v1.9.0 records every lane; one scaffolded
// before does not until the tests playbook (migrations/v1.9.0/tests) moves
// it, which `forge project upgrade` runs and `forge fix tests-capture` does
// alone. The finding is a warning at every pin: recording is report-only by
// default (docs/proposals/tests-coverage-flakes.md §5.3), and
// `lint.rules: {tests-capture: error}` is how a project makes it gate.
//
// The lanes it reads are the Taskfile's `test` and `test:<name>` tasks,
// except `test:short`: the cached inner loop records nothing on purpose. A
// command counts as running go test when it invokes `go test`; one that also
// names `test capture` is recorded.
//
//forge:exclude-contract: a pure source analyzer; its surface is CheckTaskfile and the rule ID
package testscapture

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/reliant-labs/forge/internal/linter/finding"
)

// Rule is the rule ID lint.rules grades.
const Rule = "tests-capture"

// Playbook is the migration skill the finding points at.
const Playbook = "migrations/v1.9.0/tests"

// goTestRe finds an invocation of go test: at the start of the command or
// after a separator, a space or a template action (`{{end}}go test`).
var goTestRe = regexp.MustCompile(`(^|[\s;&|(}])go test(\s|$)`)

// CheckTaskfile returns one finding per test lane of the Taskfile at rel
// (its bytes in src) whose go test runs without capture, at the line of the
// first such command. A Taskfile that does not parse yields none: task
// reports that itself.
func CheckTaskfile(rel string, src []byte) []finding.Finding {
	var doc struct {
		Tasks yaml.Node `yaml:"tasks"`
	}
	if err := yaml.Unmarshal(src, &doc); err != nil || doc.Tasks.Kind != yaml.MappingNode {
		return nil
	}
	var out []finding.Finding
	for i := 0; i+1 < len(doc.Tasks.Content); i += 2 {
		name := doc.Tasks.Content[i].Value
		if !lane(name) {
			continue
		}
		if line, ok := uncaptured(doc.Tasks.Content[i+1]); ok {
			out = append(out, finding.Finding{
				Rule:     Rule,
				Severity: finding.SeverityWarning,
				File:     rel,
				Line:     line,
				Message: fmt.Sprintf("task %s runs `go test` without `forge test capture`, so its results and coverage are not recorded "+
					"(`forge test report`, coverage and flake history see nothing of it)", name),
				Remediation: "`forge project upgrade` applies the tests playbook; `forge fix tests-capture` rewrites a scaffolded lane alone, and `forge skill load " +
					Playbook + "` covers a lane you wrote yourself. A deliberate unrecorded lane: lint.rules: {tests-capture: off}",
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Line < out[j].Line })
	return out
}

func lane(name string) bool {
	return (name == "test" || strings.HasPrefix(name, "test:")) && name != "test:short"
}

// uncaptured returns the line of a task's first command that runs go test
// without capture. A task is a string (one command), a list of commands, or
// a mapping whose cmds is that list; each command is a string or a mapping
// with cmd.
func uncaptured(task *yaml.Node) (int, bool) {
	var cmds []*yaml.Node
	switch task.Kind {
	case yaml.ScalarNode:
		cmds = []*yaml.Node{task}
	case yaml.SequenceNode:
		cmds = task.Content
	case yaml.MappingNode:
		for i := 0; i+1 < len(task.Content); i += 2 {
			if task.Content[i].Value == "cmds" && task.Content[i+1].Kind == yaml.SequenceNode {
				cmds = task.Content[i+1].Content
			}
		}
	}
	for _, c := range cmds {
		text, line := command(c)
		if goTestRe.MatchString(text) && !strings.Contains(text, "test capture") {
			return line, true
		}
	}
	return 0, false
}

func command(c *yaml.Node) (string, int) {
	switch c.Kind {
	case yaml.ScalarNode:
		return c.Value, c.Line
	case yaml.MappingNode:
		for i := 0; i+1 < len(c.Content); i += 2 {
			if c.Content[i].Value == "cmd" && c.Content[i+1].Kind == yaml.ScalarNode {
				return c.Content[i+1].Value, c.Content[i+1].Line
			}
		}
	}
	return "", 0
}
