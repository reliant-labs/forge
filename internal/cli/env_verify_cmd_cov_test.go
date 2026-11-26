package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/cluster"
	"github.com/reliant-labs/forge/internal/deploytarget"
	deployv1alpha1 "github.com/reliant-labs/forge/pkg/deploy/v1alpha1"
	"github.com/reliant-labs/forge/pkg/release"
)

func covDClusterWorkload() WorkloadEntity {
	return WorkloadEntity{
		Name:    "web",
		Image:   "ghcr.io/acme/web",
		Runtime: RuntimeEntity{Type: RuntimeCluster, Cluster: &ClusterRuntime{Cluster: "gke_prod", Namespace: "acme"}},
		Spec:    deployv1alpha1.WorkloadSpec{Image: "ghcr.io/acme/web:v1"},
	}
}

func TestCovDVerifyStateRank(t *testing.T) {
	order := []verifyState{verifySkipped, verifyMatch, verifyUnverified, verifyError, verifyDrift}
	for i := 1; i < len(order); i++ {
		if order[i].rank() <= order[i-1].rank() {
			t.Errorf("%s must rank worse than %s", order[i], order[i-1])
		}
	}
	if verifyState("bogus").rank() != verifySkipped.rank() {
		t.Error("an unknown state ranks with skipped")
	}
}

func TestCovDVerifyVerdict(t *testing.T) {
	rows := func(states ...verifyState) []envVerifyRow {
		var out []envVerifyRow
		for i, s := range states {
			out = append(out, envVerifyRow{Workload: string(rune('a' + i)), State: s})
		}
		return out
	}
	cases := []struct {
		name     string
		rows     []envVerifyRow
		wantCode int
		wantMsg  string
	}{
		{"all match", rows(verifyMatch, verifySkipped), 0, ""},
		{"drift wins over unverified", rows(verifyUnverified, verifyDrift, verifyDrift), exitWrong, "drift: b, c"},
		{"error counts as not verified", rows(verifyMatch, verifyError, verifyUnverified), exitUndetermined, "not verified: b, c"},
		{"no rows", nil, 0, ""},
	}
	for _, c := range cases {
		err := verifyVerdict(c.rows)
		if exitCodeForError(err) != c.wantCode {
			t.Errorf("%s: exit %d (%v), want %d", c.name, exitCodeForError(err), err, c.wantCode)
		}
		if c.wantMsg != "" && (err == nil || err.Error() != c.wantMsg) {
			t.Errorf("%s: message %v, want %q", c.name, err, c.wantMsg)
		}
	}
}

func TestCovDRenderEnvVerify_TableAndDetails(t *testing.T) {
	doc := &envVerifyDocument{
		Env:     "prod",
		Release: "v1.4.0",
		Workloads: []envVerifyRow{
			{Workload: "api", Runtime: RuntimeExternal, State: verifyMatch,
				Image: verifyCheck{State: verifyMatch, Detail: "expected from promotion v1.4.0"}, Config: verifyCheck{State: verifyMatch}},
			{Workload: "web", Runtime: RuntimeCluster, State: verifyDrift,
				Image:  verifyCheck{State: verifyDrift, Detail: "running aaaa, expected bbbb"},
				Config: verifyCheck{State: verifyDrift, Detail: "2 object(s) differ", Output: "apps.v1.Deployment.web\nv1.Service.web"}},
		},
		Summary: verifySummary{Match: 1, Drift: 1},
	}
	doc.stamp(exitCodeError{code: exitWrong, msg: "drift: web"})
	var out bytes.Buffer
	renderEnvVerify(&out, doc)
	got := out.String()
	for _, want := range []string{
		"Verifying env prod against v1.4.0 — read-only",
		"WORKLOAD", "RUNTIME", "RESULT",
		"api", "MATCH", "DRIFT",
		"\nweb:\n",
		"  image  drift: running aaaa, expected bbbb",
		"  config drift: 2 object(s) differ",
		"         | apps.v1.Deployment.web",
		"         | v1.Service.web",
		"1 match, 1 drift, 0 unverified, 0 error, 0 skipped",
		"exit 1: drift: web",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "\napi:\n") {
		t.Errorf("a matching workload gets no detail block:\n%s", got)
	}

	out.Reset()
	renderEnvVerify(&out, &envVerifyDocument{Env: "dev"})
	if !strings.Contains(out.String(), "Verifying env dev against (no release promoted)") || strings.Contains(out.String(), "WORKLOAD") {
		t.Errorf("an empty report: %s", out.String())
	}
}

func TestCovDFinishEnvVerify_CountsStatesAndHonoursJSON(t *testing.T) {
	doc := func() *envVerifyDocument {
		return &envVerifyDocument{Env: "prod", Workloads: []envVerifyRow{
			{Workload: "a", State: verifyMatch}, {Workload: "b", State: verifyDrift}, {Workload: "c", State: verifyUnverified},
			{Workload: "d", State: verifyError}, {Workload: "e", State: verifySkipped}, {Workload: "f", State: verifySkipped},
		}}
	}
	verr := errors.New("boom")
	d := doc()
	var out bytes.Buffer
	if err := finishEnvVerify(&out, envVerifyOptions{}, d, verr); err != verr {
		t.Errorf("the verb's own error is returned, got %v", err)
	}
	if d.Summary != (verifySummary{Match: 1, Drift: 1, Unverified: 1, Error: 1, Skipped: 2}) {
		t.Errorf("summary = %+v", d.Summary)
	}
	if !strings.Contains(out.String(), "1 match, 1 drift, 1 unverified, 1 error, 2 skipped") {
		t.Errorf("text form:\n%s", out.String())
	}

	out.Reset()
	if err := finishEnvVerify(&out, envVerifyOptions{JSON: true}, doc(), nil); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(strings.TrimSpace(out.String()), "{") || strings.Contains(out.String(), "Verifying env") {
		t.Errorf("--json prints the document only:\n%s", out.String())
	}

	if err := finishEnvVerify(failingWriter{}, envVerifyOptions{JSON: true}, doc(), verr); err == nil || err == verr {
		t.Errorf("a failed JSON write must surface its own error, got %v", err)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("disk full") }

func TestCovDEnvVerifyDeps_WithDefaultsFillsEverySeam(t *testing.T) {
	d := envVerifyDeps{}.withDefaults()
	if d.Render == nil || d.Ledger == nil || d.Lister == nil || d.LiveDiff == nil || d.Secrets == nil || d.Stderr == nil || d.Hooks.Stderr == nil {
		t.Fatalf("a seam stayed nil: %+v", d)
	}
	if d.Secrets(&KCLEntities{}) == nil {
		t.Error("the default resolver always supplies a provider (a no-op one when the env declares none)")
	}

	var sentinel bytes.Buffer
	custom := envVerifyDeps{Stderr: &sentinel}.withDefaults()
	if custom.Stderr != &sentinel || custom.Hooks.Stderr != &sentinel {
		t.Error("a supplied stderr is kept and shared with the hook runner")
	}
	own := envVerifyDeps{Stderr: &sentinel, Hooks: deploytarget.ExternalHooks{Stderr: io.Discard}}.withDefaults()
	if own.Hooks.Stderr != io.Discard {
		t.Error("a supplied hook stderr is not overwritten")
	}
}

func TestCovDEnvVerifyDefaultLedgerFailureIsReported(t *testing.T) {
	d := envVerifyDeps{}.withDefaults()
	t.Setenv("FORGE_LEDGER", "not-a-ledger-kind")
	if _, _, err := d.Ledger(context.Background(), t.TempDir(), "prod", nil); err == nil {
		t.Error("a malformed FORGE_LEDGER must fail the selection")
	}
}

func TestCovDRunEnvVerify_FailuresMapToExitCodes(t *testing.T) {
	good := &verifyFixture{entities: &KCLEntities{Workloads: []WorkloadEntity{covDClusterWorkload()}}, bindings: boundTo(nil), hooks: &verifyHooks{}}

	t.Run("render failure", func(t *testing.T) {
		deps := good.deps()
		deps.Render = func(context.Context, string, string) (*KCLEntities, error) { return nil, errors.New("kcl broke") }
		var out bytes.Buffer
		err := runEnvVerify(context.Background(), &out, envVerifyOptions{Env: "prod"}, deps)
		if err == nil || !strings.Contains(err.Error(), "render env prod: kcl broke") {
			t.Fatalf("err = %v", err)
		}
		if !strings.Contains(out.String(), "exit 1:") {
			t.Errorf("the report must still name the exit code:\n%s", out.String())
		}
	})
	t.Run("unknown workload", func(t *testing.T) {
		err := runEnvVerify(context.Background(), io.Discard, envVerifyOptions{Env: "prod", Workloads: []string{"ghost"}}, good.deps())
		if exitCodeForError(err) != exitWrong || !strings.Contains(err.Error(), `declares no workload "ghost" (it declares: web)`) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("unknown --image workload", func(t *testing.T) {
		err := runEnvVerify(context.Background(), io.Discard, envVerifyOptions{Env: "prod", Images: map[string]string{"ghost": "x@sha256:" + strings.Repeat("a", 64)}}, good.deps())
		if exitCodeForError(err) != exitWrong || !strings.Contains(err.Error(), `"ghost"`) {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("ledger selection failure is undetermined", func(t *testing.T) {
		deps := good.deps()
		deps.Ledger = func(context.Context, string, string, *KCLEntities) (envPromotionReader, envLedger, error) {
			return nil, envLedger{}, errors.New("no ledger")
		}
		err := runEnvVerify(context.Background(), io.Discard, envVerifyOptions{Env: "prod"}, deps)
		if exitCodeForError(err) != exitUndetermined || !strings.Contains(err.Error(), "read the promotion ledger for prod: no ledger") {
			t.Fatalf("err = %v", err)
		}
	})
	t.Run("a failing Current names the ledger location", func(t *testing.T) {
		deps := good.deps()
		deps.Ledger = func(context.Context, string, string, *KCLEntities) (envPromotionReader, envLedger, error) {
			return covDBrokenReader{}, envLedger{}, nil
		}
		err := runEnvVerify(nil, io.Discard, envVerifyOptions{Env: "prod"}, deps) //nolint:staticcheck // a nil context is defaulted
		if exitCodeForError(err) != exitUndetermined || !strings.Contains(err.Error(), "(broken ledger): unreadable") {
			t.Fatalf("err = %v", err)
		}
	})
}

type covDBrokenReader struct{}

func (covDBrokenReader) Current(context.Context, string) (release.Promotion, bool, error) {
	return release.Promotion{}, false, errors.New("unreadable")
}
func (covDBrokenReader) Location() string { return "broken ledger" }

func TestCovDVerifyCluster_ImageStates(t *testing.T) {
	const repo = "ghcr.io/acme/web"
	runningAs := func(image string) fixedLister {
		return fixedLister{images: []cluster.WorkloadImage{{Kind: "Deployment", Name: "web", Container: "web", Image: image}}}
	}
	cases := []struct {
		name      string
		lister    fixedLister
		wantState verifyState
		wantIn    string
	}{
		{"matches", runningAs(repo + "@" + vDigestA), verifyMatch, "expected from promotion v1.4.0"},
		{"drifts", runningAs(repo + "@" + vDigestB), verifyDrift, "running"},
		{"missing from the cluster", fixedLister{}, verifyDrift, "not running in the cluster"},
		{"running by tag", runningAs(repo + ":v1"), verifyUnverified, "running by tag"},
		{"cluster unreachable", fixedLister{err: errors.New("connection refused")}, verifyError, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &verifyFixture{
				entities: &KCLEntities{Workloads: []WorkloadEntity{covDClusterWorkload()}},
				bindings: boundTo(map[string]string{repo: vDigestA}),
				hooks:    &verifyHooks{}, lister: c.lister,
				diffs: []clusterLiveDiff{{Cluster: "gke_prod"}},
			}
			doc, _ := runVerify(t, f, envVerifyOptions{})
			r := rowOf(t, doc, "web")
			if r.Image.State != c.wantState || !strings.Contains(r.Image.Detail, c.wantIn) {
				t.Errorf("image = %+v, want %s containing %q", r.Image, c.wantState, c.wantIn)
			}
			if r.Image.Expected != vDigestA {
				t.Errorf("expected digest = %q", r.Image.Expected)
			}
		})
	}

	t.Run("no promotion and no --image", func(t *testing.T) {
		f := &verifyFixture{
			entities: &KCLEntities{Workloads: []WorkloadEntity{covDClusterWorkload()}},
			bindings: &writeCountingBindings{}, hooks: &verifyHooks{},
		}
		doc, err := runVerify(t, f, envVerifyOptions{})
		if exitCodeForError(err) != exitUndetermined {
			t.Fatalf("err = %v", err)
		}
		r := rowOf(t, doc, "web")
		if r.Image.State != verifyUnverified || !strings.Contains(r.Image.Detail, "pass --image web=") {
			t.Errorf("image = %+v", r.Image)
		}
		if r.Config.State != verifyUnverified || !strings.Contains(r.Config.Detail, "no release is promoted") {
			t.Errorf("config = %+v", r.Config)
		}
	})
}

func TestCovDVerifyCluster_ConfigStates(t *testing.T) {
	const repo = "ghcr.io/acme/web"
	running := fixedLister{images: []cluster.WorkloadImage{{Kind: "Deployment", Name: "web", Container: "web", Image: repo + "@" + vDigestA}}}
	cases := []struct {
		name      string
		diffs     []clusterLiveDiff
		diffErr   error
		wantState verifyState
		wantIn    string
	}{
		{"clean", []clusterLiveDiff{{Cluster: "gke_prod"}}, nil, verifyMatch, "matches release v1.4.0's bundle"},
		{"the diff itself failed", []clusterLiveDiff{{Cluster: "gke_prod", Error: "kubectl diff exited 3"}}, nil, verifyError, "live diff against gke_prod: kubectl diff exited 3"},
		{"objects that cannot be diffed", []clusterLiveDiff{{Cluster: "gke_prod", Undiffable: []undiffableObject{{File: "crd.yaml", Error: "unsupported"}}}}, nil, verifyError, "1 object(s) on gke_prod could not be diffed"},
		{"nothing for this cluster", []clusterLiveDiff{{Cluster: "other"}}, nil, verifyUnverified, "bundle carries nothing for gke_prod"},
		{"a diff error with nothing for this cluster", nil, errors.New("no bundle"), verifyUnverified, "not verifiable without a build: no bundle"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := &verifyFixture{
				entities: &KCLEntities{Workloads: []WorkloadEntity{covDClusterWorkload()}},
				bindings: boundTo(map[string]string{repo: vDigestA}),
				hooks:    &verifyHooks{}, lister: running, diffs: c.diffs, diffErr: c.diffErr,
			}
			doc, _ := runVerify(t, f, envVerifyOptions{})
			r := rowOf(t, doc, "web")
			if r.Config.State != c.wantState || !strings.Contains(r.Config.Detail, c.wantIn) {
				t.Errorf("config = %+v, want %s containing %q", r.Config, c.wantState, c.wantIn)
			}
		})
	}
}

func TestCovDVerifyExpected_SourcesInPrecedenceOrder(t *testing.T) {
	const repo = "ghcr.io/acme/api"
	v := &envVerifier{
		opts:    envVerifyOptions{Images: map[string]string{"flagged": "r/x@" + vDigestB}},
		pins:    map[string]string{repo + ":v2": vDigestA, repo: vDigestB},
		release: "v9",
	}
	cases := []struct {
		name       string
		w          WorkloadEntity
		wantRef    string
		wantSource string
	}{
		{"--image wins", WorkloadEntity{Name: "flagged", Spec: deployv1alpha1.WorkloadSpec{Image: repo + ":v2"}}, "r/x@" + vDigestB, "--image"},
		{"the promotion pin for the tagged ref", WorkloadEntity{Name: "api", Spec: deployv1alpha1.WorkloadSpec{Image: repo + ":v2"}}, repo + "@" + vDigestA, "promotion v9"},
		{"the promotion pin for the repository", WorkloadEntity{Name: "api", Spec: deployv1alpha1.WorkloadSpec{Image: repo + ":v3"}}, repo + "@" + vDigestB, "promotion v9"},
		{"falls back to the workload image", WorkloadEntity{Name: "api", Image: repo}, repo + "@" + vDigestB, "promotion v9"},
		{"a digest the declaration pins", WorkloadEntity{Name: "api", Spec: deployv1alpha1.WorkloadSpec{Image: "other/img@" + vDigestA}}, "other/img@" + vDigestA, "declared image"},
		{"nothing declared", WorkloadEntity{Name: "api"}, "", ""},
		{"nothing pinned", WorkloadEntity{Name: "api", Spec: deployv1alpha1.WorkloadSpec{Image: "other/img:v1"}}, "", ""},
	}
	for _, c := range cases {
		ref, source := v.expected(c.w)
		if ref != c.wantRef || source != c.wantSource {
			t.Errorf("%s: got (%q, %q), want (%q, %q)", c.name, ref, source, c.wantRef, c.wantSource)
		}
	}
}

func TestCovDVerifyExternal_DegradedChecks(t *testing.T) {
	run := func(t *testing.T, ext deploytarget.ExternalRuntime, hooks *verifyHooks, opts envVerifyOptions) envVerifyRow {
		t.Helper()
		f := &verifyFixture{
			entities: &KCLEntities{Workloads: []WorkloadEntity{externalWorkload("api", "ghcr.io/acme/api:v1", ext)}},
			bindings: boundTo(map[string]string{"ghcr.io/acme/api": vDigestA}), hooks: hooks,
		}
		doc, _ := runVerify(t, f, opts)
		return rowOf(t, doc, "api")
	}
	t.Run("a status hook that reports no digest", func(t *testing.T) {
		r := run(t, fullHooks, &verifyHooks{status: map[string]string{"api": `{"health":"healthy"}`}}, envVerifyOptions{})
		if r.Image.State != verifyUnverified || !strings.Contains(r.Image.Detail, "reported no digest") {
			t.Errorf("image = %+v", r.Image)
		}
	})
	t.Run("a status hook that fails", func(t *testing.T) {
		r := run(t, fullHooks, &verifyHooks{}, envVerifyOptions{})
		if r.Image.State != verifyError {
			t.Errorf("image = %+v", r.Image)
		}
	})
	t.Run("no status hook declared", func(t *testing.T) {
		ext := fullHooks
		ext.Status = nil
		r := run(t, ext, &verifyHooks{}, envVerifyOptions{})
		if r.Image.State != verifyUnverified || !strings.Contains(r.Image.Detail, "no status hook") {
			t.Errorf("image = %+v", r.Image)
		}
	})
	t.Run("no plan hook declared", func(t *testing.T) {
		ext := fullHooks
		ext.Plan = nil
		r := run(t, ext, &verifyHooks{status: map[string]string{"api": statusDoc(vDigestA)}}, envVerifyOptions{})
		if r.Config.State != verifyUnverified || !strings.Contains(r.Config.Detail, "image identity only") || r.Image.State != verifyMatch {
			t.Errorf("row = %+v", r)
		}
	})
	t.Run("the running digest is shown when nothing is expected", func(t *testing.T) {
		f := &verifyFixture{
			entities: &KCLEntities{Workloads: []WorkloadEntity{externalWorkload("api", "ghcr.io/acme/api:v1", fullHooks)}},
			bindings: &writeCountingBindings{}, hooks: &verifyHooks{status: map[string]string{"api": statusDoc(vDigestB)}},
		}
		doc, _ := runVerify(t, f, envVerifyOptions{})
		r := rowOf(t, doc, "api")
		if r.Image.State != verifyUnverified || r.Image.Running != vDigestB || !strings.Contains(r.Image.Detail, "pass --image api=") {
			t.Errorf("image = %+v", r.Image)
		}
	})
}
