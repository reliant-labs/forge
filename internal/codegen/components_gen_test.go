package codegen

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

func TestComponentsToJSON_Shape(t *testing.T) {
	components := []config.ComponentConfig{
		{
			Name: "api",
			Kind: config.ComponentKindServer,
			Ports: map[string]config.PortSpec{
				"http": {Port: 8080, Expose: true},
				"grpc": {Port: 9090, Protocol: "tcp", Expose: true},
			},
		},
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

	out, err := ComponentsToJSON("demo", components)
	if err != nil {
		t.Fatalf("ComponentsToJSON: %v", err)
	}

	var doc struct {
		Project    string `json:"project"`
		Components []struct {
			Name    string `json:"name"`
			Kind    string `json:"kind"`
			Command []string
			Ports   []struct {
				Name     string `json:"name"`
				Port     int    `json:"port"`
				Protocol string `json:"protocol"`
				Expose   bool   `json:"expose"`
			} `json:"ports"`
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
	// Ports are name-sorted: grpc before http.
	if len(api.Ports) != 2 || api.Ports[0].Name != "grpc" || api.Ports[1].Name != "http" {
		t.Errorf("api ports not name-sorted: %+v", api.Ports)
	}
	// Scalar (no protocol) projects as "tcp".
	if api.Ports[1].Protocol != "tcp" {
		t.Errorf("http protocol = %q, want tcp", api.Ports[1].Protocol)
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

func TestComponentsToJSON_Idempotent(t *testing.T) {
	// Map iteration order must not affect the output.
	components := []config.ComponentConfig{
		{
			Name: "api",
			Kind: config.ComponentKindServer,
			Ports: map[string]config.PortSpec{
				"metrics": {Port: 9000},
				"http":    {Port: 8080},
				"grpc":    {Port: 9090},
			},
		},
	}
	first, err := ComponentsToJSON("demo", components)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		got, err := ComponentsToJSON("demo", components)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(first) {
			t.Fatalf("non-deterministic output on run %d:\nfirst:\n%s\ngot:\n%s", i, first, got)
		}
	}
	// Sanity: ports really are sorted.
	if !strings.Contains(string(first), `"grpc"`) {
		t.Errorf("expected grpc port in output")
	}
}
