package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
)

// TestGenerate_ConstructorMarkerScaffoldsMissingSeam is the regression for a
// generate that broke its own build: a hand-written component (here, one just
// converted from `//forge:exclude-contract` into an adapter) opts into the
// observability decorator with `// forge:constructor` but has no owned
// observe_chain.go. forge emitted middleware_gen.go calling newObserveChain,
// and generate's validate step failed on `undefined: newObserveChain`.
//
// The seam must be scaffolded alongside the decorator, and never overwrite
// one the user already owns.
func TestGenerate_ConstructorMarkerScaffoldsMissingSeam(t *testing.T) {
	// A RELATIVE project root, the way `forge generate` runs it (projectDir
	// "."): the seam scaffold once derived the module root by trimming the
	// package path, which only worked for an absolute root.
	t.Chdir(t.TempDir())
	root := "."
	mustWrite(t, filepath.Join(root, "go.mod"), "module example.com/proj\n\ngo 1.24\n")
	pkg := filepath.Join(root, "internal", "vendorapi")
	if err := os.MkdirAll(pkg, 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(pkg, "contract.go"), `// forge:outbound-io
package vendorapi

import "context"

type Service interface {
	Ping(ctx context.Context) error
}

type Deps struct{}
`)
	mustWrite(t, filepath.Join(pkg, "client.go"), `package vendorapi

import "context"

type client struct{}

// New builds the adapter.
//
// forge:constructor
func New(Deps) Service { return &client{} }

func (c *client) Ping(context.Context) error { return nil }
`)

	if err := generateInternalPackageContracts(root, &config.ProjectConfig{Name: "proj"}, nil); err != nil {
		t.Fatalf("generateInternalPackageContracts: %v", err)
	}

	decorator, err := os.ReadFile(filepath.Join(pkg, "middleware_gen.go"))
	if err != nil {
		t.Fatalf("decorator not generated: %v", err)
	}
	if !strings.Contains(string(decorator), "newObserveChain()") {
		t.Fatalf("decorator does not route through the seam:\n%s", decorator)
	}
	seam, err := os.ReadFile(filepath.Join(pkg, "observe_chain.go"))
	if err != nil {
		t.Fatalf("decorator emitted without the owned seam it calls: %v", err)
	}
	if !strings.Contains(string(seam), "func newObserveChain()") || !strings.Contains(string(seam), "package vendorapi") {
		t.Fatalf("scaffolded seam is not this package's newObserveChain:\n%s", seam)
	}

	// Once scaffolded, the seam is the user's: a second generate keeps edits.
	edited := strings.Replace(string(seam), "observe.RecoverMiddleware(logger),", "observe.RecoverMiddleware(logger), // mine", 1)
	mustWrite(t, filepath.Join(pkg, "observe_chain.go"), edited)
	if err := generateInternalPackageContracts(root, &config.ProjectConfig{Name: "proj"}, nil); err != nil {
		t.Fatalf("second generate: %v", err)
	}
	if got, _ := os.ReadFile(filepath.Join(pkg, "observe_chain.go")); string(got) != edited {
		t.Fatalf("second generate overwrote the owned seam")
	}
}
