package templates

import (
	"strings"
	"testing"
)

func renderReconcile(t *testing.T, data ReconcileWorkflowData) string {
	t.Helper()
	out, err := CITemplates("github").Render("reconcile.yml.tmpl", data)
	if err != nil {
		t.Fatalf("render reconcile.yml.tmpl: %v", err)
	}
	return string(out)
}

func reconcileFixture() ReconcileWorkflowData {
	return ReconcileWorkflowData{
		ProjectName: "myapp",
		Environments: []DeployEnv{
			{Name: "staging", Auto: true},
			{Name: "prod", Protection: true},
		},
		ForgeVersion: "v0.1.15",
	}
}

// The workflow runs on a schedule AND on demand. The schedule is the drift
// detection; the manual trigger is what an operator reaches for mid-incident
// to ask "what is actually running right now?".
func TestReconcile_RunsScheduledAndOnDemand(t *testing.T) {
	out := renderReconcile(t, reconcileFixture())

	for _, want := range []string{
		"schedule:",
		`- cron: "17 * * * *"`,
		"workflow_dispatch:",
		"forge reconcile",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("reconcile.yml missing %q", want)
		}
	}
}

// Every declared environment reaches the matrix and the dispatch choice list.
// An environment that deploys but is never reconciled is the one that drifts
// unobserved, so the two lists must come from one declaration.
func TestReconcile_CoversEveryEnvironment(t *testing.T) {
	out := renderReconcile(t, reconcileFixture())

	for _, env := range []string{"staging", "prod"} {
		// Twice: once in the dispatch options, once in the matrix.
		if strings.Count(out, "- "+env) < 2 {
			t.Errorf("environment %q appears %d times; it must be in both the dispatch "+
				"options and the matrix", env, strings.Count(out, "- "+env))
		}
	}
}

// READ-ONLY, AND THE PERMISSION BLOCK SAYS SO.
//
// The default policy is observe — report drift, change nothing — and a
// workflow granted write permissions it does not use is a blast radius waiting
// for someone to add a step.
func TestReconcile_IsReadOnly(t *testing.T) {
	out := renderReconcile(t, reconcileFixture())

	if !strings.Contains(out, "permissions:\n  contents: read") {
		t.Error("reconcile.yml does not declare read-only permissions")
	}
	for _, forbidden := range []string{
		"contents: write",
		"packages: write",
		"id-token: write",
	} {
		if strings.Contains(out, forbidden) {
			t.Errorf("reconcile.yml grants %q; the observe default needs none of it", forbidden)
		}
	}
}

// ONE PASS AT A TIME PER ENVIRONMENT. Two concurrent observations of one
// environment race to write the same state records, and the later writer wins
// with no reason to believe it saw the more recent truth.
func TestReconcile_SerializesPerEnvironment(t *testing.T) {
	out := renderReconcile(t, reconcileFixture())

	if !strings.Contains(out, "concurrency:") {
		t.Fatal("reconcile.yml has no concurrency group; two passes could observe one environment at once")
	}
	// Cancelling an in-flight reconcile would abandon a pass halfway through
	// its observations, which is worse than letting it finish.
	if !strings.Contains(out, "cancel-in-progress: false") {
		t.Error("reconcile.yml cancels in-progress runs; a half-finished observation pass " +
			"reports a state that was never true")
	}
}

// One unreachable cluster must not cancel the report for every other
// environment. Each is an independent question.
func TestReconcile_OneEnvironmentFailingDoesNotCancelTheRest(t *testing.T) {
	out := renderReconcile(t, reconcileFixture())

	if !strings.Contains(out, "fail-fast: false") {
		t.Error("the matrix is fail-fast; one unreachable target would cancel every other " +
			"environment's reconcile and hide drift that was about to be reported")
	}
}

// The forge binary is PINNED. A scheduled job runs unattended for months, and
// resolving `@latest` at 03:17 on some future morning means the reconcile that
// reports drift is not the forge that wrote the project.
func TestReconcile_PinsTheForgeVersion(t *testing.T) {
	out := renderReconcile(t, reconcileFixture())
	if !strings.Contains(out, "cmd/forge@v0.1.15") {
		t.Error("reconcile.yml does not pin the forge version it installs")
	}

	// Falls back to the commit when there is no installable version — a
	// dev-built scaffold is still reproducible.
	data := reconcileFixture()
	data.ForgeVersion = ""
	data.ForgeGitCommit = "83eddf47e689"
	out = renderReconcile(t, data)
	if !strings.Contains(out, "cmd/forge@83eddf47e689") {
		t.Error("with no version, reconcile.yml does not fall back to pinning by commit")
	}
}
