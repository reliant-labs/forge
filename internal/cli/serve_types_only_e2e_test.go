//go:build e2e

package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
)

// TestE2ERegistrationTypesOnlyService drives the real `forge generate`
// pipeline through the registration-in-code lifecycle (what a binary
// serves is the row list in the user-owned pkg/app/services.go):
//
//	A. newly added (UNLISTED): a "project" service appears in forge.yaml
//	   + proto but nowhere in services.go → handlers scaffold + row
//	   constructor generate (the user might be about to implement it),
//	   but the binary does NOT serve it: no MCP tools, audit warns "row
//	   constructor generated but unreferenced", generate prints the
//	   exact line to add.
//	B. registered: the user adds the serviceRowProject line → MCP tools
//	   appear, audit clears. The opt-in is one line of user-owned code.
//	C. tombstoned (types-only): the user deletes the row and leaves a
//	   comment naming the serving binary → handlers Tier-1 stops
//	   regenerating (stale candidates), MCP excludes, audit warns with
//	   state=tombstoned.
//	D. --force-cleanup deletes the tracked generated files but never the
//	   user-written scaffold files; the user removes the dir.
//	E. steady state + idempotency: the tombstone comment keeps the
//	   scaffold retired across repeated generates; bootstrap, manifest,
//	   and services_gen are byte-stable.
//
// Plus a runtime probe: BootstrapOnly's registration guard fails
// pointedly when the unregistered name is passed to `server <name>`.
func TestE2ERegistrationTypesOnlyService(t *testing.T) {
	t.Parallel() // independent project in its own t.TempDir; binary shared via sync.Once
	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()

	runCmd(t, dir, forgeBin,
		"new", "tonly",
		"--mod", "github.com/test/tonly",
		"--service", "api",
		"--frontend", "web",
	)
	projectDir := filepath.Join(dir, "tonly")
	assertPathExistsE2E(t, filepath.Join(projectDir, "forge.yaml"))

	// `forge new` scaffolds the user-owned registration file listing the
	// initial service — the migration contract for fresh projects.
	registryPath := filepath.Join(projectDir, "pkg", "app", "services.go")
	registry := readFileE2E(t, registryPath)
	if !strings.Contains(registry, "serviceRowAPI(app, cfg, logger, opts...),") {
		t.Fatalf("forge new must scaffold pkg/app/services.go with the api row:\n%s", registry)
	}

	// Declare the second "project" service: proto + forge.yaml entry.
	// Its canonical implementation will live in a sibling binary
	// (control-plane); this repo ends up consuming only types/client.
	protoDir := filepath.Join(projectDir, "proto", "services", "project", "v1")
	if err := os.MkdirAll(protoDir, 0o755); err != nil {
		t.Fatalf("mkdir proto dir: %v", err)
	}
	const projectProto = `syntax = "proto3";

package services.project.v1;

option go_package = "github.com/test/tonly/gen/services/project/v1;projectv1";

// ProjectService is canonically served by a sibling binary
// (control-plane); this repo only consumes the generated types/client.
service ProjectService {
  rpc CreateProject(CreateProjectRequest) returns (CreateProjectResponse) {}
  rpc GetProject(GetProjectRequest) returns (GetProjectResponse) {}
}

message CreateProjectRequest {
  string name = 1;
}

message CreateProjectResponse {
  string id = 1;
}

message GetProjectRequest {
  string id = 1;
}

message GetProjectResponse {
  string id = 1;
  string name = 2;
}
`
	if err := os.WriteFile(filepath.Join(protoDir, "project.proto"), []byte(projectProto), 0o644); err != nil {
		t.Fatalf("write project.proto: %v", err)
	}
	addProjectServiceEntry(t, projectDir)

	// Wire the unpublished forge/pkg + gen modules to local sources, same
	// as the fixture-corpus harness (appkit/serverkit revisions are newer
	// than any published snapshot).
	addCorpusForgePkgReplace(t, projectDir)

	// ── Phase A: newly added — generated but NOT served ─────────────────
	out := runCmdOutput(t, projectDir, forgeBin, "generate")

	// Types + Connect client + frontend hooks generate (caller-side
	// artifacts are never gated on registration).
	assertPathExistsE2E(t, filepath.Join(projectDir, "gen", "services", "project", "v1"))
	genEntries, err := os.ReadDir(filepath.Join(projectDir, "gen", "services", "project", "v1"))
	if err != nil || len(genEntries) == 0 {
		t.Fatalf("expected generated proto types for project service, err=%v entries=%v", err, genEntries)
	}
	assertPathExistsE2E(t, filepath.Join(projectDir, "frontends", "web", "src", "hooks", "project-service-hooks.ts"))

	// The handlers scaffold + row constructor DO generate for a newly
	// added service — implement-then-register is the supported flow.
	assertPathExistsE2E(t, filepath.Join(projectDir, "handlers", "project"))
	rows := readFileE2E(t, filepath.Join(projectDir, "pkg", "app", "services_gen.go"))
	if !strings.Contains(rows, "func serviceRowProject(") {
		t.Errorf("services_gen.go must carry the row constructor for the unregistered project service:\n%s", rows)
	}

	// But the binary does not SERVE it: generate says so, with the exact
	// line to add.
	if !strings.Contains(out, "serviceRowProject(app, cfg, logger, opts...),") {
		t.Errorf("generate must print the registration line for the unlisted service:\n%s", out)
	}

	// The bootstrap registration guard knows the full inventory.
	bootstrap := readFileE2E(t, filepath.Join(projectDir, "pkg", "app", "bootstrap.go"))
	if !strings.Contains(bootstrap, `"project"`) || !strings.Contains(bootstrap, "not registered in pkg/app/services.go") {
		t.Errorf("bootstrap must carry the registration guard naming the project inventory entry:\n%s", bootstrap)
	}

	assertMCPManifestServices(t, projectDir, []string{"APIService"}, []string{"ProjectService"})
	assertAuditRegistration(t, projectDir, forgeBin, false /* served */, "unlisted")

	runCmd(t, projectDir, "go", "build", "./...")

	// ── Phase B: register — one user-owned line serves the service ──────
	editServiceRegistry(t, registryPath, registerProjectRow)
	runCmd(t, projectDir, forgeBin, "generate")
	assertMCPManifestServices(t, projectDir, []string{"APIService", "ProjectService"}, nil)
	assertAuditRegistration(t, projectDir, forgeBin, true, "")
	runCmd(t, projectDir, "go", "build", "./...")

	// ── Phase C: retire — delete the row, leave the tombstone comment ───
	editServiceRegistry(t, registryPath, tombstoneProjectRow)
	out = runCmdOutput(t, projectDir, forgeBin, "generate")
	if !strings.Contains(out, "types-only — not registered in pkg/app/services.go") {
		t.Errorf("generate output must announce the types-only skip:\n%s", out)
	}
	// The tracked Tier-1 file under the retired dir is a report-only
	// stale candidate (the dir itself survives).
	if !strings.Contains(out, "stale generated file") || !strings.Contains(out, "handlers/project/authorizer_gen.go") {
		t.Errorf("generate must report the retired tracked files as stale candidates:\n%s", out)
	}
	assertPathExistsE2E(t, filepath.Join(projectDir, "handlers", "project", "authorizer_gen.go"))
	rows = readFileE2E(t, filepath.Join(projectDir, "pkg", "app", "services_gen.go"))
	if strings.Contains(rows, "serviceRowProject") {
		t.Errorf("tombstoned service must drop out of services_gen.go")
	}
	assertMCPManifestServices(t, projectDir, []string{"APIService"}, []string{"ProjectService"})
	assertAuditRegistration(t, projectDir, forgeBin, false, "tombstoned")

	// ── Phase D: --force-cleanup removes generated files, never user files ─
	// --skip-validate: the user-owned authorizer.go references the
	// generated authorizer until the user moves or deletes their code —
	// exactly what the audit finding instructs.
	runCmd(t, projectDir, forgeBin, "generate", "--force-cleanup", "--skip-validate")
	if _, err := os.Stat(filepath.Join(projectDir, "handlers", "project", "authorizer_gen.go")); !os.IsNotExist(err) {
		t.Errorf("--force-cleanup must delete the tracked authorizer_gen.go, stat err = %v", err)
	}
	for _, userFile := range []string{"service.go", "handlers.go"} {
		if _, err := os.Stat(filepath.Join(projectDir, "handlers", "project", userFile)); err != nil {
			t.Errorf("user-written %s must survive --force-cleanup: %v", userFile, err)
		}
	}

	// User completes the retirement by removing their scaffold files.
	if err := os.RemoveAll(filepath.Join(projectDir, "handlers", "project")); err != nil {
		t.Fatalf("remove retired dir: %v", err)
	}

	// ── Phase E: steady state + idempotency ─────────────────────────────
	out = runCmdOutput(t, projectDir, forgeBin, "generate")
	if strings.Contains(out, "stale generated file") {
		t.Errorf("steady-state generate must report no stale candidates:\n%s", out)
	}
	// The tombstone comment keeps the scaffold retired — generate must
	// NOT re-scaffold handlers/project for a comment-mentioned service.
	if _, err := os.Stat(filepath.Join(projectDir, "handlers", "project")); !os.IsNotExist(err) {
		t.Fatalf("tombstoned service must stay retired (no handlers re-scaffold), stat err = %v", err)
	}
	assertAuditRegistration(t, projectDir, forgeBin, false, "")
	firstBootstrap := readFileE2E(t, filepath.Join(projectDir, "pkg", "app", "bootstrap.go"))
	firstRows := readFileE2E(t, filepath.Join(projectDir, "pkg", "app", "services_gen.go"))
	firstManifest := readFileE2E(t, filepath.Join(projectDir, "gen", "mcp", "manifest.json"))

	out = runCmdOutput(t, projectDir, forgeBin, "generate")
	if strings.Contains(out, "stale generated file") {
		t.Errorf("second generate must be a no-op for cleanup:\n%s", out)
	}
	if got := readFileE2E(t, filepath.Join(projectDir, "pkg", "app", "bootstrap.go")); got != firstBootstrap {
		t.Errorf("bootstrap.go must be byte-stable across repeated generates")
	}
	if got := readFileE2E(t, filepath.Join(projectDir, "pkg", "app", "services_gen.go")); got != firstRows {
		t.Errorf("services_gen.go must be byte-stable across repeated generates")
	}
	if got := readFileE2E(t, filepath.Join(projectDir, "gen", "mcp", "manifest.json")); got != firstManifest {
		t.Errorf("MCP manifest must be byte-stable across repeated generates")
	}
	runCmd(t, projectDir, "go", "build", "./...")

	// The registration guard is live behavior, not just rendered text:
	// running the binary with the unregistered name must fail pointedly.
	t.Run("server-name-guard", func(t *testing.T) {
		cmd := exec.Command("go", "run", "./cmd", "server", "project")
		cmd.Dir = projectDir
		// AUTH_MODE=none: without it the auth interceptor's missing-
		// provider panic fires during interceptor construction, before
		// BootstrapOnly's guard gets a chance to run.
		cmd.Env = append(os.Environ(), "AUTH_MODE=none", "ENVIRONMENT=development")
		guardOut, runErr := cmd.CombinedOutput()
		if runErr == nil {
			t.Fatalf("running the unregistered service name must fail; output:\n%s", guardOut)
		}
		if !strings.Contains(string(guardOut), "not registered in pkg/app/services.go") {
			t.Errorf("guard error must name the registration file:\n%s", guardOut)
		}
	})
}

// addProjectServiceEntry appends the "project" service to forge.yaml —
// a plain entry; serving is decided in pkg/app/services.go, not yaml.
func addProjectServiceEntry(t *testing.T, projectDir string) {
	t.Helper()
	path := filepath.Join(projectDir, "forge.yaml")
	cfg, err := loadProjectConfigFrom(path)
	if err != nil {
		t.Fatalf("load forge.yaml: %v", err)
	}
	cfg.Services = append(cfg.Services, config.ServiceConfig{
		Name: "project",
		Type: "go_service",
		Path: "handlers/project",
	})
	if err := generator.WriteProjectConfigFile(cfg, path); err != nil {
		t.Fatalf("write forge.yaml: %v", err)
	}
}

type registryEdit int

const (
	registerProjectRow registryEdit = iota
	tombstoneProjectRow
)

// editServiceRegistry mutates the user-owned pkg/app/services.go the
// way a user (or their agent) would: adding the project row after the
// api row, or replacing it with the tombstone comment.
func editServiceRegistry(t *testing.T, registryPath string, edit registryEdit) {
	t.Helper()
	const apiRow = "serviceRowAPI(app, cfg, logger, opts...),"
	const projectRow = "serviceRowProject(app, cfg, logger, opts...),"
	const tombstone = "// project: types-only — served by control-plane"

	data, err := os.ReadFile(registryPath)
	if err != nil {
		t.Fatalf("read services.go: %v", err)
	}
	content := string(data)
	switch edit {
	case registerProjectRow:
		if !strings.Contains(content, apiRow) {
			t.Fatalf("services.go missing the api row to anchor the edit:\n%s", content)
		}
		content = strings.Replace(content, apiRow, apiRow+"\n\t\t"+projectRow, 1)
	case tombstoneProjectRow:
		if !strings.Contains(content, projectRow) {
			t.Fatalf("services.go missing the project row to tombstone:\n%s", content)
		}
		content = strings.Replace(content, projectRow, tombstone, 1)
	}
	if err := os.WriteFile(registryPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write services.go: %v", err)
	}
}

// assertMCPManifestServices asserts gen/mcp/manifest.json advertises
// tools for every service in want and none for the services in absent.
func assertMCPManifestServices(t *testing.T, projectDir string, want, absent []string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(projectDir, "gen", "mcp", "manifest.json"))
	if err != nil {
		t.Fatalf("read MCP manifest: %v", err)
	}
	var manifest struct {
		Tools []struct {
			Service string `json:"service"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("parse MCP manifest: %v", err)
	}
	got := map[string]bool{}
	for _, tool := range manifest.Tools {
		got[tool.Service] = true
	}
	for _, svc := range want {
		if !got[svc] {
			t.Errorf("MCP manifest missing tools for registered service %s: %v", svc, got)
		}
	}
	for _, svc := range absent {
		if got[svc] {
			t.Errorf("MCP manifest must not advertise unregistered service %s: %v", svc, got)
		}
	}
}

// assertAuditRegistration runs `forge audit --json` and asserts (a) the
// shape category carries the served flag for the project service and
// (b) the codegen category carries (or doesn't) the
// unregistered_services finding with the expected state ("" = no
// finding expected).
func assertAuditRegistration(t *testing.T, projectDir, forgeBin string, projectServed bool, wantFindingState string) {
	t.Helper()
	out := runCmdOutput(t, projectDir, forgeBin, "audit", "--json")
	var report struct {
		Categories map[string]struct {
			Status  string         `json:"status"`
			Details map[string]any `json:"details"`
		} `json:"categories"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("parse audit JSON: %v\n%s", err, out)
	}

	shape := report.Categories["shape"]
	services, _ := shape.Details["services"].([]any)
	var project map[string]any
	for _, s := range services {
		m := s.(map[string]any)
		if m["name"] == "project" {
			project = m
		}
	}
	if project == nil {
		t.Fatalf("audit shape must keep the unregistered service discoverable: %v", shape.Details)
	}
	if project["served"] != projectServed {
		t.Errorf("audit shape project.served = %v, want %v", project["served"], projectServed)
	}
	if !projectServed {
		if rpcs, ok := project["rpcs"].([]any); ok {
			for _, r := range rpcs {
				m := r.(map[string]any)
				if m["served"] != false {
					t.Errorf("audit shape rpc %v must carry additive served:false", m["name"])
				}
				if m["mcp_callable"] != false {
					t.Errorf("audit shape rpc %v must report mcp_callable:false", m["name"])
				}
			}
		}
	}

	codegen := report.Categories["codegen"]
	findings, hasFinding := codegen.Details["unregistered_services"]
	if wantFindingState == "" {
		if hasFinding {
			t.Errorf("audit codegen unregistered_services present, want absent (details: %v)", codegen.Details)
		}
		return
	}
	if !hasFinding {
		t.Fatalf("audit codegen missing unregistered_services finding (details: %v)", codegen.Details)
	}
	list, _ := findings.([]any)
	found := false
	for _, f := range list {
		m := f.(map[string]any)
		if m["service"] == "project" {
			found = true
			if m["state"] != wantFindingState {
				t.Errorf("finding state = %v, want %s", m["state"], wantFindingState)
			}
		}
	}
	if !found {
		t.Errorf("unregistered_services has no entry for project: %v", findings)
	}
	if codegen.Status != "warn" && codegen.Status != "error" {
		t.Errorf("registration finding must degrade codegen status, got %s", codegen.Status)
	}
}
