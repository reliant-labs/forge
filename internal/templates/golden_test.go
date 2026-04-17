package templates

import (
	"flag"
	"os"
	"path/filepath"
	"testing"
)

// updateGolden re-writes every golden file from the current rendered output.
// Usage: `go test ./internal/templates/ -run TestGolden -update`.
// Kept as a package-level flag rather than a build tag so the happy path
// (compare) remains the default and no one accidentally blesses broken output.
var updateGolden = flag.Bool("update", false, "update .golden snapshots")

// goldenCase describes one template we snapshot for regression detection.
//
// Keep the data struct minimal and stable: every field you add becomes
// something a future template change might have to diff against. If a field
// isn't actually referenced by the template you're snapshotting, drop it.
type goldenCase struct {
	// name is the subtest name and the filename stem under testdata/golden/.
	name string

	// render produces the bytes to snapshot. It receives no args so each case
	// is responsible for picking a representative, deterministic input.
	render func(t *testing.T) []byte
}

// renderProject renders a project/ template with the standard templateData
// shape used by the generator (see internal/generator/project.go).
func renderProject(t *testing.T, name string, data any) []byte {
	t.Helper()
	out, err := ProjectTemplates.Render(name, data)
	if err != nil {
		t.Fatalf("ProjectTemplates.Render(%q) error = %v", name, err)
	}
	return out
}

// renderCI renders a CI workflow template.
func renderCI(t *testing.T, provider, name string, data any) []byte {
	t.Helper()
	out, err := CITemplates(provider).Render(name, data)
	if err != nil {
		t.Fatalf("CITemplates(%q).Render(%q) error = %v", provider, name, err)
	}
	return out
}

// renderService renders a service/ template.
func renderService(t *testing.T, name string, data any) []byte {
	t.Helper()
	out, err := ServiceTemplates.Render(name, data)
	if err != nil {
		t.Fatalf("ServiceTemplates.Render(%q) error = %v", name, err)
	}
	return out
}

// projectData returns the struct shape used by the project generator's
// top-level file loop (see project.go: template files block).
func projectData() any {
	return struct {
		Name                   string
		ProtoName              string
		Module                 string
		ServiceName            string
		ServicePort            int
		ProjectName            string
		FrontendName           string
		FrontendPort           int
		GoVersion              string
		GoVersionMinor         string
		DockerBuilderGoVersion string
		CLI                    string
	}{
		Name:                   "demo",
		ProtoName:              "demo",
		Module:                 "github.com/example/demo",
		ServiceName:            "api",
		ServicePort:            8080,
		ProjectName:            "demo",
		FrontendName:           "web",
		FrontendPort:           3000,
		GoVersion:              "1.25",
		GoVersionMinor:         "25",
		DockerBuilderGoVersion: "1.25",
		CLI:                    "forge",
	}
}

// TestGoldenSnapshots renders a curated set of templates with stable data
// and compares the output byte-for-byte to the corresponding file under
// testdata/golden/. When the output diverges — even by whitespace — the
// test fails and the maintainer must either fix the regression or rerun
// with `-update` to bless the new output.
//
// The goal is reviewability: a PR that changes CI, CORS, Dockerfile, or
// handler codegen output must show the diff in the snapshot file so it
// can't silently regress.
func TestGoldenSnapshots(t *testing.T) {
	cases := []goldenCase{
		{
			name: "Taskfile.yml",
			render: func(t *testing.T) []byte {
				return renderProject(t, "Taskfile.yml.tmpl", projectData())
			},
		},
		{
			name: "Dockerfile",
			render: func(t *testing.T) []byte {
				return renderProject(t, "Dockerfile.tmpl", projectData())
			},
		},
		{
			name: "middleware_cors.go",
			render: func(t *testing.T) []byte {
				// middleware-cors.go is static (no .tmpl suffix) — it
				// flows through ProjectTemplates.Render unchanged except
				// for the //go:build ignore strip. Snapshotting it here
				// guards against accidental edits to this security-
				// critical file.
				return renderProject(t, "middleware-cors.go", nil)
			},
		},
		{
			name: "reliant-forge.md",
			render: func(t *testing.T) []byte {
				return renderProject(t, "reliant-forge.md.tmpl", projectData())
			},
		},
		{
			name: "handlers_gen.go",
			render: func(t *testing.T) []byte {
				// A minimal, deterministic service with one unary and
				// one server-streaming method so the snapshot exercises
				// both code paths in the template.
				data := map[string]any{
					"ServiceName":         "api",
					"Module":              "github.com/example/demo",
					"ProtoImportPath":     "proto/services/api",
					"ProtoPackage":        "proto/services/api",
					"ProtoConnectPackage": "apiv1connect",
					"HandlerName":         "Service",
					"ProtoFileSymbol":     "File_services_api_v1_api_proto",
					"Methods": []map[string]any{
						{
							"Name":            "Echo",
							"InputType":       "EchoRequest",
							"OutputType":      "EchoResponse",
							"ClientStreaming": false,
							"ServerStreaming": false,
							"AuthRequired":    false,
						},
						{
							"Name":            "Tail",
							"InputType":       "TailRequest",
							"OutputType":      "TailResponse",
							"ClientStreaming": false,
							"ServerStreaming": true,
							"AuthRequired":    true,
						},
					},
				}
				return renderService(t, "handlers_gen.go.tmpl", data)
			},
		},
		{
			name: "ci_minimal.yml",
			render: func(t *testing.T) []byte {
				data := CIWorkflowData{
					ProjectName:  "demo",
					GoVersion:    "1.25",
					LintGolangci: true,
					TestRace:     true,
					PermContents: "read",
					Module:       "github.com/example/demo",
					Registry:     "ghcr",
					GithubOrg:    "example",
					ForgeVersion: "v0.0.0-test",
				}
				return renderCI(t, "github", "ci.yml.tmpl", data)
			},
		},
		{
			name: "ci_full.yml",
			render: func(t *testing.T) []byte {
				data := CIWorkflowData{
					ProjectName:  "demo",
					GoVersion:    "1.25",
					HasFrontends: true,
					Frontends: []FrontendCIConfig{
						{Name: "web", Path: "frontends/web"},
					},
					HasServices:  true,
					LintGolangci: true,
					LintBuf:      true,
					LintFrontend: true,
					TestRace:     true,
					TestCoverage: true,
					VulnGo:       true,
					VulnDocker:   true,
					VulnNPM:      true,
					E2EEnabled:   true,
					E2ERuntime:   "docker-compose",
					PermContents: "read",
					HasKCL:       true,
					Environments: []string{"dev", "staging", "prod"},
					Module:       "github.com/example/demo",
					Registry:     "ghcr",
					GithubOrg:    "example",
					FrontendName: "web",
					ForgeVersion: "v0.0.0-test",
				}
				return renderCI(t, "github", "ci.yml.tmpl", data)
			},
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := tc.render(t)
			goldenPath := filepath.Join("testdata", "golden", tc.name+".golden")

			if *updateGolden {
				if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
					t.Fatalf("mkdir golden dir: %v", err)
				}
				if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
					t.Fatalf("write golden %s: %v", goldenPath, err)
				}
				t.Logf("updated %s (%d bytes)", goldenPath, len(got))
				return
			}

			want, err := os.ReadFile(goldenPath)
			if err != nil {
				t.Fatalf("read golden %s: %v\nhint: run `go test ./internal/templates/ -run TestGoldenSnapshots -update` to create it",
					goldenPath, err)
			}

			if string(got) != string(want) {
				t.Fatalf("%s snapshot mismatch\n"+
					"hint: review the diff; if intentional, rerun with -update\n"+
					"want (%d bytes):\n%s\n\ngot (%d bytes):\n%s",
					tc.name, len(want), string(want), len(got), string(got))
			}
		})
	}
}