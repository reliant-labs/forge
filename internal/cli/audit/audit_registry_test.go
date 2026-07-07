package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/cli/audittype"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
)

// These tests exercise the shape / codegen registration-finding logic that
// the audit group computes over factory.AuditAPI's registration view. In
// package cli the real *serviceRegistry parses pkg/app/services.go; here a
// configurable stubRegistry supplies the same classification (api
// registered, project tombstoned, ledger unlisted) so the assertions on
// served-additivity and tombstoned-vs-unlisted findings are preserved
// without re-importing cli internals.

// TestAuditShape_ServedFalseAdditive pins the additive served:false marker:
// an unregistered Connect service stays in the inventory with served=false
// and its RPCs carry served:false / mcp_callable:false; a registered
// service omits the per-RPC served key.
func TestAuditShape_ServedFalseAdditive(t *testing.T) {
	dir := t.TempDir()
	yamlBody := `name: demo
module_path: github.com/example/demo
`
	if err := os.WriteFile(filepath.Join(dir, "forge.yaml"), []byte(yamlBody), 0o644); err != nil {
		t.Fatalf("write forge.yaml: %v", err)
	}
	writeComponentsJSONTest(t, dir,
		config.ComponentConfig{Name: "api", Kind: "server", Path: "internal/handlers/api"},
		config.ComponentConfig{Name: "project", Kind: "server", Path: "internal/handlers/project"},
	)
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

	cfg, err := generator.ReadProjectConfig(filepath.Join(dir, "forge.yaml"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	// api registered, project unlisted (unregistered).
	f := testFactory(auditAPIConfig{
		projectDefinesConnectServices: true,
		isConnectService:              func(config.ComponentConfig) bool { return true },
		registry:                      stubRegistry{exists: true, registered: map[string]bool{"api": true}},
	})
	cat := auditShape(f, cfg, dir)
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
		t.Fatalf("services = %v, want 2 entries (unregistered must NOT disappear)", services)
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
	apiRPC := api["rpcs"].([]any)[0].(map[string]any)
	if _, present := apiRPC["served"]; present {
		t.Errorf("registered service's rpc entries must omit the served key (additive contract), got %v", apiRPC)
	}
	if apiRPC["mcp_callable"] != true {
		t.Errorf("api rpc mcp_callable = %v, want true", apiRPC["mcp_callable"])
	}

	project := byName["project"]
	if project["served"] != false {
		t.Errorf("project.served = %v, want false", project["served"])
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

// TestAuditCodegen_UnregisteredServiceFindings pins both registration
// findings: a tombstoned service whose handlers/<svc>/ still exists
// (retirement half-done) and an unlisted service whose row constructor
// is generated but unreferenced (post add-service).
func TestAuditCodegen_UnregisteredServiceFindings(t *testing.T) {
	dir := t.TempDir()
	yamlBody := `name: demo
module_path: github.com/example/demo
`
	if err := os.WriteFile(filepath.Join(dir, "forge.yaml"), []byte(yamlBody), 0o644); err != nil {
		t.Fatalf("write forge.yaml: %v", err)
	}
	// The component inventory is enumerated from the proto descriptor now
	// (codegen.IntrospectComponents), not components.json. "ProjectService"
	// / "LedgerService" map to components project / ledger.
	descriptor := `{
  "services": [
    {"Name": "ProjectService", "Package": "reliant.v1"},
    {"Name": "LedgerService", "Package": "reliant.v1"}
  ]
}`
	if err := os.MkdirAll(filepath.Join(dir, "gen"), 0o755); err != nil {
		t.Fatalf("mkdir gen: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gen", "forge_descriptor.json"), []byte(descriptor), 0o644); err != nil {
		t.Fatalf("write descriptor: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module github.com/example/demo\n"), 0o644); err != nil {
		t.Fatalf("write go.mod: %v", err)
	}
	// Pre-existing scaffolds: the dirs need a parsable package clause
	// for the disk-first resolver to report FromDisk.
	for _, svc := range []string{"project", "ledger"} {
		handlerDir := filepath.Join(dir, "internal", "handlers", svc)
		if err := os.MkdirAll(handlerDir, 0o755); err != nil {
			t.Fatalf("mkdir handlers/%s: %v", svc, err)
		}
		if err := os.WriteFile(filepath.Join(handlerDir, "service.go"), []byte("package "+svc+"\n"), 0o644); err != nil {
			t.Fatalf("write service.go: %v", err)
		}
	}

	cfg, err := generator.ReadProjectConfig(filepath.Join(dir, "forge.yaml"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	// project tombstoned, ledger unlisted (the registryFixture shape).
	servesAll := func(config.ComponentConfig) bool { return true }
	f := testFactory(auditAPIConfig{
		isConnectService: servesAll,
		registry: stubRegistry{
			exists:     true,
			registered: map[string]bool{},
			tombstoned: map[string]bool{"project": true},
		},
	})

	findings := unregisteredServiceFindings(f, cfg, dir)
	if len(findings) != 2 {
		t.Fatalf("findings = %+v, want 2", findings)
	}
	byService := map[string]auditUnregisteredService{}
	for _, fnd := range findings {
		byService[fnd.Service] = fnd
	}

	tomb := byService["project"]
	if tomb.State != "tombstoned" || tomb.Dir != "internal/handlers/project" {
		t.Errorf("project finding = %+v", tomb)
	}
	if !strings.Contains(tomb.Message, "--force-cleanup") || !strings.Contains(tomb.Message, "pkg/app/services.go") {
		t.Errorf("tombstoned message must state both exits and name the registry: %s", tomb.Message)
	}

	unlisted := byService["ledger"]
	if unlisted.State != "unlisted" {
		t.Errorf("ledger finding = %+v", unlisted)
	}
	if !strings.Contains(unlisted.Message, "generated but unreferenced") || !strings.Contains(unlisted.Message, codegen.ServiceRowFuncName("ledger")) {
		t.Errorf("unlisted message must surface the unreferenced row constructor and the exact line: %s", unlisted.Message)
	}

	// The codegen category carries the findings additively and degrades
	// to warn.
	cat := auditCodegen(f, cfg, dir)
	if cat.Status != audittype.StatusWarn {
		t.Errorf("codegen status = %s, want warn", cat.Status)
	}
	if _, ok := cat.Details["unregistered_services"]; !ok {
		t.Errorf("details missing unregistered_services: %v", cat.Details)
	}
	if !strings.Contains(cat.Summary, "unregistered service") {
		t.Errorf("summary must mention the registration findings: %s", cat.Summary)
	}

	// Retired steady state: tombstoned dir gone → its finding clears.
	if err := os.RemoveAll(filepath.Join(dir, "internal", "handlers", "project")); err != nil {
		t.Fatalf("remove handler dir: %v", err)
	}
	after := unregisteredServiceFindings(f, cfg, dir)
	if len(after) != 1 || after[0].Service != "ledger" {
		t.Errorf("findings after dir removal = %+v, want just the unlisted ledger", after)
	}

	// No registration file at all (pre-migration tree) → no findings.
	noRegistry := testFactory(auditAPIConfig{
		isConnectService: servesAll,
		registry:         stubRegistry{exists: false},
	})
	if got := unregisteredServiceFindings(noRegistry, cfg, dir); len(got) != 0 {
		t.Errorf("findings without registry = %+v, want none", got)
	}
}
