package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
)

// addforgeReplaceMain adds a `replace github.com/reliant-labs/forge/pkg => <repo>/pkg`
// directive to the project go.mod so `go mod tidy` resolves the in-repo
// pkg (auth, tenant, orm, etc.). Used in tests where the generated project
// references a forge/pkg subpackage not yet present in the latest published
// forge/pkg snapshot.
func addforgeReplaceMain(t *testing.T, projectDir string) {
	t.Helper()
	repoRoot := findForgeRepoRoot(t)
	goModPath := filepath.Join(projectDir, "go.mod")
	data, err := os.ReadFile(goModPath)
	if err != nil {
		t.Fatalf("read project go.mod: %v", err)
	}
	content := string(data)
	if strings.Contains(content, "replace github.com/reliant-labs/forge/pkg") {
		return
	}
	content += fmt.Sprintf("\nreplace github.com/reliant-labs/forge/pkg => %s/pkg\n", repoRoot)
	if err := os.WriteFile(goModPath, []byte(content), 0o644); err != nil {
		t.Fatalf("write project go.mod: %v", err)
	}
}

// findForgeRepoRoot walks up from cwd to find the forge repo root (looks for
// go.mod with module github.com/reliant-labs/forge).
func findForgeRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for i := 0; i < 8; i++ {
		modPath := filepath.Join(dir, "go.mod")
		if data, err := os.ReadFile(modPath); err == nil {
			if strings.Contains(string(data), "module github.com/reliant-labs/forge\n") {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatalf("could not find forge repo root from %s", dir)
	return ""
}

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

	// Default scaffold uses local plugins (no BSR auth required).
	// Past regression: defaulted to remote: plugins which forced
	// anonymous users to `buf registry login` before `forge generate`
	// would work reliably (rate-limit hits).
	for _, want := range []string{
		"local: protoc-gen-go",
		"local: protoc-gen-connect-go",
		"paths=source_relative",
	} {
		if !contains(content, want) {
			t.Errorf("buf.gen.yaml missing %q", want)
		}
	}
	// Negative assertion: must NOT contain remote: plugins as ACTIVE
	// list entries. We check by stripping comments and confirming no
	// non-comment line starts with `  - remote:`.
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "- remote:") {
			t.Errorf("buf.gen.yaml unexpectedly contains active BSR remote plugin entry %q (default should be local:)", line)
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
	// Create proto/services with a .proto file so the discoverProtoSubdirs
	// helper picks it up. Empty dirs are now filtered out — pack-emitted
	// proto trees are detected via .proto presence, so the canonical dirs
	// follow the same rule for symmetry.
	if err := os.MkdirAll(filepath.Join(dir, "proto", "services"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "proto", "services", "x.proto"), []byte("syntax=\"proto3\";\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Create proto/api with a .proto file so both --path flags are added.
	if err := os.MkdirAll(filepath.Join(dir, "proto", "api"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "proto", "api", "y.proto"), []byte("syntax=\"proto3\";\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stubPath := filepath.Join(dir, "buf")
	stubScript := "#!/bin/sh\npwd > buf.cwd\nprintf '%s' \"$*\" > buf.args\nexit 0\n"
	if err := os.WriteFile(stubPath, []byte(stubScript), 0o755); err != nil {
		t.Fatal(err)
	}
	// runBufGenerateTypeScript pre-flights the local TS plugin binary now
	// (since the BSR-removal switch). Drop a no-op stub at the path the
	// generated buf.gen.yaml references so the pre-flight passes and the
	// function proceeds to invoke `buf`.
	pluginBinDir := filepath.Join(absFeDir, "node_modules", ".bin")
	if err := os.MkdirAll(pluginBinDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pluginBinDir, "protoc-gen-es"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
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
	// Default scaffold uses the local protoc-gen-es plugin (no BSR auth).
	// Past regression: defaulted to `remote: buf.build/bufbuild/es` which
	// forced anonymous users to `buf registry login` to escape rate limits.
	if !strings.Contains(content, "local: ./frontends/web/node_modules/.bin/protoc-gen-es") {
		t.Fatalf("expected TypeScript buf config to use local: plugin, got:\n%s", content)
	}
	for _, line := range strings.Split(content, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		if strings.HasPrefix(trimmed, "- remote:") {
			t.Fatalf("frontend buf.gen.yaml unexpectedly contains active BSR remote plugin entry %q (default should be local:)", line)
		}
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
	ormPluginPath := filepath.Join(dir, "protoc-gen-forge")
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
	for _, want := range []string{"version: v2", "local:", "protoc-gen-forge", "out: gen", "mode=orm"} {
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

import "google.golang.org/protobuf/types/known/timestamppb"

var File_services_api_v1_api_proto = struct{}{}

type API struct {
	Id          string
	Name        string
	Description string
	Active      bool
	CreatedAt   *timestamppb.Timestamp
	UpdatedAt   *timestamppb.Timestamp
	DeletedAt   *timestamppb.Timestamp
}

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

	// Generated code (pkg/middleware/claims.go etc.) imports
	// github.com/reliant-labs/forge/pkg/auth which doesn't exist in the
	// last published forge/pkg snapshot. Point the project at the in-repo
	// pkg via a replace directive so `go mod tidy` resolves locally.
	addforgeReplaceMain(t, dir)

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

// TestForgeVersionMismatchWarning covers the three cases the version-pin
// machinery cares about: legacy (no forge_version), explicit mismatch,
// and explicit match. The "dev" binary case is intentionally silent so
// local forge development doesn't spam dogfood projects.
func TestForgeVersionMismatchWarning(t *testing.T) {
	tests := []struct {
		name        string
		yaml        string
		binary      string
		wantContain string // empty = expect no warning
	}{
		{
			"legacy project (no pin) gets baseline nudge",
			"",
			"1.6.0",
			"no forge_version declared",
		},
		{
			"explicit mismatch: bump from old to newer",
			"1.4.0",
			"1.6.0",
			"forge.yaml declares forge_version: 1.4.0 but binary is 1.6.0",
		},
		{
			"matched version is silent",
			"1.6.0",
			"1.6.0",
			"",
		},
		{
			"dev binary is silent (local forge work)",
			"1.6.0",
			"dev",
			"",
		},
		{
			"(devel) binary is silent",
			"1.6.0",
			"(devel)",
			"",
		},
		{
			"empty binary is silent (defensive)",
			"1.6.0",
			"",
			"",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := forgeVersionMismatchWarning(tt.yaml, tt.binary)
			if tt.wantContain == "" {
				if got != "" {
					t.Errorf("forgeVersionMismatchWarning(%q, %q) = %q, want empty",
						tt.yaml, tt.binary, got)
				}
				return
			}
			if !strings.Contains(got, tt.wantContain) {
				t.Errorf("forgeVersionMismatchWarning(%q, %q) = %q, want substring %q",
					tt.yaml, tt.binary, got, tt.wantContain)
			}
		})
	}
}

// TestRelevantMigrationSkills_FindsCanonicalContractkit verifies that
// the upgrade path discovery hits the canonical v0.x-to-contractkit
// skill for any 0.x project bumping forward. The skill ships in the
// embedded template tree.
func TestRelevantMigrationSkills_FindsCanonicalContractkit(t *testing.T) {
	skills := relevantMigrationSkills("0.5.0", "1.6.0")
	var found bool
	for _, s := range skills {
		if s.Path == "migration/v0.x-to-contractkit" {
			found = true
			if s.Description == "" {
				t.Errorf("skill %q has empty description (frontmatter unparsed?)", s.Path)
			}
			break
		}
	}
	if !found {
		var paths []string
		for _, s := range skills {
			paths = append(paths, s.Path)
		}
		t.Errorf("relevantMigrationSkills(0.5.0 → 1.6.0) did not include migration/v0.x-to-contractkit. Got: %v", paths)
	}
}

// TestRelevantMigrationSkills_LegacyTreatedAsSurfaceAll verifies that a
// legacy project (no forge_version) sees every per-version skill,
// including the contractkit canonical example.
func TestRelevantMigrationSkills_LegacyTreatedAsSurfaceAll(t *testing.T) {
	skills := relevantMigrationSkills("0.0.0", "1.6.0")
	if len(skills) == 0 {
		t.Fatal("expected at least one migration skill for legacy projects, got none")
	}
	var found bool
	for _, s := range skills {
		if s.Path == "migration/v0.x-to-contractkit" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("legacy 0.0.0 → latest path missing canonical migration/v0.x-to-contractkit skill")
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
