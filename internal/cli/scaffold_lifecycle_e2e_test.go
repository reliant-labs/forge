//go:build e2e

package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestE2EScaffoldLifecycle exercises the full forge scaffold lifecycle:
//
//  1. `forge new myproject --mod ... --service api --frontend web`
//  2. Copy proto fixtures (entities + config annotations) into the project
//  3. `forge generate`
//  4. Assert: descriptor JSON, ORM files, handlers, migrations, mocks, config
//  5. `go build ./...` (validates the generated code compiles)
//
// The fixture protos in testdata/lifecycle/ exercise:
//   - 3 entity messages (Organization, User, Task) with forge.v1.entity/field
//   - tenant keys, soft-delete, foreign keys, nullable wrappers
//   - a config proto with ConfigFieldOptions
//   - a full service with 8 RPCs
func TestE2EScaffoldLifecycle(t *testing.T) {
	forgeBin := buildforgeBinary(t)
	dir := t.TempDir()

	// ── Step 1: scaffold the project ─────────────────────────────────────
	runCmd(t, dir, forgeBin,
		"new", "myproject",
		"--mod", "github.com/test/myproject",
		"--service", "api",
		"--frontend", "web",
	)

	projectDir := filepath.Join(dir, "myproject")
	assertPathExistsE2E(t, filepath.Join(projectDir, "forge.yaml"))
	assertPathExistsE2E(t, filepath.Join(projectDir, "go.mod"))

	// ── Step 2: copy test fixture protos ─────────────────────────────────
	// Replace the scaffold-generated api.proto with our fixture that has entities + CRUD RPCs.
	fixtureDir := filepath.Join(findRepoRoot(t), "internal", "cli", "testdata", "lifecycle")
	copyFixtureProto(t,
		filepath.Join(fixtureDir, "proto", "services", "api", "v1", "api.proto"),
		filepath.Join(projectDir, "proto", "services", "api", "v1", "api.proto"),
	)
	// Copy config proto fixture.
	configDst := filepath.Join(projectDir, "proto", "config", "v1")
	if err := os.MkdirAll(configDst, 0o755); err != nil {
		t.Fatalf("mkdir config dir: %v", err)
	}
	copyFixtureProto(t,
		filepath.Join(fixtureDir, "proto", "config", "v1", "config.proto"),
		filepath.Join(configDst, "config.proto"),
	)

	// ── Step 3: run forge generate ───────────────────────────────────────
	runCmd(t, projectDir, forgeBin, "generate")

	// ── Step 4: assertions ───────────────────────────────────────────────

	// 4a. forge_descriptor.json exists and has correct structure
	t.Run("descriptor", func(t *testing.T) {
		descPath := filepath.Join(projectDir, "gen", "forge_descriptor.json")
		assertPathExistsE2E(t, descPath)

		data, err := os.ReadFile(descPath)
		if err != nil {
			t.Fatalf("read descriptor: %v", err)
		}

		var desc struct {
			Services []struct {
				Name      string `json:"Name"`
				PkgName   string `json:"PkgName"`
				GoPackage string `json:"GoPackage"`
				Methods   []struct {
					Name string `json:"Name"`
				} `json:"Methods"`
			} `json:"services"`
			Entities []struct {
				Name      string `json:"Name"`
				TableName string `json:"TableName"`
			} `json:"entities"`
			Configs []struct {
				Name   string `json:"Name"`
				Fields []struct {
					Name string `json:"Name"`
				} `json:"Fields"`
			} `json:"configs"`
		}
		if err := json.Unmarshal(data, &desc); err != nil {
			t.Fatalf("parse descriptor JSON: %v", err)
		}

		// Services
		if len(desc.Services) < 1 {
			t.Fatalf("expected at least 1 service in descriptor, got %d", len(desc.Services))
		}
		foundAPI := false
		for _, svc := range desc.Services {
			if svc.Name == "APIService" {
				foundAPI = true
				if svc.PkgName == "" {
					t.Error("PkgName is empty for APIService — descriptor extraction bug")
				}
				if len(svc.Methods) < 8 {
					t.Errorf("expected at least 8 methods on APIService, got %d", len(svc.Methods))
				}
			}
		}
		if !foundAPI {
			t.Error("APIService not found in descriptor services")
		}

		// Entities (3: Organization, User, Task)
		if len(desc.Entities) < 3 {
			t.Errorf("expected at least 3 entities in descriptor, got %d", len(desc.Entities))
		}
		entityNames := make(map[string]bool)
		for _, e := range desc.Entities {
			entityNames[e.Name] = true
		}
		for _, want := range []string{"Organization", "User", "Task"} {
			if !entityNames[want] {
				t.Errorf("entity %q not found in descriptor", want)
			}
		}

		// Configs (1: AppConfig with 3 fields)
		if len(desc.Configs) < 1 {
			t.Errorf("expected at least 1 config message in descriptor, got %d", len(desc.Configs))
		} else {
			found := false
			for _, cfg := range desc.Configs {
				if cfg.Name == "AppConfig" {
					found = true
					if len(cfg.Fields) < 3 {
						t.Errorf("expected at least 3 config fields in AppConfig, got %d", len(cfg.Fields))
					}
				}
			}
			if !found {
				t.Error("AppConfig not found in descriptor configs")
			}
		}
	})

	// 4b. ORM files exist in internal/db/ for each entity
	t.Run("orm_files", func(t *testing.T) {
		dbDir := filepath.Join(projectDir, "internal", "db")
		if _, err := os.Stat(dbDir); os.IsNotExist(err) {
			t.Fatal("internal/db/ directory does not exist")
		}

		// Check that at least some .go files exist in internal/db/
		entries, err := os.ReadDir(dbDir)
		if err != nil {
			t.Fatalf("read internal/db/: %v", err)
		}
		goFiles := 0
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
				goFiles++
			}
		}
		if goFiles == 0 {
			t.Fatal("no .go files found in internal/db/")
		}
	})

	// 4c. Handler files exist
	t.Run("handlers", func(t *testing.T) {
		handlersDir := filepath.Join(projectDir, "handlers", "api")
		assertPathExistsE2E(t, filepath.Join(handlersDir, "service.go"))

		// handlers_gen.go is optional — forge skips it when all methods already
		// have implementations in service.go from the initial scaffold.

		// handlers_crud_gen.go should exist (CRUD implementations from entity matching)
		assertPathExistsE2E(t, filepath.Join(handlersDir, "handlers_crud_gen.go"))
	})

	// 4d. Mock files exist with correct imports
	t.Run("mocks", func(t *testing.T) {
		mockPath := filepath.Join(projectDir, "handlers", "mocks", "api_mock.go")
		assertPathExistsE2E(t, mockPath)

		mockContent := readFileE2E(t, mockPath)

		// Must import "apiv1connect" (not "/v1/connect")
		if !strings.Contains(mockContent, "apiv1connect") {
			t.Error("mock does not import apiv1connect")
		}
		if strings.Contains(mockContent, `"/v1/connect"`) {
			t.Error("mock has incorrect import path /v1/connect instead of apiv1connect")
		}

		// Must reference APIServiceMock
		if !strings.Contains(mockContent, "APIServiceMock") {
			t.Error("mock does not define APIServiceMock")
		}
	})

	// 4e. Migrations exist
	t.Run("migrations", func(t *testing.T) {
		migDir := filepath.Join(projectDir, "db", "migrations")
		if _, err := os.Stat(migDir); os.IsNotExist(err) {
			t.Fatal("db/migrations/ directory does not exist")
		}

		entries, err := os.ReadDir(migDir)
		if err != nil {
			t.Fatalf("read migrations dir: %v", err)
		}
		sqlFiles := 0
		for _, e := range entries {
			if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
				sqlFiles++
			}
		}
		if sqlFiles == 0 {
			t.Fatal("no .sql migration files found in db/migrations/")
		}
	})

	// 4f. Config loader generated from config proto annotations
	t.Run("config_loader", func(t *testing.T) {
		configPath := filepath.Join(projectDir, "pkg", "config", "config.go")
		assertPathExistsE2E(t, configPath)
	})

	// 4g. Bootstrap exists and references the api service
	t.Run("bootstrap", func(t *testing.T) {
		bootstrapPath := filepath.Join(projectDir, "pkg", "app", "bootstrap.go")
		assertPathExistsE2E(t, bootstrapPath)

		content := readFileE2E(t, bootstrapPath)
		if !strings.Contains(content, "api") {
			t.Error("bootstrap.go does not reference the api service")
		}
	})

	// 4h. Generated proto stubs exist
	t.Run("proto_stubs", func(t *testing.T) {
		assertPathExistsE2E(t, filepath.Join(projectDir, "gen", "services", "api", "v1"))
	})

	// 4i. Core UI components are installed in the frontend
	t.Run("components", func(t *testing.T) {
		componentsDir := filepath.Join(projectDir, "frontends", "web", "src", "components", "ui")
		for _, name := range []string{
			"sidebar_layout", "page_header", "badge", "modal",
			"skeleton_loader", "pagination", "search_input",
			"alert_banner", "toast_notification", "key_value_list",
		} {
			assertPathExistsE2E(t, filepath.Join(componentsDir, name+".tsx"))
		}
	})

	// ── Step 5: go build (validates everything compiles) ─────────────────
	t.Run("build", func(t *testing.T) {
		// go mod tidy first (needed after generate adds new imports)
		runCmd(t, projectDir, "go", "mod", "tidy")
		runCmd(t, filepath.Join(projectDir, "gen"), "go", "mod", "tidy")

		// Add replace directive for forge ORM package (in-repo)
		addforgeReplace(t, filepath.Join(projectDir, "gen"))

		runCmd(t, filepath.Join(projectDir, "gen"), "go", "mod", "tidy")

		runCmd(t, projectDir, "go", "build", "./...")
	})
}

// copyFixtureProto copies a proto file from src to dst, creating parent dirs.
func copyFixtureProto(t *testing.T, src, dst string) {
	t.Helper()
	data, err := os.ReadFile(src)
	if err != nil {
		t.Fatalf("read fixture %s: %v", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", dst, err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		t.Fatalf("write fixture %s: %v", dst, err)
	}
}