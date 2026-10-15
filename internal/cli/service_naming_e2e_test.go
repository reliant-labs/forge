//go:build e2e

package cli

import (
	"path/filepath"
	"strings"
	"testing"
)

// Wire identifiers follow proto spelling; Forge's own helpers remain idiomatic
// Go. Exercise an existing owned service.go, not only a newly rendered template.
func TestE2EEquivalentServiceNamingGeneratedLint(t *testing.T) {
	t.Parallel()
	requireTool(t, "golangci-lint")
	bin := buildforgeBinary(t)
	contractDir := buildContractlintBinary(t)
	dir := t.TempDir()
	runCmd(t, dir, bin, "project", "new", "namingapp", "--mod", "example.com/namingapp", "--service", "api_key")
	root := filepath.Join(dir, "namingapp")
	addCorpusForgePkgReplace(t, root)
	proto := filepath.Join(root, "proto/services/api_key/v1/api_key.proto")
	source := strings.ReplaceAll(readFileE2E(t, proto), "APIKeyService", "ApiKeyService")
	source = strings.Replace(source, "  // TODO: Add your RPC methods here.", "  rpc CreateApiKey(CreateApiKeyRequest) returns (CreateApiKeyResponse);", 1)
	source += "\nmessage CreateApiKeyRequest {}\nmessage CreateApiKeyResponse {}\n"
	writeFileE2E(t, proto, source)
	runCmd(t, root, bin, "generate")
	runCmd(t, root, "go", "build", "./...")
	out, err := runLintE2E(t, root, contractDir, bin, "lint", "--no-fix")
	if err != nil {
		t.Fatalf("equivalent proto spelling must build and lint: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Running golangci-lint") {
		t.Fatalf("Go lint did not run:\n%s", out)
	}
}
