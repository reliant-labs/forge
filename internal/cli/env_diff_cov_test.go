package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/pkg/release"
	"github.com/reliant-labs/forge/pkg/testkit"
)

func TestCovDEnvDiffTargets(t *testing.T) {
	dir := t.TempDir()
	for _, env := range []string{"prod", "dev", ".staging-tmp"} {
		covDWriteTree(t, dir, map[string]string{"deploy/kcl/" + env + "/main.k": "x = 1\n"})
	}
	covDWriteTree(t, dir, map[string]string{"deploy/kcl/not-an-env/readme.md": "x"})

	got, err := envDiffTargets(dir, envDiffOptions{Env: "prod"})
	if err != nil || len(got) != 1 || got[0] != "prod" {
		t.Errorf("a named env is returned as given, got %v %v", got, err)
	}
	got, err = envDiffTargets(dir, envDiffOptions{All: true})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "dev,prod" {
		t.Errorf("--all must list every declared env sorted and skip dot dirs, got %v", got)
	}
	if _, err := envDiffTargets(t.TempDir(), envDiffOptions{All: true}); err == nil || !strings.Contains(err.Error(), "declares no environments") {
		t.Errorf("--all on a project with no envs: %v", err)
	}
}

func TestCovDSecretsShape(t *testing.T) {
	declared, bundle := &release.Shape{Kind: release.EnvPersistent}, &release.Shape{Kind: release.EnvLocal}
	if secretsShape(declared, bundle) != declared {
		t.Error("the declaration wins")
	}
	if secretsShape(nil, bundle) != bundle {
		t.Error("the bundle is the fallback")
	}
	if secretsShape(nil, nil) != nil {
		t.Error("neither means nil")
	}
}

func TestCovDFetchSecretPresence(t *testing.T) {
	hosted := &release.Shape{Secrets: []release.ShapeSecret{{Name: "A", Provider: "hosted"}}}
	env := LiveEnvironment{Env: release.EnvRecord{Name: "prod"}, EnvironmentID: "env-1"}

	t.Run("names are joined with the store's own verdict", func(t *testing.T) {
		client := &fakeCloudCaller{replies: map[string]string{procListSecrets: `{"secrets":[
			{"name":"SET","currentVersion":2},
			{"name":"ZERO","currentVersion":0},
			{"name":"DELETED","currentVersion":3,"currentVersionDeleted":true},
			{"name":"DESTROYED","currentVersion":3,"currentVersionDestroyed":true}]}`}}
		got := fetchSecretPresence(context.Background(), client, env, hosted, io.Discard)
		want := map[string]bool{"SET": true, "ZERO": false, "DELETED": false, "DESTROYED": false}
		if len(got) != len(want) {
			t.Fatalf("presence = %v, want %v", got, want)
		}
		for k, v := range want {
			if got[k] != v {
				t.Errorf("presence[%s] = %v, want %v", k, got[k], v)
			}
		}
		if len(client.calls) != 1 || !strings.Contains(client.calls[0].body, "env-1") {
			t.Errorf("the list must be scoped to the env id: %+v", client.calls)
		}
	})

	t.Run("a failed read is unverifiable, not missing", func(t *testing.T) {
		client := &fakeCloudCaller{errs: map[string]error{procListSecrets: errors.New("boom")}}
		var errOut bytes.Buffer
		if got := fetchSecretPresence(context.Background(), client, env, hosted, &errOut); got != nil {
			t.Errorf("presence = %v, want nil", got)
		}
		if !strings.Contains(errOut.String(), "could not read prod's secret presence (boom)") {
			t.Errorf("the reason must be printed: %q", errOut.String())
		}
	})

	t.Run("an external provider is never queried", func(t *testing.T) {
		client := &fakeCloudCaller{}
		external := &release.Shape{Secrets: []release.ShapeSecret{{Name: "A", Provider: "gsm"}}}
		if got := fetchSecretPresence(context.Background(), client, env, external, io.Discard); got != nil || len(client.calls) != 0 {
			t.Errorf("got %v after %d calls", got, len(client.calls))
		}
	})
}

func TestCovDSecretPresenceFor(t *testing.T) {
	candidate := release.Shape{Secrets: []release.ShapeSecret{{Name: "A"}, {Name: "B"}, {Name: "C"}}}
	if secretPresenceFor(candidate, liveSide{}) != nil {
		t.Error("no live secrets means nothing is verifiable")
	}
	if secretPresenceFor(candidate, liveSide{Secrets: map[string]bool{"Z": true}}) != nil {
		t.Error("live secrets that match no declared name verify nothing")
	}
	got := secretPresenceFor(candidate, liveSide{Secrets: map[string]bool{"A": true, "B": false, "Z": true}})
	if len(got) != 2 || !got["A"] || got["B"] {
		t.Errorf("presence = %v, want A=true B=false and nothing for C or Z", got)
	}
	if _, ok := got["C"]; ok {
		t.Error("an unverifiable name must be absent, never false")
	}
}

func TestCovDDescribeSecretNeedAndRuntime(t *testing.T) {
	s := release.ShapeSecret{Name: "DB", Provider: "hosted", DeclaredBy: []string{"api", "worker"}}
	cases := []struct {
		presence map[string]bool
		want     string
	}{
		{nil, "DB [hosted] — declared; presence not verifiable from here (needed by api, worker)"},
		{map[string]bool{"DB": true}, "DB [hosted] — already set (needed by api, worker)"},
		{map[string]bool{"DB": false}, "DB [hosted] — MISSING (needed by api, worker)"},
	}
	for _, c := range cases {
		if got := describeSecretNeed(s, c.presence); got != c.want {
			t.Errorf("got %q, want %q", got, c.want)
		}
	}
	if got := describeSecretNeed(release.ShapeSecret{Name: "X"}, nil); got != "X — declared; presence not verifiable from here" {
		t.Errorf("bare secret = %q", got)
	}
	if describeRuntime("", "") != "(none)" || describeRuntime("hosted", "") != "hosted" || describeRuntime("cluster", "c1") != "cluster on c1" {
		t.Error("describeRuntime wrong")
	}
}

func TestCovDWriteEnvDiff_RendersEveryCategory(t *testing.T) {
	changeImg := release.ObjectChange{
		Live: obj("Deployment", "api", "a"), Candidate: obj("Deployment", "api", "b"),
		Images: []release.ImageChange{{}},
	}
	changeBoth := release.ObjectChange{Candidate: obj("Deployment", "worker", "c"), ConfigChanged: true, Images: []release.ImageChange{{}}}
	changeCfg := release.ObjectChange{Candidate: obj("ConfigMap", "cfg", "d"), ConfigChanged: true}
	noWorkload := obj("Secret", "orphan", "e")
	noWorkload.Workload = ""
	changeAnon := release.ObjectChange{Candidate: noWorkload, ConfigChanged: true}

	doc := envDiffDoc{
		Source: "/work/proj", Against: "live", Charts: false,
		Environments: []envDiffEntry{
			{Env: "broken", Status: diffImpure, Detail: "wrote 2", Wrote: []string{"a.txt", "b.txt"}},
			{Env: "fresh", Status: diffOK, WouldBeCreated: true},
			{Env: "nothing", Status: diffOK, LiveSource: "none", Diff: &release.ShapeDiff{Added: make([]release.ShapeObject, 4)}},
			{Env: "declared", Status: diffOK, LiveSource: "declared_shape", ConfigIdentical: true, Diff: &release.ShapeDiff{}},
			{Env: "nodiff", Status: diffOK, LiveSource: "bundle"},
			{Env: "busy", Status: diffOK, LiveSource: "bundle",
				SecretPresence: map[string]bool{"NEW": true},
				Diff: &release.ShapeDiff{
					Added:            []release.ShapeObject{obj("Service", "svc", "f")},
					Removed:          []release.ShapeObject{obj("Service", "old", "g"), obj("Service", "old2", "h")},
					Changed:          []release.ObjectChange{changeImg, changeBoth, changeCfg, changeAnon},
					WorkloadsAdded:   []string{"w-new"},
					WorkloadsRemoved: []string{"w-old"},
					RuntimeChanges:   []release.RuntimeChange{{Workload: "api", From: "cluster", FromCluster: "c1", To: "hosted"}},
					SecretsAdded:     []release.ShapeSecret{{Name: "NEW", Provider: "hosted"}},
					SecretsRemoved:   []release.ShapeSecret{{Name: "OLD"}},
					DomainsAdded:     []string{"a.example.com"},
					DomainsRemoved:   []string{"b.example.com"},
					ClustersAdded:    []string{"c2"},
					ClustersRemoved:  []string{"c0"},
				}},
		},
	}
	var b bytes.Buffer
	writeEnvDiff(&b, doc)
	out := b.String()
	for _, want := range []string{
		"diff of /work/proj against live",
		"(Helm chart objects are not compared; pass --charts to template them)",
		"broken: impure", "wrote 2", "    wrote a.txt", "    wrote b.txt",
		"fresh: ok", "would be created: this environment is declared here",
		"nothing: ok", "no recorded config: nothing has been built", "this render declares 4 object(s)",
		"comparing against the DECLARED shape",
		"config identical: this render normalizes",
		"nodiff: ok",
		"objects: 1 added, 2 removed, 4 changed",
		"    api: would run a new build",
		"    worker: config changed, and would run a new build",
		"    cfg: config changed",
		"    (no workload): config changed",
		"+ workload w-new", "- workload w-old",
		"~ api: cluster on c1 → hosted",
		"+ secret NEW [hosted] — already set",
		"- secret OLD (no longer declared; the value is NOT deleted)",
		"+ domain a.example.com", "- domain b.example.com",
		"+ cluster c2", "- cluster c0",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "nothing: ok\n  no changes") {
		t.Errorf("an env with no recorded config must not read as 'no changes':\n%s", out)
	}
	if !strings.Contains(out, "declared: ok\n  comparing against the DECLARED shape") || !strings.Contains(out, "  no changes") {
		t.Errorf("an empty diff against the declared shape:\n%s", out)
	}

	var charts bytes.Buffer
	writeEnvDiff(&charts, envDiffDoc{Source: "s", Against: "live", Charts: true})
	if strings.Contains(charts.String(), "Helm chart objects are not compared") {
		t.Errorf("--charts must not print the not-compared note: %s", charts.String())
	}
}

func TestCovDWriteDiffCounts_NothingToCount(t *testing.T) {
	var b bytes.Buffer
	writeDiffCounts(&b, &release.ShapeDiff{})
	if b.Len() != 0 {
		t.Errorf("an empty diff prints no object summary, got %q", b.String())
	}
}

func covDDiffCmd(out, errOut io.Writer) *cobra.Command {
	cmd := &cobra.Command{}
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	cmd.SetContext(context.Background())
	return cmd
}

func covDDiffProject(t *testing.T, envs ...string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{"forge.yaml": "name: shop\n"}
	for _, env := range envs {
		files["deploy/kcl/"+env+"/main.k"] = "x = 1\n"
	}
	covDWriteTree(t, dir, files)
	return dir
}

func TestCovDRunEnvDiff_JSONDocumentAndHumanForm(t *testing.T) {
	testkit.Slow(t, "execs git")
	dir := covDDiffProject(t, "dev", "prod")
	t.Chdir(dir)
	prevName := hostedProjectName
	hostedProjectName = func() string { return "shop" }
	t.Cleanup(func() { hostedProjectName = prevName })
	stubRender(t, map[string]stubbedRender{
		"dev":  {shape: diffShape(obj("Deployment", "api", "a"))},
		"prod": {err: errors.New("kcl exploded")},
	})

	var out, errOut bytes.Buffer
	if err := runEnvDiff(covDDiffCmd(&out, &errOut), envDiffOptions{All: true, Against: "live", JSON: true}); err != nil {
		t.Fatal(err)
	}
	var doc envDiffDoc
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("stdout must be exactly one JSON document: %v\n%s", err, out.String())
	}
	if doc.Project != "shop" || doc.Against != "live" || len(doc.Environments) != 2 {
		t.Fatalf("doc = %+v", doc)
	}
	if doc.Environments[0].Env != "dev" || doc.Environments[0].Status != diffOK || doc.Environments[0].LiveSource != "none" {
		t.Errorf("dev entry = %+v", doc.Environments[0])
	}
	if doc.Environments[1].Env != "prod" || doc.Environments[1].Status != diffError || doc.Environments[1].Detail != "kcl exploded" || doc.Environments[1].Diff != nil {
		t.Errorf("prod entry = %+v", doc.Environments[1])
	}

	out.Reset()
	if err := runEnvDiff(covDDiffCmd(&out, &errOut), envDiffOptions{Env: "dev", Against: "live"}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "against live") || !strings.Contains(out.String(), "dev: ok") || !strings.Contains(out.String(), "would be created") {
		t.Errorf("human form:\n%s", out.String())
	}
}

func TestCovDRunEnvDiff_RefusesAnUnimplementedTargetBeforeRendering(t *testing.T) {
	dir := covDDiffProject(t, "dev")
	t.Chdir(dir)
	prev := renderEnvShapeForDiff
	renderEnvShapeForDiff = func(context.Context, string, string, io.Writer) (envShapeDoc, []string, error) {
		t.Error("render must not run when --against is refused")
		return envShapeDoc{}, nil, nil
	}
	t.Cleanup(func() { renderEnvShapeForDiff = prev })
	var out bytes.Buffer
	err := runEnvDiff(covDDiffCmd(&out, io.Discard), envDiffOptions{Env: "dev", Against: "bundle-123"})
	if err == nil || !strings.Contains(err.Error(), `--against "bundle-123" is not implemented`) {
		t.Fatalf("err = %v", err)
	}
	if out.Len() != 0 {
		t.Errorf("nothing may be printed on refusal: %q", out.String())
	}
}

func TestCovDRunEnvDiff_AllWithNoEnvsFails(t *testing.T) {
	dir := covDDiffProject(t)
	t.Chdir(dir)
	err := runEnvDiff(covDDiffCmd(io.Discard, io.Discard), envDiffOptions{All: true, Against: "live"})
	if err == nil || !strings.Contains(err.Error(), "declares no environments") {
		t.Fatalf("err = %v", err)
	}
}

func TestCovDEnvDiffCmd_FlagValidation(t *testing.T) {
	for _, args := range [][]string{{}, {"prod", "--all"}} {
		cmd := newEnvDiffCmd()
		cmd.SetArgs(args)
		cmd.SetOut(io.Discard)
		cmd.SetErr(io.Discard)
		err := cmd.Execute()
		if err == nil || !strings.Contains(err.Error(), "name an environment or pass --all") {
			t.Errorf("args %v: err = %v", args, err)
		}
	}
	cmd := newEnvDiffCmd()
	if f := cmd.Flags().Lookup("against"); f == nil || f.DefValue != diffAgainstLive {
		t.Errorf("--against must default to live: %+v", f)
	}
}

func TestCovDRenderAndDiffOnce_RetriesOnceThenReportsStale(t *testing.T) {
	testkit.Slow(t, "execs git")
	dir := t.TempDir()
	srcGit(t, dir, "init", "-q")
	covDWriteTree(t, dir, map[string]string{"f.txt": "0"})
	srcGit(t, dir, "add", "-A")
	srcGit(t, dir, "commit", "-qm", "init")

	calls := 0
	prev := renderEnvShapeForDiff
	renderEnvShapeForDiff = func(context.Context, string, string, io.Writer) (envShapeDoc, []string, error) {
		calls++
		// The tree moves under every render, as if the user kept saving.
		if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte(strings.Repeat("x", calls)), 0o644); err != nil {
			t.Error(err)
		}
		return envShapeDoc{Shape: diffShape()}, nil, nil
	}
	t.Cleanup(func() { renderEnvShapeForDiff = prev })

	entry := diffOneEnv(context.Background(), dir, "prod", liveSide{Known: true}, envDiffOptions{}, io.Discard)
	if entry.Status != diffStale {
		t.Fatalf("status = %q, want stale", entry.Status)
	}
	if calls != 2 {
		t.Errorf("a stale render is retried exactly once, got %d renders", calls)
	}
	if !strings.Contains(entry.Detail, "changed while rendering, twice") {
		t.Errorf("detail = %q", entry.Detail)
	}
}
