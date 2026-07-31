package codegen

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

func TestComponentsToJSON_Shape(t *testing.T) {
	components := []config.ComponentConfig{
		{Name: "api", Kind: config.ComponentKindServer},
		{Name: "reaper", Kind: config.ComponentKindCron, Schedule: "@hourly"},
		{Name: "sync", Kind: config.ComponentKindWorker},
		{Name: "proxy", Kind: config.ComponentKindBinary},
		{
			Name:    "controller",
			Kind:    config.ComponentKindOperator,
			Group:   "reliant.dev",
			Version: "v1alpha1",
			CRDs:    []config.CRDConfig{{Name: "Workspace"}},
		},
	}

	out, err := ComponentsToJSON("demo", components, nil)
	if err != nil {
		t.Fatalf("ComponentsToJSON: %v", err)
	}

	var doc struct {
		Project    string `json:"project"`
		Components []struct {
			Name     string `json:"name"`
			Kind     string `json:"kind"`
			Command  []string
			Schedule string   `json:"schedule"`
			Group    string   `json:"group"`
			Version  string   `json:"version"`
			CRDs     []string `json:"crds"`
			Build    struct {
				Type       string `json:"type"`
				Cmd        string `json:"cmd"`
				OutputName string `json:"output_name"`
			} `json:"build"`
		} `json:"components"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}

	if doc.Project != "demo" {
		t.Errorf("project = %q, want demo", doc.Project)
	}
	if len(doc.Components) != 5 {
		t.Fatalf("got %d components, want 5", len(doc.Components))
	}

	api := doc.Components[0]
	if api.Name != "api" || api.Kind != "server" {
		t.Errorf("component[0] = %+v", api)
	}

	// Binary carries its OWN entrypoint command: it lives at
	// cmd/<binpkg>/main.go and the image builds it to /app/<binpkg>, so the
	// deploy command is ["/app/<binpkg>"] — NOT a `<project> <name>` cobra
	// subcommand of the server binary.
	proxy := doc.Components[3]
	if proxy.Kind != "binary" {
		t.Fatalf("component[3] kind = %q", proxy.Kind)
	}
	if len(proxy.Command) != 1 || proxy.Command[0] != "/app/proxy" {
		t.Errorf("binary command = %v, want [/app/proxy]", proxy.Command)
	}
	// A binary builds its OWN cmd/<binpkg> package via a GoBuild.
	if proxy.Build.Type != "go" || proxy.Build.Cmd != "./cmd/proxy" || proxy.Build.OutputName != "proxy" {
		t.Errorf("binary build = %+v, want {go ./cmd/proxy proxy}", proxy.Build)
	}
	// A server builds the SHARED project binary (./cmd/<project>) and
	// selects its behavior via a cobra subcommand at runtime.
	if api.Build.Type != "go" || api.Build.Cmd != "./cmd/demo" || api.Build.OutputName != "demo" {
		t.Errorf("server build = %+v, want {go ./cmd/demo demo}", api.Build)
	}

	// Non-binary components carry no command (KCL fills the entrypoint).
	if len(doc.Components[2].Command) != 0 {
		t.Errorf("worker command = %v, want empty", doc.Components[2].Command)
	}

	op := doc.Components[4]
	if op.Group != "reliant.dev" || op.Version != "v1alpha1" || len(op.CRDs) != 1 || op.CRDs[0] != "Workspace" {
		t.Errorf("operator projection = %+v", op)
	}
}

// TestComponentsToJSON_HyphenatedProjectPrimaryBuildPath pins the F5 fix: the
// primary (server/worker/cron/operator) GoBuild default targets the RAW
// project name (`./cmd/peptide-platform`), matching the scaffold's cmd/ dir
// and the generated Dockerfile — not the sanitized `./cmd/peptide_platform`
// that stranded every hyphenated project into hand-overriding GoBuild.cmd.
// Secondary binaries still sanitize (a Go-package dir).
func TestComponentsToJSON_HyphenatedProjectPrimaryBuildPath(t *testing.T) {
	components := []config.ComponentConfig{
		{Name: "api", Kind: config.ComponentKindServer},
		{Name: "peptide-proxy", Kind: config.ComponentKindBinary},
	}
	out, err := ComponentsToJSON("peptide-platform", components, nil)
	if err != nil {
		t.Fatalf("ComponentsToJSON: %v", err)
	}

	var doc struct {
		Components []struct {
			Kind  string `json:"kind"`
			Build struct {
				Cmd        string `json:"cmd"`
				OutputName string `json:"output_name"`
			} `json:"build"`
		} `json:"components"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}

	primary := doc.Components[0]
	if primary.Build.Cmd != "./cmd/peptide-platform" || primary.Build.OutputName != "peptide-platform" {
		t.Errorf("primary build = {%q %q}, want {./cmd/peptide-platform peptide-platform} (raw project name)",
			primary.Build.Cmd, primary.Build.OutputName)
	}

	secondary := doc.Components[1]
	if secondary.Build.Cmd != "./cmd/peptide_proxy" || secondary.Build.OutputName != "peptide_proxy" {
		t.Errorf("secondary binary build = {%q %q}, want {./cmd/peptide_proxy peptide_proxy} (Go-package-safe)",
			secondary.Build.Cmd, secondary.Build.OutputName)
	}
}

// TestComponentsToJSON_NoPortsKey pins the shape decision: components_gen.json
// carries NO `ports` key at all.
//
// A port is a DEPLOY fact. Nothing forge discovers from the tree — the proto
// descriptor, the owned worker/operator files, cmd/ — states one, so every
// port forge could put here would be invented. It used to emit `"ports": []`
// for every component, which read as "forge computed the ports and there are
// none" and silently sent every server through the KCL Server expansion's
// :8080 fallback. Ports are declared where the rest of the per-env deploy
// shape is: the component overlay in deploy/kcl/<env>/main.k.
func TestComponentsToJSON_NoPortsKey(t *testing.T) {
	components := []config.ComponentConfig{
		{Name: "api", Kind: config.ComponentKindServer},
		{Name: "sync", Kind: config.ComponentKindWorker},
	}
	out, err := ComponentsToJSON("demo", components, nil)
	if err != nil {
		t.Fatalf("ComponentsToJSON: %v", err)
	}
	if strings.Contains(string(out), `"ports"`) {
		t.Errorf("components_gen.json still carries a `ports` key — nothing populates it:\n%s", out)
	}

	var doc struct {
		Components []map[string]any `json:"components"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	for _, c := range doc.Components {
		if _, ok := c["ports"]; ok {
			t.Errorf("component %v carries a ports key", c["name"])
		}
	}
}

func TestComponentsToJSON_Idempotent(t *testing.T) {
	// Map iteration order (env) must not affect the output.
	components := []config.ComponentConfig{
		{Name: "api", Kind: config.ComponentKindServer},
		{Name: "proxy", Kind: config.ComponentKindBinary},
	}
	first, err := ComponentsToJSON("demo", components, nil)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		got, err := ComponentsToJSON("demo", components, nil)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(first) {
			t.Fatalf("non-deterministic output on run %d:\nfirst:\n%s\ngot:\n%s", i, first, got)
		}
	}
}

// TestMigrateCommand: the deploy-time migration argv exists ONLY when the
// project ships .sql migrations, and it names the binary at the path the
// generated Dockerfile actually puts it (/app/<project>, WORKDIR /app).
//
// The empty case is the load-bearing one: a migration step that runs a
// command guaranteed to fail is worse than no step, so a project with no
// migrations must produce no command and therefore no init container.
func TestMigrateCommand(t *testing.T) {
	empty := t.TempDir()
	if got := MigrateCommand(empty, "demo"); got != nil {
		t.Errorf("MigrateCommand with no db/migrations = %v, want nil", got)
	}

	// A migrations DIRECTORY that holds no .sql is still "no migrations" —
	// a fresh scaffold ships db/migrations/.gitkeep.
	gitkeep := t.TempDir()
	mustWriteFile(t, filepath.Join(gitkeep, "db", "migrations", ".gitkeep"), "")
	if got := MigrateCommand(gitkeep, "demo"); got != nil {
		t.Errorf("MigrateCommand with only .gitkeep = %v, want nil", got)
	}

	withSQL := t.TempDir()
	mustWriteFile(t, filepath.Join(withSQL, "db", "migrations", "000001_init.up.sql"), "SELECT 1;")
	want := []string{"/app/demo", "db", "migrate", "up"}
	got := MigrateCommand(withSQL, "demo")
	if len(got) != len(want) {
		t.Fatalf("MigrateCommand = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("MigrateCommand = %v, want %v", got, want)
		}
	}
}

// TestComponentsToJSON_MigrateKeyAlwaysPresent: the `migrate` key is
// emitted even when empty, so the KCL reader (fc.load_migrate) sees a
// stable shape rather than having to probe for the key's existence.
func TestComponentsToJSON_MigrateKeyAlwaysPresent(t *testing.T) {
	out, err := ComponentsToJSON("demo", nil, nil)
	if err != nil {
		t.Fatalf("ComponentsToJSON: %v", err)
	}
	var doc struct {
		Migrate *struct {
			Command []string `json:"command"`
		} `json:"migrate"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if doc.Migrate == nil {
		t.Fatalf("components_gen.json omits the `migrate` key:\n%s", out)
	}
	if len(doc.Migrate.Command) != 0 {
		t.Errorf("migrate.command = %v, want empty", doc.Migrate.Command)
	}

	out, err = ComponentsToJSON("demo", nil, []string{"/app/demo", "db", "migrate", "up"})
	if err != nil {
		t.Fatalf("ComponentsToJSON: %v", err)
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, out)
	}
	if strings.Join(doc.Migrate.Command, " ") != "/app/demo db migrate up" {
		t.Errorf("migrate.command = %v", doc.Migrate.Command)
	}
}
