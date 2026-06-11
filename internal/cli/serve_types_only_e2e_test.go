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

// TestE2ETypesOnlyService drives the real `forge generate` pipeline
// through the full types-only (services[].serve: false) lifecycle:
//
//	A. never-scaffolded: declare a serve:false service with only a proto
//	   → types + Connect client + frontend hooks generate; handlers/,
//	   bootstrap row, and MCP tools do not.
//	B. flip to served → full scaffold appears (the opt-out is reversible).
//	C. flip back to serve:false → retirement: bootstrap row + MCP tools
//	   drop, stale sweep reports the tracked handler files, audit warns.
//	D. --force-cleanup deletes the tracked generated files but never the
//	   user-written scaffold files.
//	E. idempotency: with the retired dir removed, a second generate is a
//	   no-op (stable bootstrap/manifest bytes, no stale warnings).
func TestE2ETypesOnlyService(t *testing.T) {
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

	// Declare the types-only "project" service: proto only (no handlers
	// scaffold — that's the point), serve: false + served_by in forge.yaml.
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
	setProjectServe(t, projectDir, addEntry)

	// Wire the unpublished forge/pkg + gen modules to local sources, same
	// as the fixture-corpus harness (appkit/serverkit revisions are newer
	// than any published snapshot).
	addCorpusForgePkgReplace(t, projectDir)

	// ── Phase A: serve:false from the start ─────────────────────────────
	runCmd(t, projectDir, forgeBin, "generate")

	// Types + Connect client still generate.
	assertPathExistsE2E(t, filepath.Join(projectDir, "gen", "services", "project", "v1"))
	genEntries, err := os.ReadDir(filepath.Join(projectDir, "gen", "services", "project", "v1"))
	if err != nil || len(genEntries) == 0 {
		t.Fatalf("expected generated proto types for project service, err=%v entries=%v", err, genEntries)
	}
	// Frontend hooks still generate.
	assertPathExistsE2E(t, filepath.Join(projectDir, "frontends", "web", "src", "hooks", "project-service-hooks.ts"))
	// Handlers scaffold must NOT exist.
	if _, err := os.Stat(filepath.Join(projectDir, "handlers", "project")); !os.IsNotExist(err) {
		t.Fatalf("handlers/project must not be scaffolded for serve:false, stat err = %v", err)
	}

	bootstrap := readFileE2E(t, filepath.Join(projectDir, "pkg", "app", "bootstrap.go"))
	if !strings.Contains(bootstrap, `Name: "api"`) {
		t.Errorf("bootstrap table must contain the served api row")
	}
	if strings.Contains(bootstrap, `Name: "project"`) {
		t.Errorf("bootstrap table must NOT contain a row for the types-only project service")
	}
	// The BootstrapOnly name-guard errors helpfully on the unserved name.
	if !strings.Contains(bootstrap, `case "project":`) || !strings.Contains(bootstrap, "types-only in forge.yaml") {
		t.Errorf("bootstrap must carry the unserved name-guard for 'project':\n%s", bootstrap)
	}
	if !strings.Contains(bootstrap, "served by control-plane") {
		t.Errorf("served_by documentation must render into the guard error")
	}

	assertMCPManifestServices(t, projectDir, []string{"APIService"}, []string{"ProjectService"})
	assertAuditServed(t, projectDir, forgeBin, false /* project served */, false /* retirement finding expected */)

	runCmd(t, projectDir, "go", "build", "./...")

	// ── Phase B: flip to served — the opt-out is reversible ─────────────
	setProjectServe(t, projectDir, serveTrue)
	runCmd(t, projectDir, forgeBin, "generate")
	assertPathExistsE2E(t, filepath.Join(projectDir, "handlers", "project"))
	assertPathExistsE2E(t, filepath.Join(projectDir, "handlers", "project", "authorizer_gen.go"))
	bootstrap = readFileE2E(t, filepath.Join(projectDir, "pkg", "app", "bootstrap.go"))
	if !strings.Contains(bootstrap, `Name: "project"`) {
		t.Errorf("after restoring serve, bootstrap must regain the project row")
	}
	if strings.Contains(bootstrap, `case "project":`) {
		t.Errorf("after restoring serve, the unserved guard must disappear")
	}
	assertMCPManifestServices(t, projectDir, []string{"APIService", "ProjectService"}, nil)
	runCmd(t, projectDir, "go", "build", "./...")

	// ── Phase C: retire — flip back to serve:false with scaffold on disk ─
	setProjectServe(t, projectDir, serveFalse)
	out := runCmdOutput(t, projectDir, forgeBin, "generate")
	if !strings.Contains(out, "serve: false") {
		t.Errorf("generate output must announce the types-only skip:\n%s", out)
	}
	// The tracked Tier-1 file under the retired dir is a report-only
	// stale candidate (the dir itself survives).
	if !strings.Contains(out, "stale generated file") || !strings.Contains(out, "handlers/project/authorizer_gen.go") {
		t.Errorf("generate must report the retired tracked files as stale candidates:\n%s", out)
	}
	assertPathExistsE2E(t, filepath.Join(projectDir, "handlers", "project", "authorizer_gen.go"))
	bootstrap = readFileE2E(t, filepath.Join(projectDir, "pkg", "app", "bootstrap.go"))
	if strings.Contains(bootstrap, `Name: "project"`) {
		t.Errorf("retired service must drop out of the bootstrap table")
	}
	assertMCPManifestServices(t, projectDir, []string{"APIService"}, []string{"ProjectService"})
	assertAuditServed(t, projectDir, forgeBin, false, true /* retirement finding */)

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
	assertAuditServed(t, projectDir, forgeBin, false, false /* finding cleared */)
	firstBootstrap := readFileE2E(t, filepath.Join(projectDir, "pkg", "app", "bootstrap.go"))
	firstManifest := readFileE2E(t, filepath.Join(projectDir, "gen", "mcp", "manifest.json"))

	out = runCmdOutput(t, projectDir, forgeBin, "generate")
	if strings.Contains(out, "stale generated file") {
		t.Errorf("second generate must be a no-op for cleanup:\n%s", out)
	}
	if got := readFileE2E(t, filepath.Join(projectDir, "pkg", "app", "bootstrap.go")); got != firstBootstrap {
		t.Errorf("bootstrap.go must be byte-stable across repeated generates")
	}
	if got := readFileE2E(t, filepath.Join(projectDir, "gen", "mcp", "manifest.json")); got != firstManifest {
		t.Errorf("MCP manifest must be byte-stable across repeated generates")
	}
	runCmd(t, projectDir, "go", "build", "./...")

	// The unserved name-guard is live behavior, not just rendered text:
	// running the binary with the types-only name must fail pointedly.
	t.Run("server-name-guard", func(t *testing.T) {
		cmd := exec.Command("go", "run", "./cmd", "server", "project")
		cmd.Dir = projectDir
		// AUTH_MODE=none: without it the auth interceptor's missing-
		// provider panic fires during interceptor construction, before
		// BootstrapOnly's guard gets a chance to run.
		cmd.Env = append(os.Environ(), "AUTH_MODE=none", "ENVIRONMENT=development")
		guardOut, runErr := cmd.CombinedOutput()
		if runErr == nil {
			t.Fatalf("running the types-only service name must fail; output:\n%s", guardOut)
		}
		if !strings.Contains(string(guardOut), "types-only") || !strings.Contains(string(guardOut), "control-plane") {
			t.Errorf("guard error must name the misconfiguration and served_by:\n%s", guardOut)
		}
	})
}

type serveMode int

const (
	addEntry serveMode = iota
	serveFalse
	serveTrue
)

// setProjectServe edits forge.yaml programmatically: adds the "project"
// service entry (addEntry) or flips its serve/served_by fields.
func setProjectServe(t *testing.T, projectDir string, mode serveMode) {
	t.Helper()
	path := filepath.Join(projectDir, "forge.yaml")
	cfg, err := loadProjectConfigFrom(path)
	if err != nil {
		t.Fatalf("load forge.yaml: %v", err)
	}
	notServed := false
	switch mode {
	case addEntry:
		cfg.Services = append(cfg.Services, config.ServiceConfig{
			Name:     "project",
			Type:     "go_service",
			Path:     "handlers/project",
			Serve:    &notServed,
			ServedBy: "control-plane",
		})
	default:
		found := false
		for i := range cfg.Services {
			if cfg.Services[i].Name != "project" {
				continue
			}
			found = true
			if mode == serveFalse {
				cfg.Services[i].Serve = &notServed
				cfg.Services[i].ServedBy = "control-plane"
			} else {
				cfg.Services[i].Serve = nil
				cfg.Services[i].ServedBy = ""
			}
		}
		if !found {
			t.Fatalf("project service entry not found in forge.yaml")
		}
	}
	if err := generator.WriteProjectConfigFile(cfg, path); err != nil {
		t.Fatalf("write forge.yaml: %v", err)
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
			t.Errorf("MCP manifest missing tools for served service %s: %v", svc, got)
		}
	}
	for _, svc := range absent {
		if got[svc] {
			t.Errorf("MCP manifest must not advertise types-only service %s: %v", svc, got)
		}
	}
}

// assertAuditServed runs `forge audit --json` and asserts (a) the shape
// category carries the additive served flags for the project service and
// (b) the codegen category carries (or doesn't) the retirement finding.
func assertAuditServed(t *testing.T, projectDir, forgeBin string, projectServed, wantRetirementFinding bool) {
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
		t.Fatalf("audit shape must keep the types-only service discoverable: %v", shape.Details)
	}
	if project["served"] != projectServed {
		t.Errorf("audit shape project.served = %v, want %v", project["served"], projectServed)
	}
	if !projectServed {
		if project["served_by"] != "control-plane" {
			t.Errorf("audit shape project.served_by = %v, want control-plane", project["served_by"])
		}
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
	_, hasFinding := codegen.Details["unserved_handler_dirs"]
	if hasFinding != wantRetirementFinding {
		t.Errorf("audit codegen unserved_handler_dirs present=%v, want %v (details: %v)",
			hasFinding, wantRetirementFinding, codegen.Details)
	}
	if wantRetirementFinding && codegen.Status != "warn" && codegen.Status != "error" {
		t.Errorf("retirement finding must degrade codegen status, got %s", codegen.Status)
	}
}
