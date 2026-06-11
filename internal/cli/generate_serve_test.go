// Tests for types-only services (forge.yaml services[].serve: false):
// the served-filter chokepoint, the MCP manifest gate, the audit
// surfaces (shape served:false additive marker + codegen retirement
// finding), and the stale-cleanup retirement path. The full end-to-end
// flow (real `forge generate` on a scaffolded project) lives in
// serve_types_only_e2e_test.go behind the e2e build tag.
package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
)

// serveTestConfig parses a two-service forge.yaml: "api" served (default,
// field absent) and "project" types-only with served_by documentation.
func serveTestConfig(t *testing.T) *config.ProjectConfig {
	t.Helper()
	yamlBody := `name: demo
module_path: github.com/example/demo
services:
  - name: api
    type: go_service
    path: handlers/api
  - name: project
    type: go_service
    path: handlers/project
    serve: false
    served_by: control-plane
`
	cfg, err := config.LoadStrict([]byte(yamlBody), "forge.yaml")
	if err != nil {
		t.Fatalf("load fixture config: %v", err)
	}
	return cfg
}

func serveTestServiceDefs() []codegen.ServiceDef {
	return []codegen.ServiceDef{
		{Name: "ApiService", Package: "api.v1", Methods: []codegen.Method{
			{Name: "Get", InputType: "GetRequest", OutputType: "GetResponse"},
		}},
		{Name: "ProjectService", Package: "reliant.v1", Methods: []codegen.Method{
			{Name: "CreateProject", InputType: "CreateProjectRequest", OutputType: "CreateProjectResponse"},
			{Name: "GetProject", InputType: "GetProjectRequest", OutputType: "GetProjectResponse"},
		}},
	}
}

func TestServedServiceDefs_FiltersTypesOnlyServices(t *testing.T) {
	cfg := serveTestConfig(t)
	served, unserved := servedServiceDefs(cfg, serveTestServiceDefs())
	if len(served) != 1 || served[0].Name != "ApiService" {
		t.Errorf("served = %+v, want [ApiService]", served)
	}
	if len(unserved) != 1 || unserved[0].Name != "ProjectService" {
		t.Errorf("unserved = %+v, want [ProjectService]", unserved)
	}
}

func TestServedServiceDefs_NilConfigServesEverything(t *testing.T) {
	defs := serveTestServiceDefs()
	served, unserved := servedServiceDefs(nil, defs)
	if len(served) != len(defs) || len(unserved) != 0 {
		t.Errorf("nil cfg must serve everything: served=%d unserved=%d", len(served), len(unserved))
	}
}

func TestUnservedHelpers_DirSkipsAndBootstrapGuards(t *testing.T) {
	cfg := serveTestConfig(t)
	skips := unservedHandlerDirSkips(cfg)
	if !skips["project"] || len(skips) != 1 {
		t.Errorf("unservedHandlerDirSkips = %v, want {project:true}", skips)
	}
	guards := unservedBootstrapGuards(cfg)
	if len(guards) != 1 || guards[0].Name != "project" || guards[0].ServedBy != "control-plane" {
		t.Errorf("unservedBootstrapGuards = %+v, want [{project control-plane}]", guards)
	}
	if got := unservedHandlerDirSkips(nil); got != nil {
		t.Errorf("nil cfg dir skips = %v, want nil", got)
	}
	if got := unservedBootstrapGuards(nil); got != nil {
		t.Errorf("nil cfg guards = %v, want nil", got)
	}
}

// TestStepMCPManifest_ExcludesUnservedRPCs drives the real stepMCPManifest
// against a synthetic pipeline context and asserts the emitted
// gen/mcp/manifest.json advertises only the served service's tools.
func TestStepMCPManifest_ExcludesUnservedRPCs(t *testing.T) {
	dir := t.TempDir()
	ctx := &pipelineContext{
		ProjectDir: dir,
		AbsPath:    dir,
		Cfg:        serveTestConfig(t),
		Services:   serveTestServiceDefs(),
		Checksums:  &generator.FileChecksums{Files: map[string]generator.FileChecksumEntry{}},
	}
	if err := stepMCPManifest(ctx); err != nil {
		t.Fatalf("stepMCPManifest: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "gen", "mcp", "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var manifest struct {
		Tools []struct {
			Service string `json:"service"`
			Method  string `json:"method"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		t.Fatalf("unmarshal manifest: %v", err)
	}
	if len(manifest.Tools) != 1 {
		t.Fatalf("tools = %+v, want exactly the served service's 1 RPC", manifest.Tools)
	}
	if manifest.Tools[0].Service != "ApiService" || manifest.Tools[0].Method != "Get" {
		t.Errorf("tools[0] = %+v, want ApiService/Get", manifest.Tools[0])
	}
	if strings.Contains(string(data), "ProjectService") {
		t.Errorf("manifest must not advertise the types-only ProjectService:\n%s", data)
	}
}

// TestAuditShape_ServedFalseAdditive pins the audit-json additive
// contract: unserved services keep their RPC inventory but every entry
// carries served:false and mcp_callable:false; served services' entries
// omit the served key entirely.
func TestAuditShape_ServedFalseAdditive(t *testing.T) {
	dir := t.TempDir()
	yamlBody := `name: demo
module_path: github.com/example/demo
services:
  - name: api
    type: go_service
    path: handlers/api
  - name: project
    type: go_service
    path: handlers/project
    serve: false
    served_by: control-plane
`
	if err := os.WriteFile(filepath.Join(dir, "forge.yaml"), []byte(yamlBody), 0o644); err != nil {
		t.Fatalf("write forge.yaml: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "proto", "services"), 0o755); err != nil {
		t.Fatalf("mkdir proto/services: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "gen"), 0o755); err != nil {
		t.Fatalf("mkdir gen: %v", err)
	}
	descriptor := `{
  "services": [
    {"Name": "ApiService", "Package": "api.v1", "Methods": [
      {"Name": "Get", "InputType": "GetRequest", "OutputType": "GetResponse"}
    ]},
    {"Name": "ProjectService", "Package": "reliant.v1", "Methods": [
      {"Name": "CreateProject", "InputType": "CreateProjectRequest", "OutputType": "CreateProjectResponse"}
    ]}
  ]
}`
	if err := os.WriteFile(filepath.Join(dir, "gen", "forge_descriptor.json"), []byte(descriptor), 0o644); err != nil {
		t.Fatalf("write descriptor: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module github.com/example/demo\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}

	cfg, err := loadProjectConfigFrom(filepath.Join(dir, "forge.yaml"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cat := auditShape(cfg, dir)
	data, err := json.Marshal(cat.Details)
	if err != nil {
		t.Fatalf("marshal details: %v", err)
	}

	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal raw: %v", err)
	}
	services := raw["services"].([]any)
	if len(services) != 2 {
		t.Fatalf("services = %v, want 2 entries (unserved must NOT disappear)", services)
	}
	byName := map[string]map[string]any{}
	for _, s := range services {
		m := s.(map[string]any)
		byName[m["name"].(string)] = m
	}

	api := byName["api"]
	if api["served"] != true {
		t.Errorf("api.served = %v, want true", api["served"])
	}
	if _, present := api["served_by"]; present {
		t.Errorf("api.served_by must be omitted when unset")
	}
	apiRPC := api["rpcs"].([]any)[0].(map[string]any)
	if _, present := apiRPC["served"]; present {
		t.Errorf("served service's rpc entries must omit the served key (additive contract), got %v", apiRPC)
	}
	if apiRPC["mcp_callable"] != true {
		t.Errorf("api rpc mcp_callable = %v, want true", apiRPC["mcp_callable"])
	}

	project := byName["project"]
	if project["served"] != false {
		t.Errorf("project.served = %v, want false", project["served"])
	}
	if project["served_by"] != "control-plane" {
		t.Errorf("project.served_by = %v, want control-plane", project["served_by"])
	}
	projectRPCs := project["rpcs"].([]any)
	if len(projectRPCs) != 1 {
		t.Fatalf("project.rpcs = %v, want 1 entry (surface stays discoverable)", projectRPCs)
	}
	pRPC := projectRPCs[0].(map[string]any)
	if pRPC["served"] != false {
		t.Errorf("project rpc served = %v, want additive false", pRPC["served"])
	}
	if pRPC["mcp_callable"] != false {
		t.Errorf("project rpc mcp_callable = %v, want false (excluded from MCP manifest)", pRPC["mcp_callable"])
	}
}

// TestAuditCodegen_UnservedHandlerDirRetirementFinding pins the
// retirement flow's audit half: serve flipped to false while
// handlers/<svc>/ still exists → warn finding naming the dir.
func TestAuditCodegen_UnservedHandlerDirRetirementFinding(t *testing.T) {
	dir := t.TempDir()
	yamlBody := `name: demo
module_path: github.com/example/demo
services:
  - name: project
    type: go_service
    path: handlers/project
    serve: false
    served_by: control-plane
`
	if err := os.WriteFile(filepath.Join(dir, "forge.yaml"), []byte(yamlBody), 0o644); err != nil {
		t.Fatalf("write forge.yaml: %v", err)
	}
	// Pre-existing scaffold: the dir needs a parsable package clause for
	// the disk-first resolver to report FromDisk.
	handlerDir := filepath.Join(dir, "handlers", "project")
	if err := os.MkdirAll(handlerDir, 0o755); err != nil {
		t.Fatalf("mkdir handlers/project: %v", err)
	}
	if err := os.WriteFile(filepath.Join(handlerDir, "service.go"), []byte("package project\n"), 0o644); err != nil {
		t.Fatalf("write service.go: %v", err)
	}

	cfg, err := loadProjectConfigFrom(filepath.Join(dir, "forge.yaml"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	findings := unservedHandlerDirFindings(cfg, dir)
	if len(findings) != 1 {
		t.Fatalf("findings = %+v, want 1", findings)
	}
	f := findings[0]
	if f.Service != "project" || f.Dir != "handlers/project" || f.ServedBy != "control-plane" {
		t.Errorf("finding = %+v", f)
	}
	if !strings.Contains(f.Message, "serve=false") || !strings.Contains(f.Message, "restore serve: true") {
		t.Errorf("message must state the contradiction and both exits, got: %s", f.Message)
	}

	// The codegen category carries the finding additively and degrades
	// to warn.
	cat := auditCodegen(cfg, dir)
	if cat.Status != AuditStatusWarn {
		t.Errorf("codegen status = %s, want warn", cat.Status)
	}
	if _, ok := cat.Details["unserved_handler_dirs"]; !ok {
		t.Errorf("details missing unserved_handler_dirs: %v", cat.Details)
	}
	if !strings.Contains(cat.Summary, "unserved handler dir") {
		t.Errorf("summary must mention the retirement finding: %s", cat.Summary)
	}

	// Retired steady state: dir gone → finding clears.
	if err := os.RemoveAll(handlerDir); err != nil {
		t.Fatalf("remove handler dir: %v", err)
	}
	if got := unservedHandlerDirFindings(cfg, dir); len(got) != 0 {
		t.Errorf("findings after dir removal = %+v, want none", got)
	}
}

// TestCleanupStale_UnservedHandlerFilesBecomeCandidates pins the
// retirement flow's cleanup half: tracked Tier-1 files under a retired
// handlers dir (not re-written this run because the emitters are gated)
// are report-only candidates by default and deleted under
// --force-cleanup; Tier-2 user-owned files in the same dir are never
// candidates.
func TestCleanupStale_UnservedHandlerFilesBecomeCandidates(t *testing.T) {
	checksums.ResetPerRunState()
	defer checksums.ResetPerRunState()

	dir := t.TempDir()
	handlerDir := filepath.Join(dir, "handlers", "project")
	if err := os.MkdirAll(handlerDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	genBody := "// Code generated by forge. DO NOT EDIT.\npackage project\n"
	userBody := "package project\n// user-owned handler logic\n"
	if err := os.WriteFile(filepath.Join(handlerDir, "handlers_gen.go"), []byte(genBody), 0o644); err != nil {
		t.Fatalf("write handlers_gen.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(handlerDir, "handlers.go"), []byte(userBody), 0o644); err != nil {
		t.Fatalf("write handlers.go: %v", err)
	}

	cs := &generator.FileChecksums{Files: map[string]generator.FileChecksumEntry{
		// Tier-1: regenerated every run — now gated off, so stale.
		"handlers/project/handlers_gen.go": {Hash: "x", Tier: 1},
		// Tier-2: scaffold-once user-owned — never a candidate.
		"handlers/project/handlers.go": {Hash: "y", Tier: 2},
	}}
	ctx := &pipelineContext{
		ProjectDir:  dir,
		AbsPath:     dir,
		Checksums:   cs,
		HasServices: true, // owner-step gate for handlers paths
	}

	candidates, missing, err := cleanupStaleArtifacts(ctx)
	if err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if len(missing) != 0 {
		t.Errorf("missing = %v, want none", missing)
	}
	if len(candidates) != 1 || !strings.HasSuffix(candidates[0], filepath.Join("handlers", "project", "handlers_gen.go")) {
		t.Fatalf("candidates = %v, want exactly the Tier-1 handlers_gen.go", candidates)
	}
	// Report-only by default: the file survives.
	if _, statErr := os.Stat(filepath.Join(handlerDir, "handlers_gen.go")); statErr != nil {
		t.Errorf("default run must not delete: %v", statErr)
	}

	// --force-cleanup deletes the candidate and prunes the manifest, but
	// never touches the user-written Tier-2 file.
	ctx.ForceCleanup = true
	if _, _, err := cleanupStaleArtifacts(ctx); err != nil {
		t.Fatalf("force cleanup: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(handlerDir, "handlers_gen.go")); !os.IsNotExist(statErr) {
		t.Errorf("force-cleanup must delete handlers_gen.go, stat err = %v", statErr)
	}
	if _, statErr := os.Stat(filepath.Join(handlerDir, "handlers.go")); statErr != nil {
		t.Errorf("user-written handlers.go must survive force-cleanup: %v", statErr)
	}
	if _, tracked := cs.Files["handlers/project/handlers_gen.go"]; tracked {
		t.Errorf("manifest entry must be pruned after deletion")
	}
}
