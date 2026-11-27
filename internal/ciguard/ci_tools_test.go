//go:build !windows

// scripts/ci-tools.sh is bash over a Unix host's tools, so the file builds for
// every OS but Windows rather than skipping there at run time.

package ciguard

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/templates"
)

// `task ci` (scripts/ci-premerge.sh) used to take kcl and pre-commit from
// whatever the host had on PATH, so on a host with neither its validate-kcl
// and pre-commit steps SKIPped and failed the gate. scripts/ci-tools.sh links
// the pinned releases into a checkout-local bin from the machine's shared tool
// cache, installing them there on a miss. These tests run it under bash
// against a fake cache, a fake curl, python3 and uname: no network, and the
// cache hit, the reuse of control-plane's pre-commit bundle, a pre-commit
// install and a refused kcl download are each exercised.
//
// The child reads only copies of files this process opened, so the short
// tier's cache sees an edit to the script or to either pin.

var ciToolsScript = filepath.Join("..", "..", "scripts", "ci-tools.sh")

// fakeToolsBin stands in for the host: uname reports linux/amd64 (the platform
// the kcl pin names), curl serves garbage and logs each call, and python3
// prints 3.12 and makes a "venv" whose pip writes venv/bin/pre-commit.
var fakeToolsBin = map[string]string{
	"uname": `#!/usr/bin/env bash
case "$1" in -s) echo Linux ;; -m) echo x86_64 ;; *) echo Linux ;; esac
`,
	"curl": `#!/usr/bin/env bash
out=""
while [ $# -gt 0 ]; do
  case "$1" in -o) out="$2"; shift ;; https://*) echo "$1" >> "$CURL_LOG" ;; esac
  shift
done
if [ -n "$out" ]; then echo garbage > "$out"; else echo '{"assets":[]}'; fi
`,
	"python3": `#!/usr/bin/env bash
echo "$*" >> "$PY_LOG"
case "$1" in
  -c) echo 3.12 ;;
  -m)
    mkdir -p "$3/bin"
    cat > "$3/bin/pip" <<'PIP'
#!/usr/bin/env bash
echo "pip $*" >> "$PY_LOG"
[ "$1" = install ] || exit 1
d=$(dirname "$0")
printf '#!/bin/sh\necho pre-commit\n' > "$d/pre-commit" && chmod +x "$d/pre-commit"
PIP
    chmod +x "$3/bin/pip" ;;
esac
`,
}

type ciToolsFixture struct {
	repo, cache, bin, curlLog, pyLog, path string
	kclKey, preCommitVersion               string
}

func newCIToolsFixture(t *testing.T) ciToolsFixture {
	t.Helper()
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock(1) is not installed: ci-tools.sh then uses a checkout-local cache, not the shared one under test")
	}
	tmp := t.TempDir()
	f := ciToolsFixture{
		repo:    filepath.Join(tmp, "repo"),
		cache:   filepath.Join(tmp, "cache"),
		bin:     filepath.Join(tmp, "repo", ".forge", "tools", "bin"),
		curlLog: filepath.Join(tmp, "curl.log"),
		pyLog:   filepath.Join(tmp, "py.log"),
	}
	for src, dst := range map[string]string{
		ciToolsScript: "scripts/ci-tools.sh",
		filepath.Join(installReleaseToolDir, "action.yml"):             ".github/actions/install-release-tool/action.yml",
		filepath.Join(installReleaseToolDir, "fetch-release-asset.sh"): ".github/actions/install-release-tool/fetch-release-asset.sh",
		filepath.Join("..", "..", ".pre-commit-config.yaml"):           ".pre-commit-config.yaml",
	} {
		raw, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		writeExec(t, filepath.Join(f.repo, dst), string(raw))
	}
	fake := filepath.Join(tmp, "fakebin")
	for name, body := range fakeToolsBin {
		writeExec(t, filepath.Join(fake, name), body)
	}
	f.path = fake + ":/usr/bin:/bin"
	f.kclKey = "kcl@" + strings.TrimPrefix(templates.KCLVersion, "v") + "-linux-amd64"
	f.preCommitVersion = preCommitPin(t)
	return f
}

func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// entry makes a ready cache entry holding an executable at rel.
func (f ciToolsFixture) entry(t *testing.T, key, rel string) string {
	t.Helper()
	writeExec(t, filepath.Join(f.cache, key, rel), "#!/bin/sh\necho fake\n")
	writeExec(t, filepath.Join(f.cache, key, ".complete"), "")
	return filepath.Join(f.cache, key, rel)
}

func (f ciToolsFixture) run(t *testing.T) (string, error) {
	t.Helper()
	cmd := exec.Command("bash", filepath.Join(f.repo, "scripts", "ci-tools.sh"))
	cmd.Env = []string{
		"HOME=" + filepath.Dir(f.repo),
		"PATH=" + f.path,
		"FORGE_CI_TOOLS_CACHE=" + f.cache,
		"CURL_LOG=" + f.curlLog,
		"PY_LOG=" + f.pyLog,
		"TMPDIR=" + filepath.Dir(f.repo),
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func (f ciToolsFixture) linkOf(t *testing.T, tool string) string {
	t.Helper()
	target, err := os.Readlink(filepath.Join(f.bin, tool))
	if err != nil {
		return ""
	}
	return target
}

func logOf(path string) string {
	raw, _ := os.ReadFile(path)
	return string(raw)
}

// preCommitPin is .pre-commit-config.yaml's minimum_pre_commit_version.
func preCommitPin(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", ".pre-commit-config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^minimum_pre_commit_version: "(\d+\.\d+\.\d+)"$`).FindSubmatch(raw)
	if m == nil {
		t.Fatal(`.pre-commit-config.yaml: no minimum_pre_commit_version: "X.Y.Z" — forge's pre-commit pin, which scripts/ci-tools.sh installs`)
	}
	return string(m[1])
}

// TestCIToolsLinksCachedPinsWithoutFetching: with both pins already in the
// shared cache (pre-commit as control-plane's python-tools bundle), the script
// only links them: no download, no venv.
func TestCIToolsLinksCachedPinsWithoutFetching(t *testing.T) {
	f := newCIToolsFixture(t)
	kcl := f.entry(t, f.kclKey, "kcl")
	pc := f.entry(t, "python-tools@"+f.preCommitVersion+"+shellcheck-py-0.10.0.1+py3.12-linux-amd64", "venv/bin/pre-commit")

	for range 2 { // the second run is the cached gate's: the same, idempotently
		if out, err := f.run(t); err != nil {
			t.Fatalf("ci-tools.sh: %v\n%s", err, out)
		}
		if got := f.linkOf(t, "kcl"); got != kcl {
			t.Errorf("kcl links to %q, want the cached %q", got, kcl)
		}
		if got := f.linkOf(t, "pre-commit"); got != pc {
			t.Errorf("pre-commit links to %q, want control-plane's cached bundle %q", got, pc)
		}
	}
	if l := logOf(f.curlLog); l != "" {
		t.Errorf("a cache hit downloaded:\n%s", l)
	}
	if l := logOf(f.pyLog); strings.Contains(l, "venv") {
		t.Errorf("a cache hit built a venv:\n%s", l)
	}
}

// TestCIToolsInstallsPreCommitOnAMiss: with no pre-commit entry, the script
// builds a venv at the pinned version into its own entry and links it.
func TestCIToolsInstallsPreCommitOnAMiss(t *testing.T) {
	f := newCIToolsFixture(t)
	f.entry(t, f.kclKey, "kcl")

	if out, err := f.run(t); err != nil {
		t.Fatalf("ci-tools.sh: %v\n%s", err, out)
	}
	key := "pre-commit@" + f.preCommitVersion + "+py3.12-linux-amd64"
	if _, err := os.Stat(filepath.Join(f.cache, key, ".complete")); err != nil {
		t.Errorf("no ready %s entry after the install: %v", key, err)
	}
	if got, want := f.linkOf(t, "pre-commit"), filepath.Join(f.cache, key, "venv", "bin", "pre-commit"); got != want {
		t.Errorf("pre-commit links to %q, want %q", got, want)
	}
	if l := logOf(f.pyLog); !strings.Contains(l, "-m venv "+filepath.Join(f.cache, key, "venv")) {
		t.Errorf("python3 was not asked for a venv at the entry's final path:\n%s", l)
	}
	if l := logOf(f.pyLog); !strings.Contains(l, "pre-commit=="+f.preCommitVersion) {
		t.Errorf("pip did not install pre-commit==%s:\n%s", f.preCommitVersion, l)
	}
}

// TestCIToolsRefusesAnUnverifiedKCL: a download whose sha256 is not the
// pinned one fails the script and leaves no entry and no link behind.
func TestCIToolsRefusesAnUnverifiedKCL(t *testing.T) {
	f := newCIToolsFixture(t)
	f.entry(t, "pre-commit@"+f.preCommitVersion+"+py3.12-linux-amd64", "venv/bin/pre-commit")

	out, err := f.run(t)
	if err == nil {
		t.Fatalf("ci-tools.sh passed over a kcl download that fails its checksum:\n%s", out)
	}
	if !strings.Contains(out, "NOT installed: kcl") {
		t.Errorf("the failure does not name kcl:\n%s", out)
	}
	if !strings.Contains(logOf(f.curlLog), "/releases/download/"+templates.KCLVersion+"/") {
		t.Errorf("no download of the pinned kcl %s was attempted:\n%s", templates.KCLVersion, logOf(f.curlLog))
	}
	if _, err := os.Stat(filepath.Join(f.cache, f.kclKey)); !os.IsNotExist(err) {
		t.Errorf("a failed install left %s in the cache (%v)", f.kclKey, err)
	}
	if got := f.linkOf(t, "kcl"); got != "" {
		t.Errorf("a failed install linked kcl to %q", got)
	}
}

// TestPremergeRunsCIToolsFirstOnPath: the gate links the tools before any step
// starts, and puts their directory first on PATH for all of them.
func TestPremergeRunsCIToolsFirstOnPath(t *testing.T) {
	script := readText(t, premergeScript)
	install := strings.Index(script, "\nstep ci-tools scripts/ci-tools.sh \"$tools_bin\"\n")
	export := strings.Index(script, "\nexport PATH=\"$tools_bin:$PATH\"\n")
	first := strings.Index(script, "\nstep test-short test_short &\n")
	if install < 0 || export < 0 || first < 0 {
		t.Fatalf("%s must run `step ci-tools scripts/ci-tools.sh \"$tools_bin\"` and `export PATH=\"$tools_bin:$PATH\"` before its first step", premergeScript)
	}
	if install > export || export > first {
		t.Errorf("%s: ci-tools, then the PATH export, must come before the steps start", premergeScript)
	}
	if !strings.Contains(script, `tools_bin="$PWD/.forge/tools/bin"`) {
		t.Errorf("%s: the tools bin must be the checkout's gitignored .forge/tools/bin", premergeScript)
	}
}
