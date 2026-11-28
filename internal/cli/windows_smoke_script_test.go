package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Exercise the real smoke driver without scaffolding, downloading tools or
// loading KCL. In particular, git must run before the driver leaves its checkout.
func TestWindowsSmokeScriptForwardsCheckoutRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX shell fixture; the Windows job runs the real smoke")
	}
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash unavailable for shell fixture")
	}
	// Read the script in this process so edits invalidate Go's test cache.
	script, err := os.ReadFile(filepath.Join("..", "..", "scripts", "ci-windows-smoke.sh"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	checkout := filepath.Join(root, "forge checkout")
	bin := filepath.Join(root, "bin")
	work := filepath.Join(root, "smoke work")
	for _, dir := range []string{checkout, bin, work} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	driver := filepath.Join(checkout, "smoke.sh")
	write(driver, string(script))
	write(filepath.Join(bin, "git"), `#!/usr/bin/env bash
set -euo pipefail
[[ "$*" == 'rev-parse --show-toplevel' ]]
[[ "$PWD" == "$FIXTURE_CHECKOUT" ]]
printf '%s\n' "$PWD" > "$FIXTURE_LOG/git-cwd"
printf '%s\n' "$PWD"
`)
	write(filepath.Join(bin, "mktemp"), `#!/usr/bin/env bash
set -euo pipefail
[[ "$*" == '-d' ]]
printf '%s\n' "$FIXTURE_WORK"
`)
	write(filepath.Join(bin, "forge"), `#!/usr/bin/env bash
set -euo pipefail
case "$1 $2" in
  'project new')
    printf '%s\n' "$PWD" > "$FIXTURE_LOG/new-cwd"
    printf '%s\n' "$@" > "$FIXTURE_LOG/new-argv"
    mkdir smoke
    ;;
  'env render')
    printf '%s\n' "$PWD" > "$FIXTURE_LOG/render-cwd"
    printf '%s\n' "$@" > "$FIXTURE_LOG/render-argv"
    ;;
  *) exit 1 ;;
esac
`)
	// buf is present, so a go invocation would be an unexpected download.
	write(filepath.Join(bin, "buf"), "#!/usr/bin/env bash\nexit 0\n")
	write(filepath.Join(bin, "go"), "#!/usr/bin/env bash\nexit 1\n")
	write(filepath.Join(bin, "uname"), "#!/usr/bin/env bash\nprintf 'Linux\\n'\n")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bash, driver)
	cmd.Dir = checkout
	cmd.Env = append(os.Environ(),
		"PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"FIXTURE_CHECKOUT="+checkout, "FIXTURE_WORK="+work, "FIXTURE_LOG="+root)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("smoke driver: %v\n%s", err, out)
	}
	wantFiles := map[string]string{
		"git-cwd":     checkout,
		"new-cwd":     work,
		"new-argv":    strings.Join([]string{"project", "new", "smoke", "--kind", "service", "--mod", "github.com/e2e/smoke", "--service", "api", "--link-forge", "--forge-root", checkout}, "\n"),
		"render-cwd":  filepath.Join(work, "smoke"),
		"render-argv": "env\nrender\ndev",
	}
	for name, want := range wantFiles {
		got, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want+"\n" {
			t.Errorf("%s = %q, want %q", name, got, want+"\n")
		}
	}
}
