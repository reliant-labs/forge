// Tests for registration-in-code (the user-owned pkg/app/services.go
// row list): the registry parser + classification chokepoint, the MCP
// manifest gate, the audit surfaces (shape served:false additive marker
// + codegen unregistered_services finding), and the stale-cleanup
// retirement path. The full end-to-end flow (real `forge generate` on a
// scaffolded project) lives in serve_types_only_e2e_test.go behind the
// e2e build tag.
package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/generator"
)

// writeServiceRegistry drops a pkg/app/services.go into dir with the
// given body. The body mirrors the scaffold shape: api registered,
// project tombstoned (comment only), anything else unlisted.
func writeServiceRegistry(t *testing.T, dir, body string) {
	t.Helper()
	appDir := filepath.Join(dir, "pkg", "app")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatalf("mkdir pkg/app: %v", err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "services.go"), []byte(body), 0o644); err != nil {
		t.Fatalf("write services.go: %v", err)
	}
}

const registryFixture = `package app

import (
	"log/slog"

	"connectrpc.com/connect"

	"github.com/reliant-labs/forge/pkg/appkit"

	"github.com/example/demo/pkg/config"
)

// RegisteredServices lists what THIS binary serves.
func RegisteredServices(app *App, cfg *config.Config, logger *slog.Logger, devMode bool, opts ...connect.HandlerOption) []appkit.ServiceDef {
	return []appkit.ServiceDef{
		serviceRowAPI(app, cfg, logger, devMode, opts...),
		// project: types-only — served by control-plane
	}
}
`

func serveTestServiceDefs() []codegen.ServiceDef {
	return []codegen.ServiceDef{
		{Name: "ApiService", Package: "api.v1", Methods: []codegen.Method{
			{Name: "Get", InputType: "GetRequest", OutputType: "GetResponse"},
		}},
		{Name: "ProjectService", Package: "reliant.v1", Methods: []codegen.Method{
			{Name: "CreateProject", InputType: "CreateProjectRequest", OutputType: "CreateProjectResponse"},
			{Name: "GetProject", InputType: "GetProjectRequest", OutputType: "GetProjectResponse"},
		}},
		{Name: "LedgerService", Package: "ledger.v1", Methods: []codegen.Method{
			{Name: "Post", InputType: "PostRequest", OutputType: "PostResponse"},
		}},
	}
}

func TestServiceRegistry_Classification(t *testing.T) {
	dir := t.TempDir()
	writeServiceRegistry(t, dir, registryFixture)

	reg, err := loadServiceRegistry(dir)
	if err != nil {
		t.Fatalf("loadServiceRegistry: %v", err)
	}
	if !reg.Exists {
		t.Fatalf("registry must report Exists=true")
	}

	// Spelling-agnostic: proto, kebab/CLI, and snake forms all resolve.
	for _, spelling := range []string{"ApiService", "api", "API"} {
		if got := reg.state(spelling); got != registrationRegistered {
			t.Errorf("state(%q) = %v, want registered", spelling, got)
		}
	}
	for _, spelling := range []string{"ProjectService", "project"} {
		if got := reg.state(spelling); got != registrationTombstoned {
			t.Errorf("state(%q) = %v, want tombstoned (comment mention)", spelling, got)
		}
	}
	// Ledger appears nowhere — newly added.
	if got := reg.state("LedgerService"); got != registrationUnlisted {
		t.Errorf("state(LedgerService) = %v, want unlisted", got)
	}
}

func TestServiceRegistry_MissingFileServesEverything(t *testing.T) {
	reg, err := loadServiceRegistry(t.TempDir())
	if err != nil {
		t.Fatalf("loadServiceRegistry on empty dir: %v", err)
	}
	if reg.Exists {
		t.Fatalf("Exists must be false when pkg/app/services.go is absent")
	}
	for _, name := range []string{"ApiService", "anything-at-all"} {
		if !reg.registered(name) {
			t.Errorf("missing registry must fail open to registered for %q", name)
		}
	}
}

func TestServiceRegistry_ParseErrorIsLoud(t *testing.T) {
	dir := t.TempDir()
	writeServiceRegistry(t, dir, "package app\nfunc broken( {")
	if _, err := loadServiceRegistry(dir); err == nil || !strings.Contains(err.Error(), serviceRegistryRelPath) {
		t.Fatalf("parse failure must name the registration file, got err=%v", err)
	}

	// The pipeline accessor propagates (and memoizes) the failure.
	ctx := &pipelineContext{ProjectDir: dir}
	if _, err := ctx.serviceRegistry(); err == nil {
		t.Fatalf("ctx.serviceRegistry must propagate the parse error")
	}
	if _, err := ctx.rowServiceDefs(); err == nil {
		t.Fatalf("rowServiceDefs must propagate the parse error")
	}
}

func TestServiceRegistry_CollisionPrefixedRowResolves(t *testing.T) {
	dir := t.TempDir()
	// A cross-role collision (service "billing" + internal/billing)
	// renames the FieldName to SvcBilling; the registered detection must
	// still resolve the underlying service.
	writeServiceRegistry(t, dir, `package app

func RegisteredServices() {
	_ = serviceRowSvcBilling
}
`)
	reg, err := loadServiceRegistry(dir)
	if err != nil {
		t.Fatalf("loadServiceRegistry: %v", err)
	}
	if !reg.registered("billing") || !reg.registered("BillingService") {
		t.Errorf("serviceRowSvcBilling reference must register billing in all spellings")
	}
}

func TestSplitServiceDefs_AndViews(t *testing.T) {
	dir := t.TempDir()
	writeServiceRegistry(t, dir, registryFixture)
	ctx := &pipelineContext{ProjectDir: dir, Services: serveTestServiceDefs()}

	rows, err := ctx.rowServiceDefs()
	if err != nil {
		t.Fatalf("rowServiceDefs: %v", err)
	}
	// Registered (api) + unlisted/newly-added (ledger) get rows;
	// tombstoned (project) does not.
	if len(rows) != 2 || rows[0].Name != "ApiService" || rows[1].Name != "LedgerService" {
		t.Errorf("rows = %+v, want [ApiService LedgerService]", rows)
	}

	registered, err := ctx.registeredServiceDefs()
	if err != nil {
		t.Fatalf("registeredServiceDefs: %v", err)
	}
	if len(registered) != 1 || registered[0].Name != "ApiService" {
		t.Errorf("registered = %+v, want [ApiService]", registered)
	}

	skips, err := ctx.tombstonedHandlerDirSkips()
	if err != nil {
		t.Fatalf("tombstonedHandlerDirSkips: %v", err)
	}
	if !skips["project"] || len(skips) != 1 {
		t.Errorf("skips = %v, want {project:true}", skips)
	}
}

func TestAllServiceRuntimeNames(t *testing.T) {
	got := allServiceRuntimeNames([]codegen.ServiceDef{
		{Name: "AdminServerService"}, {Name: "ApiService"},
	})
	if len(got) != 2 || got[0] != "admin-server" || got[1] != "api" {
		t.Errorf("allServiceRuntimeNames = %v, want [admin-server api]", got)
	}
}

// TestStepMCPManifest_ExcludesUnregisteredRPCs drives the real
// stepMCPManifest against a synthetic pipeline context and asserts the
// emitted gen/mcp/manifest.json advertises only the registered
// service's tools — tombstoned AND unlisted services are both excluded
// (this binary serves neither).
func TestStepMCPManifest_ExcludesUnregisteredRPCs(t *testing.T) {
	dir := t.TempDir()
	writeServiceRegistry(t, dir, registryFixture)
	ctx := &pipelineContext{
		ProjectDir: dir,
		AbsPath:    dir,
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
		t.Fatalf("tools = %+v, want exactly the registered service's 1 RPC", manifest.Tools)
	}
	if manifest.Tools[0].Service != "ApiService" || manifest.Tools[0].Method != "Get" {
		t.Errorf("tools[0] = %+v, want ApiService/Get", manifest.Tools[0])
	}
	for _, absent := range []string{"ProjectService", "LedgerService"} {
		if strings.Contains(string(data), absent) {
			t.Errorf("manifest must not advertise unregistered %s:\n%s", absent, data)
		}
	}
}

// TestAuditShape_ServedFalseAdditive pins the audit-json additive
// contract: unregistered services keep their RPC inventory but every
// entry carries served:false and mcp_callable:false; registered
// services' entries omit the served key entirely.
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
	writeServiceRegistry(t, dir, registryFixture)

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
services:
  - name: project
    type: go_service
    path: handlers/project
  - name: ledger
    type: go_service
    path: handlers/ledger
`
	if err := os.WriteFile(filepath.Join(dir, "forge.yaml"), []byte(yamlBody), 0o644); err != nil {
		t.Fatalf("write forge.yaml: %v", err)
	}
	// Pre-existing scaffolds: the dirs need a parsable package clause
	// for the disk-first resolver to report FromDisk.
	for _, svc := range []string{"project", "ledger"} {
		handlerDir := filepath.Join(dir, "handlers", svc)
		if err := os.MkdirAll(handlerDir, 0o755); err != nil {
			t.Fatalf("mkdir handlers/%s: %v", svc, err)
		}
		if err := os.WriteFile(filepath.Join(handlerDir, "service.go"), []byte("package "+svc+"\n"), 0o644); err != nil {
			t.Fatalf("write service.go: %v", err)
		}
	}
	writeServiceRegistry(t, dir, registryFixture) // project tombstoned, ledger unlisted

	cfg, err := loadProjectConfigFrom(filepath.Join(dir, "forge.yaml"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}

	findings := unregisteredServiceFindings(cfg, dir)
	if len(findings) != 2 {
		t.Fatalf("findings = %+v, want 2", findings)
	}
	byService := map[string]auditUnregisteredService{}
	for _, f := range findings {
		byService[f.Service] = f
	}

	tomb := byService["project"]
	if tomb.State != "tombstoned" || tomb.Dir != "handlers/project" {
		t.Errorf("project finding = %+v", tomb)
	}
	if !strings.Contains(tomb.Message, "--force-cleanup") || !strings.Contains(tomb.Message, serviceRegistryRelPath) {
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
	cat := auditCodegen(cfg, dir)
	if cat.Status != AuditStatusWarn {
		t.Errorf("codegen status = %s, want warn", cat.Status)
	}
	if _, ok := cat.Details["unregistered_services"]; !ok {
		t.Errorf("details missing unregistered_services: %v", cat.Details)
	}
	if !strings.Contains(cat.Summary, "unregistered service") {
		t.Errorf("summary must mention the registration findings: %s", cat.Summary)
	}

	// Retired steady state: tombstoned dir gone → its finding clears.
	if err := os.RemoveAll(filepath.Join(dir, "handlers", "project")); err != nil {
		t.Fatalf("remove handler dir: %v", err)
	}
	after := unregisteredServiceFindings(cfg, dir)
	if len(after) != 1 || after[0].Service != "ledger" {
		t.Errorf("findings after dir removal = %+v, want just the unlisted ledger", after)
	}

	// No registration file at all (pre-migration tree) → no findings.
	if err := os.Remove(filepath.Join(dir, "pkg", "app", "services.go")); err != nil {
		t.Fatalf("remove services.go: %v", err)
	}
	if got := unregisteredServiceFindings(cfg, dir); len(got) != 0 {
		t.Errorf("findings without registry = %+v, want none", got)
	}
}

// TestCleanupStale_TombstonedHandlerFilesBecomeCandidates pins the
// retirement flow's cleanup half: tracked Tier-1 files under a retired
// handlers dir (not re-written this run because the emitters are gated)
// are report-only candidates by default and deleted under
// --force-cleanup; Tier-2 user-owned files in the same dir are never
// candidates.
func TestCleanupStale_TombstonedHandlerFilesBecomeCandidates(t *testing.T) {
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

// TestGenerateWebhookRoutes_UnregisteredServiceIsHardError pins the
// reworked validation rule: webhooks on a service without a serviceRow
// in pkg/app/services.go is a generate-time error naming the
// registration file (F1 enforced this as a forge.yaml LoadStrict rule;
// the yaml surface is gone).
func TestGenerateWebhookRoutes_UnregisteredServiceIsHardError(t *testing.T) {
	dir := t.TempDir()
	writeServiceRegistry(t, dir, registryFixture) // project tombstoned
	yamlBody := `name: demo
module_path: github.com/example/demo
services:
  - name: project
    type: go_service
    path: handlers/project
    webhooks:
      - name: stripe
`
	if err := os.WriteFile(filepath.Join(dir, "forge.yaml"), []byte(yamlBody), 0o644); err != nil {
		t.Fatalf("write forge.yaml: %v", err)
	}
	cfg, err := loadProjectConfigFrom(filepath.Join(dir, "forge.yaml"))
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	reg, err := loadServiceRegistry(dir)
	if err != nil {
		t.Fatalf("load registry: %v", err)
	}
	err = generateWebhookRoutes(cfg, reg, dir, nil)
	if err == nil {
		t.Fatalf("webhooks on an unregistered service must be a hard error")
	}
	for _, want := range []string{serviceRegistryRelPath, "webhooks", codegen.ServiceRowFuncName("project")} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error missing %q: %v", want, err)
		}
	}
}
