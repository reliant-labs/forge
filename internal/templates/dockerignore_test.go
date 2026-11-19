package templates

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/testdocker"
)

// Use Docker's actual context filtering, not an approximation of its glob
// semantics. FROM scratch avoids pulling an image or running a container.
func TestProjectDockerignoreExcludesNestedFrontendOutputs(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a scratch Docker image to exercise Docker's context filtering")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, "docker", "version", "--format", "{{.Server.Version}}").CombinedOutput(); err != nil {
		testdocker.Unavailable(t, "Docker daemon unavailable: %v\n%s", err, out)
	}
	root, output := t.TempDir(), t.TempDir()
	ignore, err := ProjectTemplates().Render(".dockerignore", nil)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{".dockerignore": string(ignore), "Dockerfile": "FROM scratch\nCOPY . /\n"}
	excluded := []string{
		"node_modules/root.js", "frontends/web/node_modules/next/index.js",
		"frontends/web/.next/cache/file", "frontends/web/.next-prod/server/file",
		"frontends/web/.turbo/cache/file", "frontends/web/out/index.html",
		"frontends/admin/dist/index.html", "frontends/web/coverage/lcov.info",
		"frontends/web/tsconfig.tsbuildinfo", "frontends/web/packages/widget/node_modules/dependency/index.js",
	}
	retained := []string{"go.mod", "cmd/api/main.go", "frontends/web/package.json", "frontends/web/package-lock.json", "frontends/web/src/app/page.tsx", "frontends/web/public/logo.svg"}
	for _, path := range append(append([]string{}, excluded...), retained...) {
		files[path] = "fixture\n"
	}
	for path, content := range files {
		full := filepath.Join(root, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.CommandContext(ctx, "docker", "build", "--output", "type=local,dest="+output, root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build scratch context: %v\n%s", err, out)
	}
	for _, path := range excluded {
		if _, err := os.Stat(filepath.Join(output, filepath.FromSlash(path))); !os.IsNotExist(err) {
			t.Errorf("frontend build output entered backend context: %s (stat: %v)", path, err)
		}
	}
	for _, path := range retained {
		if _, err := os.Stat(filepath.Join(output, filepath.FromSlash(path))); err != nil {
			t.Errorf("source must remain in backend context: %s: %v", path, err)
		}
	}
}
