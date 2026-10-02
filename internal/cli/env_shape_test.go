package cli

// Tests for `forge env shape` and for the DECLARATION every hosted write path
// now carries.
//
// The property under test is not "the projection is correct" — internal/bundle
// owns that, from a literal fixture. It is that the right commands record a
// declaration, at the right moment, and that the ones which have not rendered
// the env do NOT: an absent shape leaves the stored one untouched server-side,
// so a `forge secret set` that sent one would overwrite a build's declaration
// with whatever it could infer without rendering.

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/cloud"
	"github.com/reliant-labs/forge/internal/deploytarget"
	"github.com/reliant-labs/forge/internal/kclplugin"
	"github.com/reliant-labs/forge/pkg/release"
)

const procEnsureEnv = "controlplane.v1.DeployService/EnsureEnvironment"

// ─── `forge env shape` over a real project ──────────────────────────────────

// TestEnvShapeJSONProjectsTheRenderedEnvironment runs the command over the
// same fixture project `forge env render`'s stdout test uses: a prod env with
// a Deployment and a Job, a recorded build state, and a promotion that pins a
// different digest.
//
// It is the slow lane — it evaluates KCL — so it is gated under -short, like
// the render tests beside it.
func TestEnvShapeJSONProjectsTheRenderedEnvironment(t *testing.T) {
	if testing.Short() {
		t.Skip("evaluates KCL; the projection itself is covered from a literal fixture in internal/bundle")
	}
	kclplugin.Register()
	dir := writeRenderStdoutProject(t)

	stdout, stderr, err := runEnvShapeCapturingProcessStdout(t, dir, "prod", "--json")
	if err != nil {
		t.Fatalf("forge env shape prod --json: %v\nstderr:\n%s", err, stderr)
	}

	// STDOUT CARRIES ONLY THE DOCUMENT. The render path prints notes with
	// fmt.Printf in dozens of places — this fixture fires the
	// release-override one — and a single prose line makes the JSON
	// unparseable for the daemon hook that reads it.
	var doc envShapeDoc
	if jerr := json.Unmarshal([]byte(stdout), &doc); jerr != nil {
		t.Fatalf("stdout is not the JSON document (%v):\n%s", jerr, stdout)
	}
	if !strings.Contains(stderr, "deploying the RELEASE") {
		t.Errorf("the release-override Note is still true and belongs on stderr; got stderr:\n%s", stderr)
	}

	if doc.Env != "prod" || doc.Project != "rendertest" {
		t.Errorf("got env %q project %q, want prod / rendertest", doc.Env, doc.Project)
	}
	// Nothing hosted and nothing on a cluster the platform runs, but the
	// workloads are OnCluster — so this env is SELF-MANAGED, which is the
	// distinction the kind split exists to make.
	if doc.Kind != string(release.EnvSelfManaged) {
		t.Errorf("kind = %q, want %s", doc.Kind, release.EnvSelfManaged)
	}
	if doc.Shape.Kind != release.EnvKind(doc.Kind) {
		t.Errorf("the shape's kind %q disagrees with the document's %q", doc.Shape.Kind, doc.Kind)
	}

	byName := map[string]release.ShapeWorkload{}
	for _, w := range doc.Shape.Workloads {
		byName[w.Name] = w
	}
	for _, name := range []string{"api", "migrate"} {
		w, ok := byName[name]
		if !ok {
			t.Fatalf("workload %q missing from the shape (%v)", name, byName)
		}
		if w.Runtime != "cluster" || w.Cluster != "k3d-rendertest" {
			t.Errorf("workload %s: runtime %q on %q, want cluster on k3d-rendertest", name, w.Runtime, w.Cluster)
		}
	}

	kinds := map[string]int{}
	for _, o := range doc.Shape.Objects {
		kinds[o.Kind]++
		if !release.ValidDigest(o.Hash) {
			t.Errorf("%s %s: hash %q is not a canonical digest", o.Kind, o.Name, o.Hash)
		}
		if o.Cluster != "k3d-rendertest" {
			t.Errorf("%s %s: cluster %q, want k3d-rendertest", o.Kind, o.Name, o.Cluster)
		}
	}
	for _, want := range []string{"Deployment", "Job"} {
		if kinds[want] == 0 {
			t.Errorf("the shape carries no %s; kinds = %v", want, kinds)
		}
	}

	// The shape is projected from the RELEASE's bytes, as a deploy would
	// ship them — so the release-bound digest is what the objects' Images
	// record, and the promoted digest is what ConfigHash normalizes away.
	api := shapeObjectNamed(t, doc.Shape, "Deployment", "api")
	if got := api.Images["reg.example.com/rendertest"]; got != renderReleaseDigest {
		t.Errorf("Deployment api images = %v, want the release digest %s", api.Images, renderReleaseDigest)
	}
	if !release.ValidDigest(api.ConfigHash) {
		t.Errorf("Deployment api config hash %q is not a canonical digest", api.ConfigHash)
	}
	if doc.Provenance.ForgeVersion == "" {
		t.Error("the declaration records no forge version: a render is a function of (forge, KCL, config)")
	}
}

// TestEnvShapeTextSummaryNamesTheEnvironment: the default form is read by a
// person deciding something, so it has to answer "what is this env" without
// them parsing JSON.
func TestEnvShapeTextSummaryNamesTheEnvironment(t *testing.T) {
	if testing.Short() {
		t.Skip("evaluates KCL")
	}
	kclplugin.Register()
	dir := writeRenderStdoutProject(t)

	stdout, stderr, err := runEnvShapeCapturingProcessStdout(t, dir, "prod")
	if err != nil {
		t.Fatalf("forge env shape prod: %v\nstderr:\n%s", err, stderr)
	}
	for _, want := range []string{"env prod", string(release.EnvSelfManaged), "k3d-rendertest", "api", "objects:"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the summary does not mention %q:\n%s", want, stdout)
		}
	}
}

// runEnvShapeCapturingProcessStdout runs the command with cobra's output left
// UNSET — the real CLI's configuration — and captures the PROCESS's two
// streams. Capturing file descriptors rather than a cobra buffer is the
// point: the notes that would corrupt the document are fmt.Printf calls,
// which never go near cobra's writer.
func runEnvShapeCapturingProcessStdout(t *testing.T, dir, env string, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	t.Chdir(dir)

	outR, outW, perr := os.Pipe()
	if perr != nil {
		t.Fatal(perr)
	}
	errR, errW, perr := os.Pipe()
	if perr != nil {
		t.Fatal(perr)
	}
	realOut, realErr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = outW, errW
	outC, errC := make(chan string), make(chan string)
	go func() { b, _ := io.ReadAll(outR); outC <- string(b) }()
	go func() { b, _ := io.ReadAll(errR); errC <- string(b) }()

	cmd := newEnvShapeCmd()
	cmd.SetArgs(append([]string{env}, args...))
	cmd.SetContext(context.Background())
	err = cmd.Execute()

	os.Stdout, os.Stderr = realOut, realErr
	outW.Close()
	errW.Close()
	return <-outC, <-errC, err
}

func shapeObjectNamed(t *testing.T, shape release.Shape, kind, name string) release.ShapeObject {
	t.Helper()
	for _, o := range shape.Objects {
		if o.Kind == kind && o.Name == name {
			return o
		}
	}
	t.Fatalf("no %s named %q among %d object(s)", kind, name, len(shape.Objects))
	return release.ShapeObject{}
}

// ─── Recording ──────────────────────────────────────────────────────────────

// withDeclarationRecorder points the declaration path at a fake control
// plane, and makes the projection return a fixed shape rather than rendering.
// What these tests pin is the WIRE — which call carries which fields, in
// which order — so rendering a real project here would only make them slow
// and flaky without testing anything internal/bundle does not already cover.
func withDeclarationRecorder(t *testing.T, shape *release.Shape) *fakeCPCaller {
	t.Helper()
	fake := &fakeCPCaller{ensureID: "env-declared"}
	prevClient, prevProject := envDeclarationClient, projectEnvShapeFn
	envDeclarationClient = func(string, *KCLEntities) (cloudCaller, string, error) {
		return fake, "https://cp.example.test", nil
	}
	projectEnvShapeFn = func(_ context.Context, _ io.Writer, envName string) (envShapeDoc, error) {
		return envShapeDoc{
			Project: "acme", Env: envName, Kind: string(shape.Kind), Shape: *shape,
			Provenance: release.Provenance{
				Repo:         "github.com/acme/shop",
				Commit:       "1111111111111111111111111111111111111111",
				Branch:       "main",
				Tree:         "2222222222222222222222222222222222222222",
				ForgeVersion: "v9.9.9",
				Worktree:     release.Worktree{Key: "wt", Label: "main", Host: "host-1", Path: "/Users/someone/src/shop"},
			},
		}, nil
	}
	t.Cleanup(func() { envDeclarationClient, projectEnvShapeFn = prevClient, prevProject })
	return fake
}

func declarationFixtureShape() *release.Shape {
	shape := release.Shape{
		Kind:      release.EnvSelfManaged,
		Workloads: []release.ShapeWorkload{{Name: "api", Runtime: "cluster", Cluster: "gke-prod", Artifact: "api"}},
		Secrets:   []release.ShapeSecret{{Name: "DATABASE_URL", Provider: "external", DeclaredBy: []string{"api"}}},
		Clusters:  []string{"gke-prod"},
		Objects: []release.ShapeObject{{
			Cluster: "gke-prod", APIVersion: "apps/v1", Kind: "Deployment", Namespace: "shop", Name: "api",
			Hash: "sha256:" + strings.Repeat("a", 64),
		}},
	}.Canonical()
	return &shape
}

// TestRecordEnvDeclarationSendsTheShapeAndItsProvenance is the write the
// slice exists for. After it, the control plane knows what the project
// declares for this env — which is what lets a Live view answer "what kind,
// which secrets, which provider" with no checkout and no daemon.
func TestRecordEnvDeclarationSendsTheShapeAndItsProvenance(t *testing.T) {
	shape := declarationFixtureShape()
	fake := withDeclarationRecorder(t, shape)
	withProjectName(t, "acme")

	entities := &KCLEntities{
		ControlPlane: &ControlPlaneEntity{Type: "control_plane", Endpoint: "https://cp.example.test"},
		Workloads:    []WorkloadEntity{{Name: "api", Runtime: RuntimeEntity{Type: RuntimeCluster, Cluster: &ClusterRuntime{Cluster: "gke-prod"}}}},
	}
	if err := recordEnvDeclaration(context.Background(), "prod", entities); err != nil {
		t.Fatalf("recordEnvDeclaration: %v", err)
	}

	ensures := fake.callsTo(procEnsureEnv)
	if len(ensures) != 1 {
		t.Fatalf("EnsureEnvironment calls = %d, want 1", len(ensures))
	}
	spec, _ := ensures[0].Body["spec"].(map[string]any)
	if spec["name"] != "prod" || spec["project"] != "acme" {
		t.Errorf("spec addresses %v, want name=prod project=acme", spec)
	}
	// The env binds nothing hosted and runs on a cluster the author
	// operates, so it is SELF_MANAGED — and its secrets are therefore
	// write-only, which is the whole reason that kind exists.
	if spec["kind"] != string(deploytarget.HostedEnvSelfManaged) {
		t.Errorf("spec kind = %v, want %s", spec["kind"], deploytarget.HostedEnvSelfManaged)
	}

	// The shape rides as a Struct, whose proto3-JSON form is the object
	// itself — and it must be the CANONICAL encoding, byte for byte what
	// `forge env shape` prints, so the stored shape and the printed one
	// cannot differ.
	sent, ok := spec["shape"].(map[string]any)
	if !ok {
		t.Fatalf("spec carries no shape: %v", spec)
	}
	canonical, err := shape.Encode()
	if err != nil {
		t.Fatal(err)
	}
	sentJSON, err := json.Marshal(sent)
	if err != nil {
		t.Fatal(err)
	}
	var want, got any
	if err := json.Unmarshal(canonical, &want); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(sentJSON, &got); err != nil {
		t.Fatal(err)
	}
	wantRe, _ := json.Marshal(want)
	gotRe, _ := json.Marshal(got)
	if string(wantRe) != string(gotRe) {
		t.Errorf("the shape on the wire is not the canonical encoding:\n sent: %s\n want: %s", gotRe, wantRe)
	}

	declaredBy, ok := spec["declaredBy"].(map[string]any)
	if !ok {
		t.Fatalf("spec carries no declaredBy: %v", spec)
	}
	for field, want := range map[string]any{
		"repo": "github.com/acme/shop", "branch": "main", "forgeVersion": "v9.9.9",
		"commit": "1111111111111111111111111111111111111111",
	} {
		if declaredBy[field] != want {
			t.Errorf("declaredBy[%s] = %v, want %v", field, declaredBy[field], want)
		}
	}
	// THE WORKTREE PATH NEVER LEAVES THE MACHINE. It names a person's home
	// directory, and this record is read by everyone in the org.
	if raw, _ := json.Marshal(declaredBy); strings.Contains(string(raw), "/Users/") {
		t.Errorf("declaredBy carries a filesystem path: %s", raw)
	}
	worktree, _ := declaredBy["worktree"].(map[string]any)
	if worktree["hostId"] != "host-1" || worktree["key"] != "wt" {
		t.Errorf("declaredBy worktree = %v, want key=wt hostId=host-1", worktree)
	}
}

// TestRecordEnvDeclarationSkipsAnEnvWithNoControlPlane: an env whose ledger
// is the project's own files records nothing and fails at nothing. Refusing
// here would make `forge env build` depend on a control plane that a purely
// local env has no reason to have.
func TestRecordEnvDeclarationSkipsAnEnvWithNoControlPlane(t *testing.T) {
	fake := withDeclarationRecorder(t, declarationFixtureShape())
	if err := recordEnvDeclaration(context.Background(), "dev", &KCLEntities{}); err != nil {
		t.Fatalf("an env with no control plane must record nothing, not fail: %v", err)
	}
	if n := len(fake.callsTo(procEnsureEnv)); n != 0 {
		t.Errorf("an env with no control plane ensured %d environment(s)", n)
	}
}

// TestRecordEnvDeclarationFailsLoudlyOnARefusedShape is F-15's forge half.
//
// A control plane that PREDATES the shape ignores the field — connect's JSON
// codec discards unknown fields — so there is nothing to special-case for an
// old server. An InvalidArgument is the opposite situation: the server read
// the shape and refused it, and continuing would leave every Live surface
// reading a stale declaration while the build reported success.
func TestRecordEnvDeclarationFailsLoudlyOnARefusedShape(t *testing.T) {
	shape := declarationFixtureShape()
	fake := withDeclarationRecorder(t, shape)
	fake.ensureErr = &cloud.Error{HTTPStatus: 400, Code: cloud.CodeInvalidArgument, Message: "declared shape: unknown identity key"}

	entities := &KCLEntities{ControlPlane: &ControlPlaneEntity{Type: "control_plane", Endpoint: "https://cp.example.test"}}
	err := recordEnvDeclaration(context.Background(), "prod", entities)
	if err == nil {
		t.Fatal("a refused shape must fail the command, never be dropped silently")
	}
	if !strings.Contains(err.Error(), "prod") || !strings.Contains(err.Error(), "https://cp.example.test") {
		t.Errorf("the failure must name the env and the control plane: %v", err)
	}
}

// ─── The ensures that must NOT carry a shape ────────────────────────────────

// TestEnsureWithoutARenderSendsNoShape: `forge secret set`, a promote and
// every other ensure that has not rendered the env send no shape at all.
//
// This is load-bearing rather than tidy. An absent shape leaves the stored
// one untouched server-side, so an ensure that sent an empty or inferred one
// would ERASE the declaration a build recorded — and the Live view would go
// blank the first time anyone set a secret.
func TestEnsureWithoutARenderSendsNoShape(t *testing.T) {
	fake := &fakeCPCaller{ensureID: "env-1"}
	if _, err := ensureHostedEnv(context.Background(), fake, deploytarget.HostedEnvRef{
		Project: "acme", Name: "prod", Kind: deploytarget.HostedEnvPersistent,
	}); err != nil {
		t.Fatal(err)
	}
	spec, _ := fake.callsTo(procEnsureEnv)[0].Body["spec"].(map[string]any)
	if _, present := spec["shape"]; present {
		t.Errorf("an ensure with no render sent a shape, which would overwrite the recorded declaration: %v", spec)
	}
	if _, present := spec["declaredBy"]; present {
		t.Errorf("an ensure with no render sent declaredBy: %v", spec)
	}
}

// TestHostedSecretSetSendsNoShape is the same rule through the real command,
// because that is the caller it actually protects.
func TestHostedSecretSetSendsNoShape(t *testing.T) {
	fake, _ := hostedFixture(t, nil)
	withProjectName(t, "acme")
	if err := runSecretSet(context.Background(), "dev", "K", "", strings.NewReader("v"), io.Discard); err != nil {
		t.Fatal(err)
	}
	ensures := fake.callsTo("controlplane.v1.DeployService/EnsureEnvironment")
	if len(ensures) == 0 {
		t.Fatal("`forge secret set` did not ensure the env, so this test proves nothing")
	}
	for _, call := range ensures {
		spec, _ := call.Body["spec"].(map[string]any)
		if _, present := spec["shape"]; present {
			t.Errorf("`forge secret set` sent a shape: %v", spec)
		}
		if _, present := spec["declaredBy"]; present {
			t.Errorf("`forge secret set` sent declaredBy: %v", spec)
		}
	}
}

// ─── The release ledger's own provenance ────────────────────────────────────

// TestHostedCutSendsProvenanceAndProject: a release's own source, recorded
// beside the three legacy git fields rather than instead of them.
func TestHostedCutSendsProvenanceAndProject(t *testing.T) {
	fake := &fakeCPCaller{ensureID: "env-1"}
	store := &hostedStore{client: fake, endpoint: "https://cp.example.test", project: "acme", kind: deploytarget.HostedEnvPersistent}

	rel := release.Release{
		Version: "v1.0.0", Project: "acme",
		Artifacts: map[string]release.Artifact{
			"api": {Kind: release.KindOCI, Mode: release.ModeShared, Digests: map[string]string{release.SharedVariant: "sha256:" + strings.Repeat("b", 64)}},
		},
	}
	rel.SetProvenance(release.Provenance{
		Repo: "github.com/acme/shop", Commit: "1111111111111111111111111111111111111111",
		Branch: "main", Dirty: true, Tree: "2222222222222222222222222222222222222222",
		ForgeVersion: "v9.9.9", Worktree: release.Worktree{Key: "wt", Path: "/Users/someone/src/shop"},
	})
	if _, err := store.Cut(context.Background(), rel); err != nil {
		t.Fatalf("Cut: %v", err)
	}

	body := fake.callsTo("controlplane.v1.DeployService/CutRelease")[0].Body
	// The project the version belongs to. A release's identity is (org,
	// version), so without it a second project cutting v1.0.0 collides
	// with the first.
	if body["project"] != "acme" {
		t.Errorf("CutRelease project = %v, want acme", body["project"])
	}
	prov, ok := body["provenance"].(map[string]any)
	if !ok {
		t.Fatalf("CutRelease carries no provenance: %v", body)
	}
	if prov["commit"] != "1111111111111111111111111111111111111111" || prov["dirty"] != true {
		t.Errorf("provenance = %v, want the commit and dirty=true", prov)
	}
	if raw, _ := json.Marshal(prov); strings.Contains(string(raw), "/Users/") {
		t.Errorf("CutRelease provenance carries a filesystem path: %s", raw)
	}
	// The legacy trio stays, filled from the same provenance: dropping it
	// would make a new forge record LESS than an old one against a control
	// plane that predates provenance.
	if body["gitCommit"] != "1111111111111111111111111111111111111111" || body["gitDirty"] != true {
		t.Errorf("the legacy git fields must stay filled from the provenance: %v", body)
	}
}

// TestHostedReleaseReadsAreProjectScoped: a version is unique per ORG, so an
// unscoped read in an org with two forge projects can return the other
// project's release — whose artifacts a deploy would then pin.
func TestHostedReleaseReadsAreProjectScoped(t *testing.T) {
	fake := &fakeCPCaller{}
	store := &hostedStore{client: fake, endpoint: "https://cp.example.test", project: "acme", kind: deploytarget.HostedEnvPersistent}

	if _, err := store.Get(context.Background(), "v1.0.0"); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if _, err := store.List(context.Background()); err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, proc := range []string{"controlplane.v1.DeployService/GetRelease", "controlplane.v1.DeployService/ListReleases"} {
		calls := fake.callsTo(proc)
		if len(calls) != 1 {
			t.Fatalf("%s calls = %d, want 1", proc, len(calls))
		}
		if calls[0].Body["project"] != "acme" {
			t.Errorf("%s is not project-scoped: %v", proc, calls[0].Body)
		}
	}
}

// ─── Command surface ────────────────────────────────────────────────────────

// TestEnvShapeIsRegisteredAndDocumented: the daemon's `forge.env_shape` hook
// and Preview's Register both shell out to this verb by name, so its absence
// would surface as a UI that cannot bootstrap an environment.
func TestEnvShapeIsRegisteredAndDocumented(t *testing.T) {
	var found bool
	for _, sub := range newEnvCmd().Commands() {
		if strings.HasPrefix(sub.Use, "shape ") {
			found = true
			if sub.Flags().Lookup("json") == nil {
				t.Error("`forge env shape` must carry --json: it is what the daemon hook reads")
			}
			if !strings.Contains(sub.Long, "NO SECRET VALUE") {
				t.Error("`forge env shape --help` must state that it carries no secret value")
			}
		}
	}
	if !found {
		t.Fatal("`forge env shape` is not registered under `forge env`")
	}
}

// TestEnvBuildRecordsTheDeclarationBeforeCuttingAnything pins the ORDERING,
// which is the contract: a build that fails half-way has still told the
// control plane what the env is, because that is the fact every other surface
// needs and it costs one idempotent RPC.
//
// It drives the real `forge env build` command rather than calling the
// declaration helper, because the helper existing is not the property — the
// COMMAND calling it is. A version of this test that invoked
// recordEnvBuildDeclaration directly passed with the call site deleted from
// RunE, which is exactly the regression it is supposed to catch.
//
// The cut that follows FAILS (there is no project to render), and that is the
// point: the assertion is that the declaration landed anyway.
func TestEnvBuildRecordsTheDeclarationBeforeCuttingAnything(t *testing.T) {
	shape := declarationFixtureShape()
	fake := withDeclarationRecorder(t, shape)
	withProjectName(t, "acme")
	withDeclaredEntities(t, &KCLEntities{
		ControlPlane: &ControlPlaneEntity{Type: "control_plane", Endpoint: "https://cp.example.test"},
		Workloads:    []WorkloadEntity{{Name: "api", Runtime: RuntimeEntity{Type: RuntimeCluster, Cluster: &ClusterRuntime{Cluster: "gke-prod"}}}},
	})
	t.Chdir(t.TempDir())

	cmd := newEnvBuildCmd()
	// --no-build is the cut-only half of a pipeline, and the one path that
	// reaches the recording without compiling anything or needing the
	// build feature gate.
	cmd.SetArgs([]string{"prod", "--release", "v1.0.0", "--no-build"})
	cmd.SetContext(context.Background())
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	buildErr := cmd.Execute()

	calls := fake.calls
	if len(calls) == 0 {
		t.Fatalf("`forge env build prod` made no control-plane call, so it recorded no declaration (build error: %v)", buildErr)
	}
	if calls[0].Proc != procEnsureEnv {
		t.Fatalf("the declaration must be the FIRST control-plane call a build makes; got %v", procNames(calls))
	}
	spec, _ := calls[0].Body["spec"].(map[string]any)
	if _, present := spec["shape"]; !present {
		t.Errorf("env build's ensure carries no shape: %v", spec)
	}
	if _, present := spec["declaredBy"]; !present {
		t.Errorf("env build's ensure carries no declaredBy: %v", spec)
	}
}

// TestEnvBuildPlanRecordsNothing: --plan preflights a build and writes
// nothing, so recording from it would make the dry run the thing that changed
// the world.
//
// It needs a real forge.yaml, because `forge env build --plan` passes through
// the build feature gate before reaching the recording — without one the
// command fails at the gate and the test would pass whatever the recording
// did. Verified by removing the --plan guard: the test then fails, which it
// did not before the project was written.
func TestEnvBuildPlanRecordsNothing(t *testing.T) {
	fake := withDeclarationRecorder(t, declarationFixtureShape())
	withProjectName(t, "acme")
	withDeclaredEntities(t, &KCLEntities{
		ControlPlane: &ControlPlaneEntity{Type: "control_plane", Endpoint: "https://cp.example.test"},
	})
	withBuildableProject(t)

	cmd := newEnvBuildCmd()
	cmd.SetArgs([]string{"prod", "--plan"})
	cmd.SetContext(context.Background())
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	_ = cmd.Execute()

	if n := len(fake.callsTo(procEnsureEnv)); n != 0 {
		t.Errorf("`forge env build --plan` recorded %d declaration(s); a preflight must write nothing", n)
	}
}

// withBuildableProject writes the minimum forge.yaml that loads, and chdirs
// into it. `forge env build` resolves its project config and the build
// feature gate before it reaches anything this file tests, so a project that
// fails to LOAD makes a recording assertion pass vacuously — which is how
// the --plan test first passed with its guard removed.
func withBuildableProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	// features.build is stated rather than left to derivation: a project
	// with no cmd/ tree derives as a library, which disables build and
	// makes `forge env build` refuse at the gate.
	const forgeYAML = "name: shapetest\nmodule_path: github.com/example/shapetest\nfeatures:\n  build: true\n"
	if err := os.WriteFile(filepath.Join(dir, "forge.yaml"), []byte(forgeYAML), 0o644); err != nil {
		t.Fatalf("write forge.yaml: %v", err)
	}
	t.Chdir(dir)
	return dir
}

// withDeclaredEntities states the env's rendered declaration, so a test can
// exercise the recording without a project on disk.
func withDeclaredEntities(t *testing.T, entities *KCLEntities) {
	t.Helper()
	prev := renderKCLForDeclaration
	renderKCLForDeclaration = func(context.Context, string, string) (*KCLEntities, error) { return entities, nil }
	t.Cleanup(func() { renderKCLForDeclaration = prev })
}

func procNames(calls []fakeCPCall) []string {
	out := make([]string, len(calls))
	for i, c := range calls {
		out[i] = c.Proc
	}
	return out
}
