package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
)

func TestDirExists(t *testing.T) {
	dir := t.TempDir()

	if !dirExists(dir) {
		t.Errorf("dirExists(%q) = false, want true", dir)
	}

	if dirExists(filepath.Join(dir, "nonexistent")) {
		t.Error("dirExists for nonexistent path returned true")
	}

	f := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(f, []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	if dirExists(f) {
		t.Error("dirExists for a regular file returned true")
	}
}

func TestToServiceDir(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"EchoService", "handlers/echo"},
		{"UserService", "handlers/user"},
		{"NotificationService", "handlers/notification"},
		{"Foo", "handlers/foo"},
	}

	for _, tt := range tests {
		got := toServiceDir(tt.input)
		if got != tt.want {
			t.Errorf("toServiceDir(%q) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestIsPluginAvailable(t *testing.T) {
	if !isPluginAvailable("go") {
		t.Error("isPluginAvailable(\"go\") = false, expected true")
	}

	if isPluginAvailable("definitely-not-a-real-binary-xyz-123") {
		t.Error("isPluginAvailable returned true for non-existent binary")
	}
}

func TestWriteDefaultBufGenYaml(t *testing.T) {
	dir := t.TempDir()

	if err := writeDefaultBufGenYaml(dir); err != nil {
		t.Fatalf("writeDefaultBufGenYaml() error: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "buf.gen.yaml"))
	if err != nil {
		t.Fatalf("failed to read buf.gen.yaml: %v", err)
	}

	content := string(data)
	if len(content) == 0 {
		t.Fatal("buf.gen.yaml is empty")
	}

	for _, want := range []string{
		"buf.build/protocolbuffers/go",
		"buf.build/connectrpc/go",
		"paths=source_relative",
	} {
		if !contains(content, want) {
			t.Errorf("buf.gen.yaml missing %q", want)
		}
	}
}

func TestRunBufGenerateTypeScriptWritesWorkspaceRelativeConfig(t *testing.T) {
	dir := t.TempDir()

	feRelDir := filepath.Join("frontends", "web")
	absFeDir := filepath.Join(dir, feRelDir)
	if err := os.MkdirAll(absFeDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Create proto/services so --path flag is used
	if err := os.MkdirAll(filepath.Join(dir, "proto", "services"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Create proto/api to test that both --path flags are added
	if err := os.MkdirAll(filepath.Join(dir, "proto", "api"), 0o755); err != nil {
		t.Fatal(err)
	}
	stubPath := filepath.Join(dir, "buf")
	stubScript := "#!/bin/sh\npwd > buf.cwd\nprintf '%s' \"$*\" > buf.args\nexit 0\n"
	if err := os.WriteFile(stubPath, []byte(stubScript), 0o755); err != nil {
		t.Fatal(err)
	}
	pathEnv := os.Getenv("PATH")
	if err := os.Setenv("PATH", dir+string(os.PathListSeparator)+pathEnv); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Setenv("PATH", pathEnv) }()

	cfg := &config.ProjectConfig{ModulePath: "example.com/project"}
	fe := config.FrontendConfig{Name: "web", Type: "nextjs", Path: feRelDir}
	if err := runBufGenerateTypeScript(fe, cfg, dir); err != nil {
		t.Fatalf("runBufGenerateTypeScript() error = %v", err)
	}

	// buf.gen.yaml should use project-root-relative out: path and no inputs: directive
	content := readFileForTest(t, filepath.Join(absFeDir, "buf.gen.yaml"))
	if !strings.Contains(content, "out: frontends/web/src/gen") {
		t.Fatalf("expected TypeScript buf config out: to be project-root-relative, got:\n%s", content)
	}
	if strings.Contains(content, "inputs:") {
		t.Fatalf("expected TypeScript buf config not to use inputs: directive, got:\n%s", content)
	}

	// buf should run from project root, not frontend dir
	invocationDir := strings.TrimSpace(readFileForTest(t, filepath.Join(dir, "buf.cwd")))
	expectedInvocationDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(expected project dir) error = %v", err)
	}
	actualInvocationDir, err := filepath.EvalSymlinks(invocationDir)
	if err != nil {
		t.Fatalf("EvalSymlinks(actual invocation dir) error = %v", err)
	}
	if actualInvocationDir != expectedInvocationDir {
		t.Fatalf("expected buf generate to run from project root %q, got %q", expectedInvocationDir, actualInvocationDir)
	}

	// Check command uses --template with relative path and --path flags
	invocationArgs := readFileForTest(t, filepath.Join(dir, "buf.args"))
	expectedTemplate := filepath.Join("frontends", "web", "buf.gen.yaml")
	if !strings.Contains(invocationArgs, "--template "+expectedTemplate) {
		t.Fatalf("expected buf generate to use --template %s, got %q", expectedTemplate, invocationArgs)
	}
	if !strings.Contains(invocationArgs, "--path proto/services") {
		t.Fatalf("expected buf generate to use --path proto/services, got %q", invocationArgs)
	}
	if !strings.Contains(invocationArgs, "--path proto/api") {
		t.Fatalf("expected buf generate to use --path proto/api, got %q", invocationArgs)
	}
}

func TestRunOrmGenerateSkipsWhenProtoDBHasNoProtoFiles(t *testing.T) {
	dir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(dir, "proto", "db"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := runOrmGenerate(dir); err != nil {
		t.Fatalf("runOrmGenerate() error = %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "buf.gen.orm.yaml")); !os.IsNotExist(err) {
		t.Fatalf("expected temporary ORM config to be absent after skip, err = %v", err)
	}
}

func TestRunOrmGenerateUsesProtoDBPathAndCleansUpTempConfig(t *testing.T) {
	dir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(dir, "proto", "db", "v1"), 0o755); err != nil {
		t.Fatal(err)
	}
	protoSource := `syntax = "proto3";
package db.v1;
message Account {}
`
	if err := os.WriteFile(filepath.Join(dir, "proto", "db", "v1", "account.proto"), []byte(protoSource), 0o644); err != nil {
		t.Fatal(err)
	}

	bufStubPath := filepath.Join(dir, "buf")
	bufStubScript := "#!/bin/sh\npwd > buf.cwd\nprintf '%s' \"$*\" > buf.args\nif [ -f buf.gen.orm.yaml ]; then cp buf.gen.orm.yaml buf.gen.orm.yaml.captured; fi\nexit 0\n"
	if err := os.WriteFile(bufStubPath, []byte(bufStubScript), 0o755); err != nil {
		t.Fatal(err)
	}
	ormPluginPath := filepath.Join(dir, "protoc-gen-forge-orm")
	ormPluginScript := "#!/bin/sh\nexit 0\n"
	if err := os.WriteFile(ormPluginPath, []byte(ormPluginScript), 0o755); err != nil {
		t.Fatal(err)
	}
	pathEnv := os.Getenv("PATH")
	if err := os.Setenv("PATH", dir+string(os.PathListSeparator)+pathEnv); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Setenv("PATH", pathEnv) }()

	if err := runOrmGenerate(dir); err != nil {
		t.Fatalf("runOrmGenerate() error = %v", err)
	}

	invocationArgs := readFileForTest(t, filepath.Join(dir, "buf.args"))
	if !strings.Contains(invocationArgs, "generate --template buf.gen.orm.yaml --path proto/db") {
		t.Fatalf("expected buf generate to target proto/db with temp template, got %q", invocationArgs)
	}

	ormConfig := readFileForTest(t, filepath.Join(dir, "buf.gen.orm.yaml.captured"))
	for _, want := range []string{"version: v2", "local:", "protoc-gen-forge-orm", "out: gen", "paths=source_relative"} {
		if !strings.Contains(ormConfig, want) {
			t.Fatalf("expected ORM temp config to contain %q, got:\n%s", want, ormConfig)
		}
	}

	if _, err := os.Stat(filepath.Join(dir, "buf.gen.orm.yaml")); !os.IsNotExist(err) {
		t.Fatalf("expected temporary ORM config to be cleaned up, err = %v", err)
	}
}

func TestLoadProjectConfigFromNormalizesFrontendTypeCasing(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "forge.yaml")
	config := `name: sample
module_path: example.com/sample
frontends:
  - name: web
    type: NEXTJS
`
	if err := os.WriteFile(configPath, []byte(config), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadProjectConfigFrom(configPath)
	if err != nil {
		t.Fatalf("loadProjectConfigFrom() error = %v", err)
	}
	if cfg == nil {
		t.Fatal("expected config, got nil")
	}
	if len(cfg.Frontends) != 1 {
		t.Fatalf("expected one frontend, got %d", len(cfg.Frontends))
	}
	if cfg.Frontends[0].Type != "nextjs" {
		t.Fatalf("expected frontend type to be normalized to nextjs, got %q", cfg.Frontends[0].Type)
	}
	if cfg.Frontends[0].Path != filepath.Join("frontends", "web") {
		t.Fatalf("expected frontend path frontends/web, got %q", cfg.Frontends[0].Path)
	}
}

func TestShouldRunRootGoModTidySkipsWhenGeneratedImportsAreMissing(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/sample\n\ngo 1.23\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	serviceDir := filepath.Join(dir, "handlers", "api")
	if err := os.MkdirAll(serviceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	serviceSource := `package apiservice

import (
	pb "example.com/sample/gen/services/api/v1"
)

var _ = pb.File_services_api_v1_api_proto
`
	if err := os.WriteFile(filepath.Join(serviceDir, "service.go"), []byte(serviceSource), 0o644); err != nil {
		t.Fatal(err)
	}

	shouldRun, err := shouldRunRootGoModTidy(dir)
	if err != nil {
		t.Fatalf("shouldRunRootGoModTidy() error = %v", err)
	}
	if shouldRun {
		t.Fatal("expected root go mod tidy to be skipped until generated imports exist")
	}
}

func TestShouldRunRootGoModTidyAllowsWhenGeneratedImportsExist(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/sample\n\ngo 1.23\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	serviceDir := filepath.Join(dir, "handlers", "api")
	generatedDir := filepath.Join(dir, "gen", "services", "api", "v1")
	if err := os.MkdirAll(serviceDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(generatedDir, 0o755); err != nil {
		t.Fatal(err)
	}
	serviceSource := `package apiservice

import (
	pb "example.com/sample/gen/services/api/v1"
)

var _ = pb.File_services_api_v1_api_proto
`
	if err := os.WriteFile(filepath.Join(serviceDir, "service.go"), []byte(serviceSource), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(generatedDir, "api.pb.go"), []byte("package apiv1\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	shouldRun, err := shouldRunRootGoModTidy(dir)
	if err != nil {
		t.Fatalf("shouldRunRootGoModTidy() error = %v", err)
	}
	if !shouldRun {
		t.Fatal("expected root go mod tidy to run once generated imports exist")
	}
}

func TestWithForcedEnvReplacesExistingValue(t *testing.T) {
	env := []string{"PATH=/usr/bin", "NODE_ENV=development", "HOME=/tmp/home"}

	got := withForcedEnv(env, "NODE_ENV", "production")
	joined := strings.Join(got, "\n")
	if strings.Count(joined, "NODE_ENV=") != 1 {
		t.Fatalf("expected exactly one NODE_ENV entry, got:\n%s", joined)
	}
	if !strings.Contains(joined, "NODE_ENV=production") {
		t.Fatalf("expected NODE_ENV to be forced to production, got:\n%s", joined)
	}
	if !strings.Contains(joined, "PATH=/usr/bin") || !strings.Contains(joined, "HOME=/tmp/home") {
		t.Fatalf("expected unrelated env vars to be preserved, got:\n%s", joined)
	}
}

func TestWithForcedEnvAddsMissingValue(t *testing.T) {
	env := []string{"PATH=/usr/bin"}

	got := withForcedEnv(env, "NODE_ENV", "production")
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "NODE_ENV=production") {
		t.Fatalf("expected NODE_ENV to be added, got:\n%s", joined)
	}
	if !strings.Contains(joined, "PATH=/usr/bin") {
		t.Fatalf("expected existing env vars to be preserved, got:\n%s", joined)
	}
}

func TestBootstrapGeneratedCodeRunsGeneratePipelineInProjectDirectory(t *testing.T) {
	dir := t.TempDir()
	generator := generator.NewProjectGenerator("sample-app", dir, "example.com/sample-app")
	generator.ServiceName = "api"
	generator.FrontendName = "web"
	if err := generator.Generate(); err != nil {
		t.Fatalf("Generate() error = %v", err)
	}

	stubPath := filepath.Join(dir, "buf")
	stubScript := `#!/bin/sh
set -eu
mkdir -p gen/services/api/v1/apiv1connect
cat > gen/services/api/v1/api.pb.go <<'PBEOF'
package apiv1

var File_services_api_v1_api_proto = struct{}{}

type CreateRequest struct{}
type CreateResponse struct{}
type GetRequest struct{}
type GetResponse struct{}
type UpdateRequest struct{}
type UpdateResponse struct{}
type DeleteRequest struct{}
type DeleteResponse struct{}
type ListRequest struct{}
type ListResponse struct{}
PBEOF
cat > gen/services/api/v1/apiv1connect/api.connect.go <<'CONNECTEOF'
package apiv1connect

import (
	"net/http"

	"connectrpc.com/connect"
)

type UnimplementedAPIServiceHandler struct{}

type APIServiceHandler interface{}

type APIServiceClient interface{}

func NewAPIServiceHandler(_ any, _ ...connect.HandlerOption) (string, http.Handler) {
	return "/api.v1.APIService/", http.NotFoundHandler()
}

func NewAPIServiceClient(_ connect.HTTPClient, _ string, _ ...connect.ClientOption) APIServiceClient {
	return nil
}
CONNECTEOF
exit 0
`
	if err := os.WriteFile(stubPath, []byte(stubScript), 0o755); err != nil {
		t.Fatal(err)
	}

	pathEnv := os.Getenv("PATH")
	if err := os.Setenv("PATH", dir+string(os.PathListSeparator)+pathEnv); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Setenv("PATH", pathEnv) }()

	if err := bootstrapGeneratedCode(dir); err != nil {
		t.Fatalf("bootstrapGeneratedCode() error = %v", err)
	}

	assertPath := filepath.Join(dir, "gen", "services", "api", "v1", "api.pb.go")
	if _, err := os.Stat(assertPath); err != nil {
		t.Fatalf("expected generated Go stub at %s: %v", assertPath, err)
	}
	connectPath := filepath.Join(dir, "gen", "services", "api", "v1", "apiv1connect", "api.connect.go")
	if _, err := os.Stat(connectPath); err != nil {
		t.Fatalf("expected generated Connect stub at %s: %v", connectPath, err)
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func readFileForTest(t *testing.T, path string) string {
	t.Helper()
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", path, err)
	}
	return string(contents)
}