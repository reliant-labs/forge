package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/generator"
)

func TestRunPackageNew(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	defer func() { _ = os.Chdir(orig) }()

	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	// Write minimal forge.project.yaml
	configContent := `name: testproject
module_path: example.com/testproject
version: "0.1.0"
mode: full
services:
  - name: api
    type: GO_SERVICE
    path: handlers/api
    port: 8080
`
	if err := os.WriteFile("forge.project.yaml", []byte(configContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Run the command
	if err := runPackageNew(nil, []string{"cache"}); err != nil {
		t.Fatalf("runPackageNew() error = %v", err)
	}

	// Verify contract.go was created
	contractPath := filepath.Join("internal", "cache", "contract.go")
	contractContent, err := os.ReadFile(contractPath)
	if err != nil {
		t.Fatalf("contract.go not created: %v", err)
	}
	contract := string(contractContent)

	if !strings.Contains(contract, "package cache") {
		t.Errorf("contract.go missing 'package cache', got:\n%s", contract)
	}
	if !strings.Contains(contract, "type Service interface") {
		t.Errorf("contract.go missing Service interface, got:\n%s", contract)
	}

	// Verify service.go was created
	servicePath := filepath.Join("internal", "cache", "service.go")
	serviceContent, err := os.ReadFile(servicePath)
	if err != nil {
		t.Fatalf("service.go not created: %v", err)
	}
	svc := string(serviceContent)

	if !strings.Contains(svc, "package cache") {
		t.Errorf("service.go missing 'package cache', got:\n%s", svc)
	}
	if !strings.Contains(svc, "type service struct") {
		t.Errorf("service.go missing unexported service struct, got:\n%s", svc)
	}
	if !strings.Contains(svc, "func New(deps Deps) Service") {
		t.Errorf("service.go missing New constructor returning Service interface, got:\n%s", svc)
	}

	// Verify forge.project.yaml was updated
	cfg, err := generator.ReadProjectConfig("forge.project.yaml")
	if err != nil {
		t.Fatalf("ReadProjectConfig() error = %v", err)
	}
	if len(cfg.Packages) != 1 {
		t.Fatalf("expected 1 package in config, got %d", len(cfg.Packages))
	}
	if cfg.Packages[0].Name != "cache" {
		t.Errorf("expected package name 'cache', got %q", cfg.Packages[0].Name)
	}
}

func TestRunPackageNewValidatesName(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	defer func() { _ = os.Chdir(orig) }()

	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	configContent := `name: testproject
module_path: example.com/testproject
`
	if err := os.WriteFile("forge.project.yaml", []byte(configContent), 0644); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		wantErr bool
	}{
		{"cache", false},
		{"my_cache", false},
		{"cache2", false},
		{"Cache", true},       // uppercase
		{"my-cache", true},    // hyphen
		{"2cache", true},      // starts with digit
		{"", true},            // empty (caught by cobra ExactArgs, but regex won't match)
		{"my cache", true},    // space
		{"cache.v1", true},    // dot
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.name == "" {
				// Skip empty - cobra handles this before our code
				if !validGoPackageName.MatchString(tt.name) == tt.wantErr {
					return
				}
				return
			}
			got := validGoPackageName.MatchString(tt.name)
			if got == tt.wantErr {
				// got=true means valid, wantErr=true means should be invalid
				t.Errorf("validGoPackageName(%q) = %v, wantErr = %v", tt.name, got, tt.wantErr)
			}
		})
	}
}

func TestRunPackageNewRejectsDuplicate(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	defer func() { _ = os.Chdir(orig) }()

	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	configContent := `name: testproject
module_path: example.com/testproject
`
	if err := os.WriteFile("forge.project.yaml", []byte(configContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Create package first time — should succeed
	if err := runPackageNew(nil, []string{"cache"}); err != nil {
		t.Fatalf("first runPackageNew() error = %v", err)
	}

	// Create same package again — should fail (directory exists)
	if err := runPackageNew(nil, []string{"cache"}); err == nil {
		t.Fatal("expected error for duplicate package, got nil")
	}
}

func TestRunPackageNewMultiplePackages(t *testing.T) {
	dir := t.TempDir()
	orig, _ := os.Getwd()
	defer func() { _ = os.Chdir(orig) }()

	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}

	configContent := `name: testproject
module_path: example.com/testproject
`
	if err := os.WriteFile("forge.project.yaml", []byte(configContent), 0644); err != nil {
		t.Fatal(err)
	}

	// Create two packages
	for _, name := range []string{"cache", "notifications"} {
		if err := runPackageNew(nil, []string{name}); err != nil {
			t.Fatalf("runPackageNew(%q) error = %v", name, err)
		}
	}

	// Verify both exist in config
	cfg, err := generator.ReadProjectConfig("forge.project.yaml")
	if err != nil {
		t.Fatalf("ReadProjectConfig() error = %v", err)
	}
	if len(cfg.Packages) != 2 {
		t.Fatalf("expected 2 packages in config, got %d", len(cfg.Packages))
	}
	if cfg.Packages[0].Name != "cache" {
		t.Errorf("expected first package 'cache', got %q", cfg.Packages[0].Name)
	}
	if cfg.Packages[1].Name != "notifications" {
		t.Errorf("expected second package 'notifications', got %q", cfg.Packages[1].Name)
	}

	// Verify both directories exist
	for _, name := range []string{"cache", "notifications"} {
		contractPath := filepath.Join("internal", name, "contract.go")
		if _, err := os.Stat(contractPath); os.IsNotExist(err) {
			t.Errorf("expected %s to exist", contractPath)
		}
		servicePath := filepath.Join("internal", name, "service.go")
		if _, err := os.Stat(servicePath); os.IsNotExist(err) {
			t.Errorf("expected %s to exist", servicePath)
		}
	}
}