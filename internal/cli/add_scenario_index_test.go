package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestValidateScenarioName moved with `forge add scenario` to the
// internal/cli/add group (add/add_scenario_test.go). The tests that remain
// here exercise the scenario-index emitters that stay in internal/cli
// (writeScenariosIndex / emitScenarioScaffolding / scenarioImportIdent in
// generate_frontend_mocks.go).

func TestScenarioImportIdent(t *testing.T) {
	cases := map[string]string{
		"default":            "scenario_default",
		"github-connected":   "scenario_githubConnected",
		"a-b-c":              "scenario_aBC",
		"single":             "scenario_single",
		"github-1-connected": "scenario_github1Connected",
	}
	for in, want := range cases {
		t.Run(in, func(t *testing.T) {
			got := scenarioImportIdent(in)
			if got != want {
				t.Errorf("scenarioImportIdent(%q) = %q, want %q", in, got, want)
			}
		})
	}
}

// TestWriteScenariosIndex_Idempotent verifies that re-running
// writeScenariosIndex on the same directory contents produces
// byte-identical output.
func TestWriteScenariosIndex_Idempotent(t *testing.T) {
	dir := t.TempDir()

	// Seed two scenario files.
	mustWriteFile(t, filepath.Join(dir, "default.ts"), "// default")
	mustWriteFile(t, filepath.Join(dir, "github-connected.ts"), "// gh")

	if err := writeScenariosIndex(dir); err != nil {
		t.Fatalf("first writeScenariosIndex: %v", err)
	}
	first, err := os.ReadFile(filepath.Join(dir, "index.ts"))
	if err != nil {
		t.Fatalf("read first index: %v", err)
	}

	if err := writeScenariosIndex(dir); err != nil {
		t.Fatalf("second writeScenariosIndex: %v", err)
	}
	second, err := os.ReadFile(filepath.Join(dir, "index.ts"))
	if err != nil {
		t.Fatalf("read second index: %v", err)
	}

	if string(first) != string(second) {
		t.Errorf("non-idempotent regeneration:\nfirst:\n%s\nsecond:\n%s", first, second)
	}

	// Sanity-check expected content.
	body := string(first)
	for _, expect := range []string{
		`import scenario_default from "./default";`,
		`import scenario_githubConnected from "./github-connected";`,
		`[scenario_default.name]: scenario_default,`,
		`[scenario_githubConnected.name]: scenario_githubConnected,`,
		`export { default as defaultScenario } from "./default";`,
	} {
		if !strings.Contains(body, expect) {
			t.Errorf("index missing %q in:\n%s", expect, body)
		}
	}
}

// TestEmitScenarioScaffolding_SeedsDefaultOnce verifies that emitting
// scaffolding twice doesn't overwrite a hand-edited default.ts.
func TestEmitScenarioScaffolding_SeedsDefaultOnce(t *testing.T) {
	dir := t.TempDir()
	mocksDir := filepath.Join(dir, "src", "mocks")
	if err := os.MkdirAll(mocksDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := emitScenarioScaffolding(mocksDir, nil); err != nil {
		t.Fatalf("first emit: %v", err)
	}

	defaultPath := filepath.Join(mocksDir, "scenarios", "default.ts")
	if _, err := os.Stat(defaultPath); err != nil {
		t.Fatalf("default.ts not written: %v", err)
	}

	// Hand-edit default.ts.
	mustWriteFile(t, defaultPath, "// HAND-EDITED")

	if err := emitScenarioScaffolding(mocksDir, nil); err != nil {
		t.Fatalf("second emit: %v", err)
	}

	got, err := os.ReadFile(defaultPath)
	if err != nil {
		t.Fatalf("read default.ts: %v", err)
	}
	if string(got) != "// HAND-EDITED" {
		t.Errorf("default.ts was overwritten on re-emit; got:\n%s", got)
	}

	// scenario-types.ts is regenerated every run — so it should exist and
	// not be the hand-edited marker.
	typesPath := filepath.Join(mocksDir, "scenario-types.ts")
	tb, err := os.ReadFile(typesPath)
	if err != nil {
		t.Fatalf("read scenario-types.ts: %v", err)
	}
	if !strings.Contains(string(tb), "export interface Scenario") {
		t.Errorf("scenario-types.ts missing Scenario interface; got:\n%s", tb)
	}
	// Hybrid mode contract: both new optional fields must be exported on
	// the Scenario interface, otherwise scenarios can't opt into
	// passthrough or bypass-auth and the hybrid wiring is dead code.
	for _, want := range []string{"passthrough?:", `auth?: "bypass" | "required"`} {
		if !strings.Contains(string(tb), want) {
			t.Errorf("scenario-types.ts missing %q (required for hybrid mode); got:\n%s", want, tb)
		}
	}

	// index.ts exists and references the hand-edited default.
	indexPath := filepath.Join(mocksDir, "scenarios", "index.ts")
	ib, err := os.ReadFile(indexPath)
	if err != nil {
		t.Fatalf("read index.ts: %v", err)
	}
	if !strings.Contains(string(ib), `from "./default"`) {
		t.Errorf("index.ts missing default reference; got:\n%s", ib)
	}
}

// mustWriteFile is defined in map_test.go in this package; reuse it.
