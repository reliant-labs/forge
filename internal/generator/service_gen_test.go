package generator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGenerateServiceFilesCreatesExpectedFiles(t *testing.T) {
	root := t.TempDir()

	if err := GenerateServiceFiles(root, "example.com/myapp", "orders", "myapp", 8081); err != nil {
		t.Fatalf("GenerateServiceFiles() error = %v", err)
	}

	// Verify all expected files exist
	expectedFiles := []string{
		"handlers/orders/service.go",
		"handlers/orders/authorizer.go",
		"proto/services/orders/v1/orders.proto",
	}
	for _, f := range expectedFiles {
		path := filepath.Join(root, f)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected file %s to exist: %v", f, err)
		}
	}

	// handlers.go should NOT exist at scaffold (zero RPC methods).
	if _, err := os.Stat(filepath.Join(root, "handlers", "orders", "handlers.go")); !os.IsNotExist(err) {
		t.Errorf("handlers.go should not be emitted at scaffold with zero RPC methods (got err=%v)", err)
	}
}

func TestGenerateServiceFilesServiceGoUsesTemplates(t *testing.T) {
	root := t.TempDir()

	if err := GenerateServiceFiles(root, "example.com/myapp", "orders", "myapp", 8081); err != nil {
		t.Fatalf("GenerateServiceFiles() error = %v", err)
	}

	content, err := os.ReadFile(filepath.Join(root, "handlers", "orders", "service.go"))
	if err != nil {
		t.Fatalf("ReadFile error = %v", err)
	}
	svc := string(content)

	// The template version includes the Unimplemented handler embedding
	if !strings.Contains(svc, "UnimplementedOrdersServiceHandler") {
		t.Errorf("service.go should embed UnimplementedOrdersServiceHandler, got:\n%s", svc)
	}
	// The template version uses the correct Connect import path
	if !strings.Contains(svc, "gen/services/orders/v1/ordersv1connect") {
		t.Errorf("service.go should import ordersv1connect package, got:\n%s", svc)
	}
	// Package name should follow template convention
	if !strings.HasPrefix(svc, "package orders") {
		t.Errorf("service.go should have package orders, got:\n%s", svc)
	}
}

func TestGenerateServiceFilesProtoSkipsExisting(t *testing.T) {
	root := t.TempDir()

	// Pre-create a proto file
	protoDir := filepath.Join(root, "proto", "services", "orders", "v1")
	if err := os.MkdirAll(protoDir, 0755); err != nil {
		t.Fatal(err)
	}
	existing := []byte("syntax = \"proto3\";\n// existing proto\n")
	if err := os.WriteFile(filepath.Join(protoDir, "orders.proto"), existing, 0644); err != nil {
		t.Fatal(err)
	}

	if err := GenerateServiceFiles(root, "example.com/myapp", "orders", "myapp", 8081); err != nil {
		t.Fatalf("GenerateServiceFiles() error = %v", err)
	}

	// Proto should not be overwritten
	content, err := os.ReadFile(filepath.Join(protoDir, "orders.proto"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "// existing proto") {
		t.Errorf("existing proto should not be overwritten, got:\n%s", string(content))
	}
}

func TestGenerateFrontendFilesCreatesExpectedFiles(t *testing.T) {
	root := t.TempDir()

	if err := GenerateFrontendFiles(root, "example.com/myapp", "myapp", "web", 8080, ""); err != nil {
		t.Fatalf("GenerateFrontendFiles() error = %v", err)
	}

	// At minimum, package.json should exist
	path := filepath.Join(root, "frontends", "web", "package.json")
	if _, err := os.Stat(path); err != nil {
		t.Errorf("expected package.json to exist: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "web") {
		t.Errorf("package.json should reference frontend name, got:\n%s", string(content))
	}
	if !strings.Contains(string(content), "lint:styles") {
		t.Errorf("package.json should include lint:styles, got:\n%s", string(content))
	}
	if !strings.Contains(string(content), "stylelint") {
		t.Errorf("package.json should include stylelint dependencies, got:\n%s", string(content))
	}
	if _, err := os.Stat(filepath.Join(root, "frontends", "web", "stylelint.config.mjs")); err != nil {
		t.Errorf("expected stylelint.config.mjs to exist: %v", err)
	}

	// A nested go.mod must exist so that `go test ./...` from the project
	// root skips this subtree (frontend node_modules may contain stray .go
	// files from transitive npm deps).
	goModPath := filepath.Join(root, "frontends", "web", "go.mod")
	goModBytes, err := os.ReadFile(goModPath)
	if err != nil {
		t.Fatalf("expected frontend go.mod to exist: %v", err)
	}
	if !strings.Contains(string(goModBytes), "module example.com/myapp/frontends/web") {
		t.Errorf("frontend go.mod should declare nested module path, got:\n%s", string(goModBytes))
	}
}

func TestGenerateFrontendFilesReactNative(t *testing.T) {
	root := t.TempDir()

	if err := GenerateFrontendFiles(root, "example.com/myapp", "myapp", "mobile-app", 8080, "mobile"); err != nil {
		t.Fatalf("GenerateFrontendFiles(mobile) error = %v", err)
	}

	feDir := filepath.Join(root, "frontends", "mobile-app")

	// Core files must exist
	for _, file := range []string{
		"package.json", "app.json", "tsconfig.json", "babel.config.js",
		".gitignore", ".env.local", "buf.gen.yaml",
		"src/lib/connect.ts", "src/lib/query-client.ts",
		"src/hooks/use-api-query.ts", "src/hooks/use-api-mutation.ts",
		"app/_layout.tsx", "app/index.tsx",
		"go.mod",
	} {
		if _, err := os.Stat(filepath.Join(feDir, file)); err != nil {
			t.Errorf("expected %s to exist: %v", file, err)
		}
	}

	// package.json should reference expo and the frontend name
	pkgJSON, err := os.ReadFile(filepath.Join(feDir, "package.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(pkgJSON), "mobile-app") {
		t.Error("package.json should contain frontend name")
	}
	if !strings.Contains(string(pkgJSON), "expo") {
		t.Error("package.json should contain expo dependency")
	}

	// .env.local should contain EXPO_PUBLIC_API_URL
	envContent, err := os.ReadFile(filepath.Join(feDir, ".env.local"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(envContent), "EXPO_PUBLIC_API_URL=http://localhost:8080") {
		t.Errorf(".env.local should contain EXPO_PUBLIC_API_URL, got:\n%s", string(envContent))
	}

	// connect.ts should use EXPO_PUBLIC_API_URL
	connectTS, err := os.ReadFile(filepath.Join(feDir, "src", "lib", "connect.ts"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(connectTS), "EXPO_PUBLIC_API_URL") {
		t.Error("connect.ts should reference EXPO_PUBLIC_API_URL")
	}

	// go.mod should declare nested module
	goModBytes, err := os.ReadFile(filepath.Join(feDir, "go.mod"))
	if err != nil {
		t.Fatalf("expected frontend go.mod to exist: %v", err)
	}
	if !strings.Contains(string(goModBytes), "module example.com/myapp/frontends/mobile-app") {
		t.Errorf("frontend go.mod should declare nested module path, got:\n%s", string(goModBytes))
	}
}
