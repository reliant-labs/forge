package testscapture

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func lanes(t *testing.T, src string) map[string]int {
	t.Helper()
	out := map[string]int{}
	for _, f := range CheckTaskfile("Taskfile.yml", []byte(src)) {
		if f.Rule != Rule || f.Severity != "warning" || f.File != "Taskfile.yml" {
			t.Errorf("finding %+v: want a %s warning on Taskfile.yml", f, Rule)
		}
		out[f.Message] = f.Line
	}
	return out
}

func TestCheckTaskfile(t *testing.T) {
	for name, tc := range map[string]struct {
		src  string
		want int // findings
		line int // the first one's line
	}{
		"a plain go test lane":                  {"tasks:\n  test:\n    cmds:\n      - go test ./...\n", 1, 4},
		"behind SLOT, quoted":                   {"tasks:\n  test:integration:\n    cmds:\n      - \"{{.SLOT}} go test -tags=integration ./...\"\n", 1, 4},
		"a cmd: mapping":                        {"tasks:\n  test:e2e:\n    cmds:\n      - cmd: go test -tags=e2e ./e2e/...\n        silent: true\n", 1, 4},
		"a task that is one command":            {"tasks:\n  test: go test ./...\n", 1, 2},
		"captured":                              {"tasks:\n  test:\n    cmds:\n      - >-\n        {{.SLOT}} {{if .RUN}}{{.FORGE}} test capture --run '{{.RUN}}' --lane unit -- {{end}}go test -count=1\n", 0, 0},
		"test:short records nothing on purpose": {"tasks:\n  test:short:\n    cmds:\n      - go test -short ./...\n", 0, 0},
		"not a test lane":                       {"tasks:\n  coverage:\n    cmds:\n      - go test -coverprofile=c.out ./...\n", 0, 0},
		"a lane with no go test":                {"tasks:\n  test:all:\n    cmds:\n      - task: test\n  test:web:\n    cmds:\n      - npm test\n", 0, 0},
		"gotest is not go test":                 {"tasks:\n  test:\n    cmds:\n      - gotestsum ./...\n", 0, 0},
		"a pipeline still runs it":              {"tasks:\n  test:\n    cmds:\n      - set -o pipefail; go test -json ./... | tee t.json\n", 1, 4},
		"no tasks":                              {"version: \"3\"\n", 0, 0},
		"not YAML":                              {"tasks: [\n", 0, 0},
	} {
		t.Run(name, func(t *testing.T) {
			fs := CheckTaskfile("Taskfile.yml", []byte(tc.src))
			if len(fs) != tc.want {
				t.Fatalf("got %d finding(s) %+v, want %d", len(fs), fs, tc.want)
			}
			if tc.want > 0 && fs[0].Line != tc.line {
				t.Errorf("line %d, want %d", fs[0].Line, tc.line)
			}
		})
	}
}

// A forge v1.8.0 scaffold's Taskfile has three unrecorded lanes, and this
// forge's has none: the rule and the tests playbook agree on what moved.
func TestCheckTaskfileOnScaffolds(t *testing.T) {
	root := filepath.Join("..", "..", "codemod", "testdata", "testscapture", "v180-service")
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	have := map[string]bool{}
	for msg := range lanes(t, read("in/Taskfile.yml")) {
		have[strings.Fields(msg)[1]] = true // "task <name> runs …"
	}
	want := map[string]bool{"test": true, "test:integration": true, "test:e2e": true}
	if !reflect.DeepEqual(have, want) {
		t.Errorf("v1.8 scaffold lanes = %v, want %v", have, want)
	}
	if got := lanes(t, read("want/Taskfile.yml")); len(got) != 0 {
		t.Errorf("this forge's scaffold has unrecorded lanes: %v", got)
	}
}
