package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

// TestGenerateHookHelperProcess is not a test: it is the command the hook
// tests run, re-executing this test binary so they need no shell and behave
// the same on every platform. Its argv after "--" is the action:
//
//	write <file>          create <file> in the working directory
//	require <file> write <out>  fail unless <file> exists, then create <out>
//	fail                  exit 3
func TestGenerateHookHelperProcess(t *testing.T) {
	if os.Getenv("FORGE_GENERATE_HOOK_HELPER") != "1" {
		return
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	for len(args) > 0 {
		switch args[0] {
		case "fail":
			os.Exit(3)
		case "require":
			if _, err := os.Stat(args[1]); err != nil {
				os.Exit(4)
			}
			args = args[2:]
		case "write":
			if err := os.WriteFile(args[1], []byte("ok"), 0o644); err != nil {
				os.Exit(5)
			}
			args = args[2:]
		default:
			os.Exit(6)
		}
	}
	os.Exit(0)
}

func helperHook(name, dir string, action ...string) config.GenerateHook {
	cmd := append([]string{os.Args[0], "-test.run=^TestGenerateHookHelperProcess$", "--"}, action...)
	return config.GenerateHook{Name: name, Command: cmd, Dir: dir}
}

func TestRunGenerateHooks_InOrderAndInDir(t *testing.T) {
	t.Setenv("FORGE_GENERATE_HOOK_HELPER", "1")
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	hooks := []config.GenerateHook{
		helperHook("first", "", "write", "a.txt"),
		// Runs in sub/, and only succeeds if "first" already ran.
		helperHook("second", "sub", "require", filepath.Join("..", "a.txt"), "write", "b.txt"),
	}
	var out bytes.Buffer
	if err := runGenerateHooks(root, hooks, &out, &out); err != nil {
		t.Fatalf("runGenerateHooks: %v\n%s", err, out.String())
	}
	if _, err := os.Stat(filepath.Join(root, "sub", "b.txt")); err != nil {
		t.Errorf("second hook did not run in its dir: %v", err)
	}
	if !strings.Contains(out.String(), `generate hook "first"`) || !strings.Contains(out.String(), `generate hook "second"`) {
		t.Errorf("each hook should be announced, got:\n%s", out.String())
	}
}

func TestRunGenerateHooks_FailureStopsAndNamesTheHook(t *testing.T) {
	t.Setenv("FORGE_GENERATE_HOOK_HELPER", "1")
	root := t.TempDir()
	hooks := []config.GenerateHook{
		helperHook("broken", "", "fail"),
		helperHook("after", "", "write", "after.txt"),
	}
	var out bytes.Buffer
	err := runGenerateHooks(root, hooks, &out, &out)
	if err == nil || !strings.Contains(err.Error(), `generate hook "broken"`) {
		t.Fatalf("want an error naming the failed hook, got %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(root, "after.txt")); statErr == nil {
		t.Error("a hook after a failed one must not run")
	}
}

func TestRunGenerateHooks_MissingCommand(t *testing.T) {
	hooks := []config.GenerateHook{{Name: "q", Command: []string{"forge-test-no-such-binary-6b1f"}}}
	var out bytes.Buffer
	err := runGenerateHooks(t.TempDir(), hooks, &out, &out)
	if err == nil || !strings.Contains(err.Error(), "not found on PATH") {
		t.Fatalf("want a not-found-on-PATH error, got %v", err)
	}
}

func TestWritesGenerateHooks(t *testing.T) {
	ctx := &pipelineContext{Cfg: &config.ProjectConfig{Generate: config.GenerateConfig{Hooks: []config.GenerateHook{
		{Name: "a", Writes: []string{"internal/db/sqlc", "./docs/"}},
		{Name: "b", Writes: []string{"docs", "web/src/gen"}},
	}}}}
	got := writesGenerateHooks(ctx)
	want := []string{"docs", "internal/db/sqlc", "web/src/gen"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("writesGenerateHooks = %v, want %v", got, want)
	}
	if hasGenerateHooks(&pipelineContext{Cfg: &config.ProjectConfig{}}) {
		t.Error("a project with no hooks must skip the step")
	}
}
