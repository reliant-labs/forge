package ciguard

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Match parsed YAML values, not commentary. A workflow token hidden in a
// composite env, or passed as a differently named input, is still a token.
var candidateCredential = regexp.MustCompile(`(?i)\b(?:GH_TOKEN|GITHUB_TOKEN)\b|github\s*(?:\.\s*token|\[\s*['"]token['"]\s*\])|\bsecrets\s*[.\[]`)

func candidateWorkflowFindings(data []byte, readAction func(string) ([]byte, error)) []string {
	var root map[string]any
	if err := yaml.Unmarshal(data, &root); err != nil {
		return []string{err.Error()}
	}
	var findings []string
	if p, ok := root["permissions"].(map[string]any); !ok || len(p) != 1 || p["contents"] != "read" {
		findings = append(findings, "CI must declare only contents: read at workflow level")
	}
	var walk func(any, string)
	seen := map[string]bool{}
	visitAction := func(step map[string]any, where string) {
		uses, _ := step["uses"].(string)
		if strings.HasPrefix(uses, "actions/checkout@") {
			with, _ := step["with"].(map[string]any)
			if with["persist-credentials"] != false || with["token"] != nil || with["ssh-key"] != nil {
				findings = append(findings, where+": checkout requires persist-credentials: false and no custom credentials")
			}
			if strings.HasPrefix(where, "ci.jobs.gate.") {
				findings = append(findings, where+": required gate must not check out candidate source")
			}
		}
		if !strings.HasPrefix(uses, "./") || seen[uses] {
			return
		}
		seen[uses] = true
		raw, err := readAction(uses)
		if err != nil {
			findings = append(findings, where+": cannot inspect local action: "+err.Error())
			return
		}
		var action map[string]any
		if err := yaml.Unmarshal(raw, &action); err != nil {
			findings = append(findings, where+": cannot decode local action: "+err.Error())
			return
		}
		walk(action, uses)
	}
	walk = func(v any, where string) {
		switch v := v.(type) {
		case map[string]any:
			visitAction(v, where)
			for k, value := range v {
				if candidateCredential.MatchString(k) {
					findings = append(findings, where+": credential key "+k)
				}
				if k == "permissions" && where != "ci" {
					p, ok := value.(map[string]any)
					if !ok || (len(p) > 0 && (len(p) != 1 || p["contents"] != "read")) {
						findings = append(findings, where+": job cannot expand CI permissions")
					}
				}
				walk(value, where+"."+k)
			}
		case []any:
			for i, item := range v {
				walk(item, fmt.Sprintf("%s[%d]", where, i))
			}
		case string:
			if candidateCredential.MatchString(v) {
				findings = append(findings, where+": explicitly exposes a credential")
			}
		}
	}
	walk(root, "ci")
	return findings
}

func TestCandidateCIHasNoExposedGitHubCredentials(t *testing.T) {
	raw, err := os.ReadFile(ciWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	readAction := func(uses string) ([]byte, error) {
		dir := filepath.Join("..", "..", uses)
		body, err := os.ReadFile(filepath.Join(dir, "action.yml"))
		if os.IsNotExist(err) {
			return os.ReadFile(filepath.Join(dir, "action.yaml"))
		}
		return body, err
	}
	for _, finding := range candidateWorkflowFindings(raw, readAction) {
		t.Error(finding)
	}
}

func TestCandidateCIGateDoesNotDelegateItsVerdict(t *testing.T) {
	raw, err := os.ReadFile(ciWorkflow)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Jobs map[string]struct {
			Permissions map[string]any
			Steps       []struct {
				Name, Uses, Run string
				ContinueOnError bool `yaml:"continue-on-error"`
			}
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	gate := doc.Jobs["gate"]
	if gate.Permissions == nil || len(gate.Permissions) != 0 || len(gate.Steps) != 3 {
		t.Fatal("the required gate must have zero permissions, its verdict, and two advisory receipt steps")
	}
	verdict := gate.Steps[0]
	if verdict.Name != "Every job passed" || verdict.Uses != "" || verdict.ContinueOnError ||
		strings.Contains(verdict.Run, "scripts/") || strings.Contains(verdict.Run, "gh api") {
		t.Fatal("the required verdict must be computed from needs inline, not by candidate scripts or reporting")
	}
	for _, step := range gate.Steps[1:] {
		if !step.ContinueOnError || (step.Uses != "" && !strings.HasPrefix(step.Uses, "actions/upload-artifact@")) {
			t.Fatalf("receipt step %q cannot alter the verdict or execute an arbitrary action", step.Name)
		}
	}
	if testing.Short() {
		return
	}
	requireCICommands(t, "bash", "jq")
	for _, tc := range []struct {
		name, event, results string
		pass                 bool
	}{
		{"all queue checks pass", "merge_group", `{"test-short":{"result":"success"},"checks":{"result":"success"},"golangci":{"result":"success"},"windows-changed":{"result":"success"},"lint":{"result":"skipped"}}`, true},
		{"ordinary pull request skips merge tier", "pull_request", `{"pr-checks":{"result":"success"},"test-short":{"result":"skipped"}}`, true},
		{"queue check skipped", "merge_group", `{"test-short":{"result":"skipped"}}`, false},
		{"job failed", "merge_group", `{"test-short":{"result":"failure"}}`, false},
		{"job cancelled", "push", `{"lint":{"result":"cancelled"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("bash", "-c", verdict.Run)
			cmd.Dir = t.TempDir() // no checkout or candidate helper is available
			cmd.Env = append(os.Environ(), "GITHUB_EVENT_NAME="+tc.event, "RESULTS="+tc.results)
			if output, err := cmd.CombinedOutput(); (err == nil) != tc.pass {
				t.Fatalf("want pass=%v; got %v\n%s", tc.pass, err, output)
			}
		})
	}
}

func TestCandidateCICredentialGuardCatchesRegressions(t *testing.T) {
	for _, tc := range []struct {
		name, job, composite, want string
	}{
		{"inherited read only", "steps: [{uses: 'actions/checkout@v7', with: {persist-credentials: false}}]", "", ""},
		{"default persists", "steps: [{uses: 'actions/checkout@v7'}]", "", "persist-credentials"},
		{"explicit persists", "steps: [{uses: 'actions/checkout@v7', with: {persist-credentials: true}}]", "", "persist-credentials"},
		{"write override", "permissions: {issues: write}", "", "cannot expand"},
		{"unrestricted override", "permissions: write-all", "", "cannot expand"},
		{"read token exposure", `env: {UNUSUAL_NAME: "${{ github['token'] }}"}`, "", "credential"},
		{"transitive credential", "steps: [{uses: './.github/actions/child'}]", `runs: {steps: [{env: {GH_TOKEN: '${{ github.token }}'}}]}`, "credential"},
		{"required gate checkout", "", "", "gate must not check out"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := "permissions: {contents: read}\njobs:\n  candidate:\n    " + tc.job + "\n"
			if tc.job == "" {
				raw = "permissions: {contents: read}\njobs:\n  gate:\n    steps: [{uses: 'actions/checkout@v7', with: {persist-credentials: false}}]\n"
			}
			got := strings.Join(candidateWorkflowFindings([]byte(raw), func(string) ([]byte, error) { return []byte(tc.composite), nil }), "\n")
			if (tc.want == "" && got != "") || (tc.want != "" && !strings.Contains(got, tc.want)) {
				t.Fatalf("want finding %q, got %q", tc.want, got)
			}
		})
	}
}
