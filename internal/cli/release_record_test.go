package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/cloud"
	"github.com/reliant-labs/forge/internal/deploytarget"
	"github.com/reliant-labs/forge/pkg/release"
)

const (
	recordDigestA = "sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	recordDigestB = "sha256:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	recordCommitA = "0123456789abcdef0123456789abcdef01234567"
	recordCommitB = "89abcdef0123456789abcdef0123456789abcdef"
)

// fakeRecordCP is a control plane with just enough state for a record: it
// keeps cut releases (and answers a re-cut as the real ledger does — a retry
// when the content matches, already_exists when it does not), environments by
// name, and one env's promotions, newest last.
type fakeRecordCP struct {
	mu         sync.Mutex
	calls      []fakeRecordCall
	releases   map[string]map[string]any
	envs       map[string]map[string]any
	promotions []map[string]any
	promoteErr error
	cutErr     error
}

type fakeRecordCall struct {
	proc string
	body map[string]any
}

func newFakeRecordCP() *fakeRecordCP {
	return &fakeRecordCP{releases: map[string]map[string]any{}, envs: map[string]map[string]any{}}
}

func (f *fakeRecordCP) Call(_ context.Context, proc string, req, out any) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	raw, _ := json.Marshal(req)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	short := proc[strings.LastIndex(proc, "/")+1:]
	f.calls = append(f.calls, fakeRecordCall{short, body})

	var reply any
	switch short {
	case "CutRelease":
		if f.cutErr != nil {
			return f.cutErr
		}
		version, _ := body["version"].(string)
		content := func(b map[string]any) string {
			c, _ := json.Marshal([]any{b["artifacts"], b["gitCommit"]})
			return string(c)
		}
		if prev, ok := f.releases[version]; ok {
			if content(prev) != content(body) {
				return &cloud.Error{Code: cloud.CodeAlreadyExists, Message: "release " + version + " already exists with different artifacts"}
			}
			reply = map[string]any{"created": false}
			break
		}
		f.releases[version] = body
		reply = map[string]any{"created": true}
	case "ListEnvironments":
		var list []any
		if e, ok := f.envs[body["search"].(string)]; ok {
			list = append(list, e)
		}
		reply = map[string]any{"environments": list}
	case "EnsureEnvironment":
		spec := body["spec"].(map[string]any)
		name := spec["name"].(string)
		created := false
		if _, ok := f.envs[name]; !ok {
			f.envs[name] = map[string]any{"id": "env-" + name, "name": name, "project": spec["project"], "kind": spec["kind"]}
			created = true
		}
		reply = map[string]any{"environment": f.envs[name], "created": created}
	case "ListPromotions":
		var list []any
		if n := len(f.promotions); n > 0 {
			list = append(list, f.promotions[n-1])
		}
		reply = map[string]any{"promotions": list}
	case "Promote":
		if f.promoteErr != nil {
			return f.promoteErr
		}
		version := body["version"].(string)
		if n := len(f.promotions); n > 0 && f.promotions[n-1]["releaseVersion"] == version {
			reply = map[string]any{"promotion": f.promotions[n-1]}
			break
		}
		cut := f.releases[version]
		resolved := map[string]any{}
		if arts, ok := cut["artifacts"].([]any); ok {
			for _, a := range arts {
				am := a.(map[string]any)
				resolved[am["name"].(string)] = am["digest"]
			}
		}
		p := map[string]any{
			"id": fmt.Sprintf("promo-%d", len(f.promotions)+1), "environmentId": body["environmentId"],
			"releaseVersion": version, "kind": wireKindPromote, "createdAt": time.Now().UTC().Format(time.RFC3339Nano),
			"resolvedArtifacts": resolved, "promotedByActor": body["promotedByActor"], "run": body["run"],
			"recordOnly": cut["recordOnly"] == true,
		}
		f.promotions = append(f.promotions, p)
		reply = map[string]any{"promotion": p}
	default:
		return &cloud.Error{Code: "unimplemented", Message: proc}
	}
	b, _ := json.Marshal(reply)
	return json.Unmarshal(b, out)
}

func (f *fakeRecordCP) called(short string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, c := range f.calls {
		if c.proc == short {
			out = append(out, c.body)
		}
	}
	return out
}

// useRecordLedger points openRecordLedger at l for the test.
func useRecordLedger(t *testing.T, open func(ctx context.Context, projectDir, project, env, token string, entities *KCLEntities) (recordLedger, error)) {
	t.Helper()
	prev := openRecordLedger
	openRecordLedger = open
	t.Cleanup(func() { openRecordLedger = prev })
}

func hostedRecordTarget(cp *fakeRecordCP) func(context.Context, string, string, string, string, *KCLEntities) (recordLedger, error) {
	return func(_ context.Context, _, project, _, _ string, _ *KCLEntities) (recordLedger, error) {
		return recordLedger{
			ledger:   hostedLedger(cp, cloud.DefaultEndpoint, project, deploytarget.HostedEnvSelfManaged),
			endpoint: cloud.DefaultEndpoint,
		}, nil
	}
}

// githubEnv is a GitHub Actions job's environment.
func githubEnv(key string) string {
	return map[string]string{
		"CI": "true", "GITHUB_REPOSITORY": "acme/shop", "GITHUB_RUN_ID": "77", "GITHUB_RUN_ATTEMPT": "1",
		"GITHUB_SHA": recordCommitA,
	}[key]
}

func recordScopeFor(t *testing.T, getenv func(string) string) recordScope {
	return recordScope{projectDir: t.TempDir(), project: "shop", getenv: getenv}
}

// The whole CI story against a control plane: a record cuts a record-only
// release with CI's digests and commit, ensures the env as SELF_MANAGED,
// promotes it as "ci" with the run, prints the console link — and a retry of
// the same step records nothing new.
func TestReleaseRecord_HostedCutsPromotesAndIsIdempotent(t *testing.T) {
	cp := newFakeRecordCP()
	useRecordLedger(t, hostedRecordTarget(cp))
	sc := recordScopeFor(t, githubEnv)
	o := releaseRecordOptions{env: "prod", version: "v1.4.0",
		workloads: []string{"api"}, images: []string{"ghcr.io/acme/api@" + recordDigestA}}

	res, err := recordReleaseIn(context.Background(), o, sc, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if !res.ReleaseCreated || !res.Promoted || res.Ledger != "hosted" {
		t.Errorf("first record: %+v", res)
	}
	if want := "https://forge.reliantlabs.io/forge/p/shop/releases/v1.4.0"; res.ConsoleURL != want {
		t.Errorf("console url = %q, want %q", res.ConsoleURL, want)
	}
	if res.Commit != recordCommitA || res.RunURL != "https://github.com/acme/shop/actions/runs/77" {
		t.Errorf("commit %q run %q: want CI's", res.Commit, res.RunURL)
	}

	cut := cp.called("CutRelease")[0]
	if cut["recordOnly"] != true || cut["project"] != "shop" || cut["gitCommit"] != recordCommitA {
		t.Errorf("CutRelease = %v; want recordOnly, project shop, CI's commit", cut)
	}
	if arts := cut["artifacts"].([]any); len(arts) != 1 || arts[0].(map[string]any)["digest"] != recordDigestA ||
		arts[0].(map[string]any)["uri"] != "ghcr.io/acme/api" {
		t.Errorf("CutRelease artifacts = %v; want api at CI's digest, repo as uri", arts)
	}
	ensure := cp.called("EnsureEnvironment")[0]["spec"].(map[string]any)
	if ensure["kind"] != string(deploytarget.HostedEnvSelfManaged) || ensure["project"] != "shop" {
		t.Errorf("EnsureEnvironment spec = %v; want a SELF_MANAGED env of project shop", ensure)
	}
	promote := cp.called("Promote")[0]
	if promote["promotedByActor"] != releaseRecordActorCI || promote["run"] == nil {
		t.Errorf("Promote = %v; want actor ci and the run", promote)
	}
	if _, ok := promote["planDigest"]; ok {
		t.Error("a record approved a plan it never computed")
	}

	again, err := recordReleaseIn(context.Background(), o, sc, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if again.ReleaseCreated || again.Promoted || again.PromotionID != res.PromotionID {
		t.Errorf("retry: %+v; want nothing new, the same promotion", again)
	}
	if n := len(cp.promotions); n != 1 {
		t.Errorf("the retry appended: %d promotions", n)
	}
}

// Version-only: no workloads, the commit from CI. The release names nothing
// and is still recorded — as record-only, which is what makes that valid.
func TestReleaseRecord_VersionOnly(t *testing.T) {
	cp := newFakeRecordCP()
	useRecordLedger(t, hostedRecordTarget(cp))
	res, err := recordReleaseIn(context.Background(),
		releaseRecordOptions{env: "prod", version: "v1.4.0"}, recordScopeFor(t, githubEnv), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	cut := cp.called("CutRelease")[0]
	if arts, _ := cut["artifacts"].([]any); cut["recordOnly"] != true || len(arts) != 0 {
		t.Errorf("CutRelease = %v; want a record-only release with no artifacts", cut)
	}
	if len(res.Workloads) != 0 {
		t.Errorf("workloads = %v, want none", res.Workloads)
	}
	var out bytes.Buffer
	printReleaseRecord(&out, res)
	if !strings.Contains(out.String(), "version-only") || !strings.Contains(out.String(), res.ConsoleURL) {
		t.Errorf("text output does not say version-only and link the console:\n%s", out.String())
	}
}

// Re-recording a version with other digests is a conflict, not an overwrite:
// a version names one release.
func TestReleaseRecord_DifferentDigestsConflict(t *testing.T) {
	cp := newFakeRecordCP()
	useRecordLedger(t, hostedRecordTarget(cp))
	sc := recordScopeFor(t, githubEnv)
	rec := func(d string) error {
		_, err := recordReleaseIn(context.Background(), releaseRecordOptions{env: "prod", version: "v1.4.0",
			workloads: []string{"api"}, images: []string{"ghcr.io/acme/api@" + d}}, sc, &bytes.Buffer{})
		return err
	}
	if err := rec(recordDigestA); err != nil {
		t.Fatal(err)
	}
	if err := rec(recordDigestB); !errors.Is(err, release.ErrReleaseConflict) {
		t.Fatalf("re-record with other digests: %v, want ErrReleaseConflict", err)
	}
}

// The control plane's refusal to bind a record to an env forge itself deploys
// reaches the user as the refusal, with its reason.
func TestReleaseRecord_RefusalOnAForgeDeployedEnv(t *testing.T) {
	cp := newFakeRecordCP()
	cp.promoteErr = &cloud.Error{Code: "failed_precondition", Reason: "record_only_release",
		Message: `release "v1.4.0" was recorded from an outside pipeline, not built by forge, and environment "prod" is deployed by forge`}
	useRecordLedger(t, hostedRecordTarget(cp))
	_, err := recordReleaseIn(context.Background(),
		releaseRecordOptions{env: "prod", version: "v1.4.0"}, recordScopeFor(t, githubEnv), &bytes.Buffer{})
	var refused *promoteRefusedError
	if !errors.As(err, &refused) || refused.Reason != "record_only_release" {
		t.Fatalf("err = %v, want the record_only_release refusal", err)
	}
}

// declaredRecordProject is a project whose deploy/kcl/<env>/main.k renders to
// entities (through the renderRecordEnv seam, so no KCL runs), with a
// temporary machine ledger. Not parallel: it swaps package-level seams.
func declaredRecordProject(t *testing.T, env string, entities *KCLEntities) recordScope {
	t.Helper()
	home := t.TempDir()
	prevHome := ledgerHome
	ledgerHome = func() (string, error) { return home, nil }
	t.Cleanup(func() { ledgerHome = prevHome })
	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "forge.yaml"), []byte("name: shop\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(projectDir, "deploy", "kcl", env), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "deploy", "kcl", env, "main.k"), []byte("# "+env+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prevRender := renderRecordEnv
	renderRecordEnv = func(_ context.Context, _, got string) (*KCLEntities, error) {
		if got != env {
			return nil, fmt.Errorf("rendered env %q, want %q", got, env)
		}
		return entities, nil
	}
	t.Cleanup(func() { renderRecordEnv = prevRender })
	return recordScope{projectDir: projectDir, project: "shop", getenv: func(string) string { return "" }}
}

// recordedNothing fails unless the machine ledger holds no release v1 and no
// binding of env.
func recordedNothing(t *testing.T, projectDir, env string) {
	t.Helper()
	l, err := machineLedger(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if rel, err := l.Releases.Get(context.Background(), "v1"); err == nil && rel != nil {
		t.Fatalf("a refused record cut release %+v", rel)
	}
	if _, bound, err := l.Bindings.Current(context.Background(), env); err != nil || bound {
		t.Fatalf("a refused record bound %s (bound=%v err=%v)", env, bound, err)
	}
}

func recordExternalWorkload(name string) WorkloadEntity {
	return WorkloadEntity{Name: name, Runtime: RuntimeEntity{Type: RuntimeExternal, External: &deploytarget.ExternalRuntime{}}}
}

// An env whose deploys the PLATFORM applies — a hosted part, a unit the hub
// converges, a Flux applier — is refused before anything is cut, on the
// machine ledger too, where no control plane would refuse it.
func TestReleaseRecord_RefusesPlatformAppliedEnvs(t *testing.T) {
	cases := []struct {
		name     string
		entities func() *KCLEntities
		want     string
	}{
		{"a hosted workload", func() *KCLEntities {
			return &KCLEntities{
				ControlPlane: &ControlPlaneEntity{Endpoint: "https://cp.example"},
				Workloads:    []WorkloadEntity{{Name: "api", Runtime: RuntimeEntity{Type: RuntimeHosted, Hosted: &HostedRuntime{}}}},
			}
		}, "Reliant hosting"},
		{"a hosted frontend beside an OnExternal workload", func() *KCLEntities {
			return &KCLEntities{
				ControlPlane: &ControlPlaneEntity{Endpoint: "https://cp.example"},
				Workloads:    []WorkloadEntity{recordExternalWorkload("api")},
				Frontends:    []FrontendEntity{{Name: "web", Runtime: FrontendRuntime{Type: FrontendRuntimeHosted}}},
			}
		}, "Reliant hosting"},
		{"a cluster the hub converges", prodShaped, "hub"},
		{"an applier = flux env", func() *KCLEntities {
			e := prodShaped()
			e.Applier, e.FluxDelivery = applierForge, &FluxDeliveryEntity{}
			return e
		}, "flux"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := declaredRecordProject(t, "prod", tc.entities())
			t.Setenv(ledgerSelectionEnv, string(ledgerSelectMachine))
			_, err := recordReleaseIn(context.Background(),
				releaseRecordOptions{env: "prod", version: "v1", commit: recordCommitA}, sc, &bytes.Buffer{})
			var refused *promoteRefusedError
			if !errors.As(err, &refused) || refused.Reason != reasonRecordOnlyRelease {
				t.Fatalf("err = %v, want the record_only_release refusal", err)
			}
			if !strings.Contains(refused.Detail, tc.want) {
				t.Errorf("detail %q does not say why (%q)", refused.Detail, tc.want)
			}
			recordedNothing(t, sc.projectDir, "prod")
		})
	}
}

// An env the forge CLI applies takes a record: forge.OnExternal (the team's
// own tooling deploys it), and a cluster forge applies from CI. The control
// plane's own refusal stays the final check on a hosted ledger.
func TestReleaseRecord_AllowsCLIAppliedEnvs(t *testing.T) {
	t.Run("OnExternal, no control plane: the machine ledger its KCL selects, and a note", func(t *testing.T) {
		sc := declaredRecordProject(t, "prod", &KCLEntities{Workloads: []WorkloadEntity{recordExternalWorkload("api")}})
		t.Setenv(ledgerSelectionEnv, "")
		var notes bytes.Buffer
		res, err := recordReleaseIn(context.Background(),
			releaseRecordOptions{env: "prod", version: "v1", commit: recordCommitA}, sc, &notes)
		if err != nil {
			t.Fatalf("record into an OnExternal env: %v", err)
		}
		if res.Ledger != "machine" || !res.ReleaseCreated || !res.Promoted {
			t.Errorf("result %+v, want a new record on the machine ledger", res)
		}
		if !strings.Contains(notes.String(), "declares no forge.ControlPlane") {
			t.Errorf("notes %q do not say why monitoring cannot see it", notes.String())
		}
	})
	t.Run("OnExternal with a control plane: recorded there, as the env forge deploys", func(t *testing.T) {
		entities := &KCLEntities{
			ControlPlane: &ControlPlaneEntity{Endpoint: "https://cp.example"},
			Workloads:    []WorkloadEntity{recordExternalWorkload("api")},
		}
		sc := declaredRecordProject(t, "prod", entities)
		cp := newFakeRecordCP()
		var gotEntities *KCLEntities
		useRecordLedger(t, func(ctx context.Context, projectDir, project, env, token string, e *KCLEntities) (recordLedger, error) {
			gotEntities = e
			return hostedRecordTarget(cp)(ctx, projectDir, project, env, token, e)
		})
		if _, err := recordReleaseIn(context.Background(),
			releaseRecordOptions{env: "prod", version: "v1", commit: recordCommitA}, sc, &bytes.Buffer{}); err != nil {
			t.Fatalf("record: %v", err)
		}
		if gotEntities != entities {
			t.Errorf("the ledger was chosen without the env's render (%v)", gotEntities)
		}
		if len(cp.called("Promote")) != 1 {
			t.Errorf("want one Promote, got %v", cp.called("Promote"))
		}
	})
	t.Run("a cluster forge applies", func(t *testing.T) {
		e := prodShaped()
		e.Applier = applierForge
		sc := declaredRecordProject(t, "prod", e)
		t.Setenv(ledgerSelectionEnv, string(ledgerSelectMachine))
		if _, err := recordReleaseIn(context.Background(),
			releaseRecordOptions{env: "prod", version: "v1", commit: recordCommitA}, sc, &bytes.Buffer{}); err != nil {
			t.Fatalf("record into a forge-applied cluster env: %v", err)
		}
	})
	t.Run("an undeclared env", func(t *testing.T) {
		sc := declaredRecordProject(t, "prod", nil)
		t.Setenv(ledgerSelectionEnv, string(ledgerSelectMachine))
		if _, err := recordReleaseIn(context.Background(),
			releaseRecordOptions{env: "staging", version: "v1", commit: recordCommitA}, sc, &bytes.Buffer{}); err != nil {
			t.Fatalf("an undeclared env: %v", err)
		}
	})
}

// A declared env whose render fails is an error, never "undeclared": that
// would skip the refusal for an env the platform may apply.
func TestReleaseRecord_ARenderFailureIsAnError(t *testing.T) {
	sc := declaredRecordProject(t, "prod", nil)
	renderRecordEnv = func(context.Context, string, string) (*KCLEntities, error) { return nil, errors.New("kcl: boom") }
	t.Setenv(ledgerSelectionEnv, string(ledgerSelectMachine))
	_, err := recordReleaseIn(context.Background(),
		releaseRecordOptions{env: "prod", version: "v1", commit: recordCommitA}, sc, &bytes.Buffer{})
	if err == nil || !strings.Contains(err.Error(), "kcl: boom") {
		t.Fatalf("err = %v, want the render failure", err)
	}
	recordedNothing(t, sc.projectDir, "prod")
}

// A control plane whose record-only rollout switch is off declines the cut.
// The record says exactly that — nothing was saved — exits exitRefused, and
// never falls back to the machine ledger or reaches Promote.
func TestReleaseRecord_RecordOnlyDisabledOnTheControlPlane(t *testing.T) {
	cp := newFakeRecordCP()
	cp.cutErr = &cloud.Error{Code: "failed_precondition", Reason: reasonRecordOnlyReleasesDisabled,
		Message: "record-only releases are not enabled on this control plane"}
	useRecordLedger(t, hostedRecordTarget(cp))
	_, err := recordReleaseIn(context.Background(),
		releaseRecordOptions{env: "prod", version: "v1.4.0"}, recordScopeFor(t, githubEnv), &bytes.Buffer{})
	var disabled *recordOnlyDisabledError
	if !errors.As(err, &disabled) {
		t.Fatalf("err = %v, want recordOnlyDisabledError", err)
	}
	if disabled.ExitCode() != exitRefused {
		t.Errorf("exit %d, want exitRefused (%d)", disabled.ExitCode(), exitRefused)
	}
	for _, want := range []string{"has not enabled recorded deploys yet", "v1.4.0 deployed to prod was NOT saved", cloud.DefaultEndpoint} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("message %q lacks %q", err.Error(), want)
		}
	}
	if got := cp.called("Promote"); len(got) != 0 {
		t.Errorf("a declined cut promoted: %v", got)
	}
	// The same switch declines a ledger import that carries a recorded
	// release, and the import says so the same way.
	_, err = hostedImportClient{client: callerFunc(func(context.Context, string, any, any) error {
		return cp.cutErr
	})}.ImportLedger(context.Background(), &ledgerImport{project: "shop"})
	if !errors.As(err, &disabled) || disabled.ExitCode() != exitRefused || !strings.Contains(err.Error(), "this import") {
		t.Errorf("import err = %v, want the record_only_releases_disabled refusal naming the import", err)
	}
	if exitCodeForRefusal(reasonRecordOnlyReleasesDisabled) != exitRefused {
		t.Errorf("the refusal table maps %s to %d, want exitRefused", reasonRecordOnlyReleasesDisabled, exitCodeForRefusal(reasonRecordOnlyReleasesDisabled))
	}
}

// Not signed in, or FORGE_LEDGER=machine: the record lands in this machine's
// ledger, record-only, idempotently — and a version-only record at another
// commit is a conflict there too.
func TestReleaseRecord_MachineLedger(t *testing.T) {
	home := t.TempDir()
	prevHome := ledgerHome
	ledgerHome = func() (string, error) { return home, nil }
	t.Cleanup(func() { ledgerHome = prevHome })
	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "forge.yaml"), []byte("name: shop\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(ledgerSelectionEnv, string(ledgerSelectMachine))
	sc := recordScope{projectDir: projectDir, project: "shop", getenv: func(string) string { return "" }}
	o := releaseRecordOptions{env: "prod", version: "v1.4.0", commit: recordCommitA}

	var notes bytes.Buffer
	res, err := recordReleaseIn(context.Background(), o, sc, &notes)
	if err != nil {
		t.Fatalf("record: %v", err)
	}
	if res.Ledger != "machine" || res.ConsoleURL != "" || !res.ReleaseCreated || !res.Promoted {
		t.Errorf("machine record: %+v", res)
	}
	again, err := recordReleaseIn(context.Background(), o, sc, &notes)
	if err != nil || again.ReleaseCreated || again.Promoted {
		t.Fatalf("retry: %+v %v; want nothing new", again, err)
	}

	l, err := machineLedger(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	cur, bound, err := l.Bindings.Current(context.Background(), "prod")
	if err != nil || !bound || cur.Release != "v1.4.0" || !cur.RecordOnly {
		t.Fatalf("current = %+v bound=%v err=%v; want the record-only v1.4.0", cur, bound, err)
	}
	if cur.PromotedBy.Actor != "" {
		t.Errorf("outside CI the actor is the person, not %q", cur.PromotedBy.Actor)
	}
	rel, err := l.Releases.Get(context.Background(), "v1.4.0")
	if err != nil || rel == nil || !rel.RecordOnly || rel.Git.Commit != recordCommitA {
		t.Fatalf("release = %+v %v; want record-only at the commit", rel, err)
	}

	o.commit = recordCommitB
	if _, err := recordReleaseIn(context.Background(), o, sc, &notes); !errors.Is(err, release.ErrReleaseConflict) {
		t.Fatalf("version-only at another commit: %v, want ErrReleaseConflict", err)
	}
}

func TestRecordArtifacts(t *testing.T) {
	t.Parallel()
	arts, named, err := recordArtifacts([]string{"api", "web"},
		[]string{"ghcr.io/acme/api:v1@" + recordDigestA, "ghcr.io/acme/web@" + recordDigestB})
	if err != nil {
		t.Fatal(err)
	}
	if d, _ := arts["api"].SharedDigest(); d != recordDigestA || arts["api"].URI != "ghcr.io/acme/api:v1" {
		t.Errorf("api = %+v", arts["api"])
	}
	if named["web"] != "ghcr.io/acme/web@"+recordDigestB {
		t.Errorf("web = %q", named["web"])
	}
	for name, tc := range map[string]struct{ w, i []string }{
		"unpaired":     {[]string{"api"}, nil},
		"tag only":     {[]string{"api"}, []string{"ghcr.io/acme/api:v1"}},
		"short digest": {[]string{"api"}, []string{"ghcr.io/acme/api@sha256:abc"}},
		"duplicate":    {[]string{"api", "api"}, []string{"r@" + recordDigestA, "r@" + recordDigestB}},
	} {
		if _, _, err := recordArtifacts(tc.w, tc.i); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRecordCommitAndRun(t *testing.T) {
	t.Parallel()
	if c, err := recordCommit("", githubEnv); err != nil || c != recordCommitA {
		t.Errorf("commit from CI = %q %v", c, err)
	}
	if _, err := recordCommit("main", githubEnv); err == nil {
		t.Error("a branch name was accepted as a commit")
	}
	none := func(string) string { return "" }
	run, err := recordRun(runOptions{URL: "https://ci.example/build/9"}, none)
	if err != nil || run.ID != "https://ci.example/build/9" || run.URL != run.ID {
		t.Errorf("a --url outside a known CI = %+v %v; want the url as its id", run, err)
	}
	bk := func(k string) string {
		return map[string]string{"BUILDKITE_BUILD_NUMBER": "12", "BUILDKITE_PIPELINE_SLUG": "shop",
			"BUILDKITE_BUILD_URL": "https://buildkite.com/acme/shop/builds/12"}[k]
	}
	if run, err := recordRun(runOptions{}, bk); err != nil || run.ID != "buildkite:shop/12" || run.Provider != "buildkite" {
		t.Errorf("buildkite run = %+v %v", run, err)
	}
}

func TestReleaseConsoleURL(t *testing.T) {
	t.Setenv("FORGE_CONSOLE_URL", "")
	if got := releaseConsoleURL(cloud.DefaultEndpoint, "shop", "v1.4.0+build/7"); got !=
		"https://forge.reliantlabs.io/forge/p/shop/releases/v1.4.0+build%2F7" {
		t.Errorf("console url = %q", got)
	}
	if got := releaseConsoleURL("https://cp.internal.example", "shop", "v1"); got != "" {
		t.Errorf("another control plane with no FORGE_CONSOLE_URL linked %q", got)
	}
	t.Setenv("FORGE_CONSOLE_URL", "https://console.staging.example/")
	if got := releaseConsoleURL("https://cp.internal.example", "shop", "v1"); got != "https://console.staging.example/forge/p/shop/releases/v1" {
		t.Errorf("FORGE_CONSOLE_URL = %q", got)
	}
}
