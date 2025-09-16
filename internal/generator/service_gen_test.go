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
		"handlers/orders/handlers.go",
		"handlers/orders/authorizer.go",
		"proto/services/orders/v1/orders.proto",
	}
	for _, f := range expectedFiles {
		path := filepath.Join(root, f)
		if _, err := os.Stat(path); err != nil {
			t.Errorf("expected file %s to exist: %v", f, err)
		}
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

	if err := GenerateFrontendFiles(root, "example.com/myapp", "myapp", "web", 8080); err != nil {
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
}