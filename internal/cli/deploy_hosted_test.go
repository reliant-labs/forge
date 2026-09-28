package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/cloud"
	"github.com/reliant-labs/forge/internal/deploytarget"
)

const hostedTestDigest = "sha256:1111111111111111111111111111111111111111111111111111111111111111"

// writeHostedProject writes a real forge project with ONE env, "hosted",
// whose Bundle declares control_plane → endpoint and binds its one workload,
// `api`, to forge.OnHosted. apiFields are the fw.Workload fields spliced into
// it (image, ports, env, resources …).
func writeHostedProject(t *testing.T, endpoint, apiFields string) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"forge.yaml":         "name: acme\nmodule_path: github.com/example/acme\nversion: 0.1.0\nfrontends: []\n",
		"deploy/kcl/kcl.mod": "[package]\nname = \"acme_deploy\"\nedition = \"v0.11.0\"\nversion = \"0.0.1\"\n\n[dependencies]\n",
		"deploy/kcl/hosted/main.k": `import forge
import forge.workloads as fw

_bundle = forge.Bundle {
    project = "acme"
    env = "hosted"
    control_plane = forge.ControlPlane {
        endpoint = "` + endpoint + `"
        token_env = "ACME_CP_TOKEN"
    }
    secret_provider = forge.HostedSecrets {}
    workloads = [_w | {runtime = forge.OnHosted {}} if not _w.runtime else _w for _w in [fw.Workload {
        name = "api"
` + apiFields + `
    }]]
}

output = forge.render(_bundle)
`,
	}
	for rel, body := range files {
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	markServiceProject(t, dir)
	return dir
}

// The image is TAG-pinned on purpose: the release cut resolves it to a digest
// (hostedBackendDigestResolver, stubbed per test), and the deploy must publish
// THAT digest — a deploy that shipped the declared reference would publish a
// tag, and the e2e test would see it.
const hostedOnBandSpec = `        image = "localhost:5051/acme/api:v1"
        ports = [fw.Port {name = "http", port = 8080, expose = True}]
        env = {GREETING = forge.ManagedSecret {name = "GREETING"}}`

// stubHostedRegistry answers the cut's tag → digest lookup for
// localhost:5051/acme/api:v1 with hostedTestDigest, and fails any other ref.
func stubHostedRegistry(t *testing.T) {
	t.Helper()
	prev := hostedBackendDigestResolver
	hostedBackendDigestResolver = func(_ context.Context, ref string) (string, []string, error) {
		if ref == "localhost:5051/acme/api:v1" {
			return hostedTestDigest, []string{"linux/amd64"}, nil
		}
		return "", nil, fmt.Errorf("no such image %s", ref)
	}
	t.Cleanup(func() { hostedBackendDigestResolver = prev })
}

// runForge runs one forge command in-process through the real root command,
// capturing stdout (the JSON commands write to os.Stdout directly).
func runForge(t *testing.T, args ...string) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	prev := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() {
		var b bytes.Buffer
		_, _ = io.Copy(&b, r)
		done <- b.String()
	}()
	cmd := NewRootCmd()
	cmd.SetArgs(args)
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	runErr := cmd.ExecuteContext(context.Background())
	_ = w.Close()
	os.Stdout = prev
	return <-done, runErr
}

// TestHostedCLIEndToEnd drives the real commands against an httptest control
// plane speaking Connect JSON:
//
//	forge release cut v1 --env hosted
//	forge env promote v1 --to hosted       (creates the env by name)
//	forge env deploy hosted --json         (ensure → publish → readiness)
//	forge env status hosted --json
//	forge env topology --json
//
// Nothing but the HTTP endpoint is faked: KCL renders for real, the ledger is
// the hosted ledger, the provider is the hosted provider.
func TestHostedCLIEndToEnd(t *testing.T) {
	if testing.Short() {
		t.Skip("renders KCL and runs the whole CLI; skipped in -short")
	}
	fake := newFakeDeployService(map[string]string{})
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	dir := writeHostedProject(t, srv.URL, hostedOnBandSpec)
	t.Chdir(dir)
	t.Setenv("ACME_CP_TOKEN", "rlat_e2e")
	t.Setenv("FORGE_HOME", t.TempDir())
	prevPoll := hostedPollInterval
	hostedPollInterval = time.Millisecond
	t.Cleanup(func() { hostedPollInterval = prevPoll })
	stubHostedRegistry(t)

	if out, err := runForge(t, "release", "cut", "v1", "--env", "hosted"); err != nil {
		t.Fatalf("release cut: %v\n%s", err, out)
	}
	rel, ok := fake.releases["v1"]
	if !ok || len(rel.Artifacts) != 1 || rel.Artifacts[0].Name != "api" || rel.Artifacts[0].Digest != hostedTestDigest {
		t.Fatalf("cut release = %+v, want one artifact api@%s", rel, hostedTestDigest)
	}
	if out, err := runForge(t, "env", "promote", "v1", "--to", "hosted"); err != nil {
		t.Fatalf("promote: %v\n%s", err, out)
	}
	envID := fake.envs["hosted"]
	if envID == "" {
		t.Fatal("promote did not ensure the hosted env")
	}

	fake.bodies = nil
	out, err := runForge(t, "env", "deploy", "hosted", "--json", "--rollout-timeout", "2s")
	if err != nil {
		t.Fatalf("deploy: %v\n%s", err, out)
	}
	var rep struct {
		OK    bool `json:"ok"`
		Guard struct {
			DeclaredContext string `json:"declared_context"`
			Verdict         string `json:"verdict"`
			Reason          string `json:"reason"`
		} `json:"guard"`
		Target struct {
			KubeContext   string   `json:"kube_context"`
			AllContexts   []string `json:"all_kube_contexts"`
			Destination   string   `json:"destination"`
			Endpoint      string   `json:"endpoint"`
			EnvironmentID string   `json:"environment_id"`
		} `json:"target"`
		Release string `json:"release"`
		Rollout struct {
			Results []struct {
				Name  string `json:"name"`
				State string `json:"state"`
			} `json:"results"`
		} `json:"rollout"`
	}
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("deploy --json is not JSON: %v\n%s", err, out)
	}
	if !rep.OK || rep.Release != "v1" {
		t.Errorf("ok=%v release=%q", rep.OK, rep.Release)
	}
	if rep.Guard.DeclaredContext != srv.URL || rep.Guard.Verdict != "allow" || rep.Guard.Reason != "control_plane_declared" {
		t.Errorf("guard = %+v, want the endpoint as the declared context", rep.Guard)
	}
	if rep.Target.Destination != "hosted" || rep.Target.Endpoint != srv.URL || rep.Target.EnvironmentID != envID {
		t.Errorf("target = %+v", rep.Target)
	}
	if rep.Target.KubeContext != "" || len(rep.Target.AllContexts) != 0 {
		t.Errorf("a hosted deploy must name no kube context, got %+v", rep.Target)
	}
	if len(rep.Rollout.Results) != 1 || rep.Rollout.Results[0].State != "ready" {
		t.Errorf("rollout = %+v", rep.Rollout)
	}

	// The wire: ensure (env) → ensure (deployment) → publish → status, with
	// the BOUND digest in the published spec.
	var paths []string
	for _, b := range fake.bodies {
		paths = append(paths, b.Path[strings.LastIndex(b.Path, "/")+1:])
	}
	joined := strings.Join(paths, ",")
	for _, seq := range []string{"EnsureEnvironment,EnsureDeployment,PublishDeploymentConfig,GetStatus"} {
		if !strings.Contains(joined, seq) {
			t.Fatalf("deploy call sequence = %s, want it to contain %s", joined, seq)
		}
	}
	d := fake.deployments[envID]["api"]
	if d == nil || !d.Published || d.Tier != "DEPLOY_TIER_BACKEND" {
		t.Fatalf("deployment = %+v", d)
	}
	if d.Spec["image"] != "localhost:5051/acme/api@"+hostedTestDigest {
		t.Errorf("published image = %v", d.Spec["image"])
	}

	statusOut, err := runForge(t, "env", "status", "hosted", "--json")
	if err != nil {
		t.Fatalf("status: %v\n%s", err, statusOut)
	}
	var st struct {
		Destination   string                              `json:"destination"`
		Endpoint      string                              `json:"endpoint"`
		EnvironmentID string                              `json:"environment_id"`
		Verdict       string                              `json:"verdict"`
		Workloads     []deploytarget.HostedWorkloadStatus `json:"hosted_workloads"`
	}
	if err := json.Unmarshal([]byte(statusOut), &st); err != nil {
		t.Fatalf("status --json: %v\n%s", err, statusOut)
	}
	if st.Destination != "hosted" || st.Endpoint != srv.URL || st.EnvironmentID != envID || st.Verdict != "converging" {
		t.Errorf("status env = %+v", st)
	}
	if len(st.Workloads) != 1 || st.Workloads[0].Name != "api" || st.Workloads[0].ObservedState != "ready" ||
		st.Workloads[0].Hostname != "api-acme.reliantapps.dev" || st.Workloads[0].ObservedDigest != hostedTestDigest {
		t.Errorf("status workloads = %+v", st.Workloads)
	}

	topoOut, err := runForge(t, "env", "topology", "--json")
	if err != nil {
		t.Fatalf("topology: %v\n%s", err, topoOut)
	}
	var topo struct {
		Environments []topologyEnv `json:"environments"`
	}
	if err := json.Unmarshal([]byte(topoOut), &topo); err != nil {
		t.Fatalf("topology --json: %v\n%s", err, topoOut)
	}
	if len(topo.Environments) != 1 {
		t.Fatalf("topology envs = %+v", topo.Environments)
	}
	row := topo.Environments[0]
	if row.Destination != "hosted" || row.Endpoint != srv.URL || row.EnvironmentID != envID || !row.Bound || row.Release != "v1" {
		t.Errorf("topology row = %+v", row)
	}
	if row.KubeContext != "" || len(row.Workloads) != 1 || row.Workloads[0].URL != "https://api-acme.reliantapps.dev" {
		t.Errorf("topology row workloads/context = %+v", row)
	}
}

// TestHostedSecretBeforeFirstDeploy: on a FRESH control plane, a secret can
// be set before anything else — the order a managedSecret backend needs, so
// its pod never starts without the value:
//
//	forge secret set hosted GREETING   (ensures the env by name)
//	forge release cut v1 --env hosted
//	forge env promote v1 --to hosted
//	forge env deploy hosted
//
// All four land in ONE environment id. Mutation: making secret set resolve
// instead of ensure fails the first step with "no environment".
func TestHostedSecretBeforeFirstDeploy(t *testing.T) {
	if testing.Short() {
		t.Skip("renders KCL and runs the whole CLI; skipped in -short")
	}
	fake := newFakeDeployService(map[string]string{})
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	dir := writeHostedProject(t, srv.URL, hostedOnBandSpec)
	t.Chdir(dir)
	t.Setenv("ACME_CP_TOKEN", "rlat_e2e")
	t.Setenv("FORGE_HOME", t.TempDir())
	stubHostedRegistry(t)
	prevPoll := hostedPollInterval
	hostedPollInterval = time.Millisecond
	t.Cleanup(func() { hostedPollInterval = prevPoll })

	// The fake answers SecretStoreService/SetSecret below; record the env id.
	var secretEnv string
	fake.onSecret = func(body map[string]any) { secretEnv, _ = body["environmentId"].(string) }

	stdin := os.Stdin
	r, w, _ := os.Pipe()
	_, _ = w.WriteString("hello-from-secret\n")
	_ = w.Close()
	os.Stdin = r
	out, err := runForge(t, "secret", "set", "--env", "hosted", "GREETING")
	os.Stdin = stdin
	if err != nil {
		t.Fatalf("secret set before any deploy: %v\n%s", err, out)
	}
	envID := fake.envs["hosted"]
	if envID == "" || secretEnv != envID {
		t.Fatalf("secret landed in %q, env is %q", secretEnv, envID)
	}
	for _, args := range [][]string{
		{"release", "cut", "v1", "--env", "hosted"},
		{"env", "promote", "v1", "--to", "hosted"},
		{"env", "deploy", "hosted", "--rollout-timeout", "2s"},
	} {
		if out, err := runForge(t, args...); err != nil {
			t.Fatalf("forge %v: %v\n%s", args, err, out)
		}
	}
	if fake.envs["hosted"] != envID || len(fake.envs) != 1 {
		t.Fatalf("envs = %v, want the one env the secret created", fake.envs)
	}
	if d := fake.deployments[envID]["api"]; d == nil || !d.Published {
		t.Fatalf("api not published into %s: %+v", envID, fake.deployments)
	}
}

// TestHostedDeployRefusals: unbound, off-band and non-tier each refuse before
// a single write reaches the control plane.
func TestHostedDeployRefusals(t *testing.T) {
	if testing.Short() {
		t.Skip("renders KCL; skipped in -short")
	}
	writes := func(f *fakeDeployService) int {
		n := 0
		for _, b := range f.bodies {
			switch b.Path[strings.LastIndex(b.Path, "/")+1:] {
			case "EnsureEnvironment", "EnsureDeployment", "PublishDeploymentConfig":
				n++
			}
		}
		return n
	}
	setup := func(t *testing.T, spec string) (*fakeDeployService, string) {
		fake := newFakeDeployService(map[string]string{})
		srv := httptest.NewServer(fake)
		t.Cleanup(srv.Close)
		dir := writeHostedProject(t, srv.URL, spec)
		t.Chdir(dir)
		t.Setenv("ACME_CP_TOKEN", "rlat_e2e")
		t.Setenv("FORGE_HOME", t.TempDir())
		stubHostedRegistry(t)
		return fake, dir
	}

	t.Run("unbound", func(t *testing.T) {
		fake, _ := setup(t, hostedOnBandSpec)
		_, err := runForge(t, "env", "deploy", "hosted")
		if err == nil || !strings.Contains(err.Error(), "forge env promote") {
			t.Fatalf("err = %v, want the promote fix", err)
		}
		if n := writes(fake); n != 0 {
			t.Fatalf("%d write(s) for an unbound env", n)
		}
	})

	t.Run("off-band", func(t *testing.T) {
		fake, _ := setup(t, `        image = "localhost:5051/acme/api:v1"
        ports = [fw.Port {name = "http", port = 8080, expose = True}]
        resources = fw.Resources {cpuRequestMillicores = 500, memoryRequestBytes = 1073741824}`)
		if _, err := runForge(t, "release", "cut", "v1", "--env", "hosted"); err != nil {
			t.Fatal(err)
		}
		if _, err := runForge(t, "env", "promote", "v1", "--to", "hosted"); err != nil {
			t.Fatal(err)
		}
		fake.bodies = nil
		_, err := runForge(t, "env", "deploy", "hosted")
		if err == nil || !strings.Contains(err.Error(), "shape band") {
			t.Fatalf("err = %v, want the shape-band refusal", err)
		}
		if n := writes(fake); n != 0 {
			t.Fatalf("%d write(s) before the off-band refusal", n)
		}
	})

	// The control plane admits only registry.reliant.dev/org-1/…; the project
	// ships localhost:5051/acme/api. Refused after EnsureEnvironment (which
	// reports the base) and before any EnsureDeployment or publish.
	// MUTATION VERIFIED RED: deleting checkImagePushBase from Deploy.
	t.Run("foreign registry", func(t *testing.T) {
		fake, _ := setup(t, hostedOnBandSpec)
		fake.imagePushBase = "registry.reliant.dev/org-1"
		if _, err := runForge(t, "release", "cut", "v1", "--env", "hosted"); err != nil {
			t.Fatal(err)
		}
		if _, err := runForge(t, "env", "promote", "v1", "--to", "hosted"); err != nil {
			t.Fatal(err)
		}
		fake.bodies = nil
		_, err := runForge(t, "env", "deploy", "hosted")
		if err == nil || !strings.Contains(err.Error(), "push the image to registry.reliant.dev/org-1/api") {
			t.Fatalf("err = %v, want the push-base refusal naming the fix", err)
		}
		for _, b := range fake.bodies {
			switch b.Path[strings.LastIndex(b.Path, "/")+1:] {
			case "EnsureDeployment", "PublishDeploymentConfig":
				t.Fatalf("%s called before the push-base refusal", b.Path)
			}
		}
	})
}

// TestBuildDeployGroups_HostedIsPerWorkload: a control_plane env that binds
// one workload to the platform and another to a cluster deploys BOTH — a
// hosted group and a k8s-cluster group from one render. Before, a
// control_plane env was hosted env-wide: buildDeployGroups short-circuited
// to buildHostedGroups, which REFUSED the cluster workload ("not a tier").
// Hosted jobs and databases ride the hosted group; a build-only workload
// joins none.
func TestBuildDeployGroups_HostedIsPerWorkload(t *testing.T) {
	migrate := hostedWL("migrate", func(w *WorkloadEntity) {
		w.Kind = "job"
		w.Spec.Kind = "job"
		w.Spec.Ports, w.Spec.Probes = nil, nil
		w.Spec.Args = []string{"db", "migrate", "up"}
		w.Spec.Before = []string{"api"}
	})
	e := &KCLEntities{
		ControlPlane: &ControlPlaneEntity{Type: "control_plane", Endpoint: "https://cp.example"},
		Workloads: []WorkloadEntity{
			hostedWL("api"),
			migrate,
			clusterWL("worker", "k3d-acme", "acme-dev"),
			{Name: "img", Kind: "tool", Runtime: RuntimeEntity{Type: RuntimeBuildOnly, BuildOnly: &BuildOnlyDeploy{}}},
		},
		Databases: []DatabaseEntity{{Name: "orders", Runtime: RuntimeHosted}},
	}
	groups, err := buildDeployGroups("prod", e, "")
	if err != nil {
		t.Fatalf("a mixed hosted + cluster env must group, got: %v", err)
	}
	var hosted, k8s []string
	for _, g := range groups {
		for _, s := range g.Services {
			switch g.ProviderID {
			case deploytarget.HostedProviderID:
				hosted = append(hosted, s.Name)
			case "k8s-cluster":
				k8s = append(k8s, s.Name)
			default:
				t.Errorf("unexpected group %s for %s", g.ProviderID, s.Name)
			}
		}
	}
	if strings.Join(hosted, ",") != "api,migrate,orders" {
		t.Errorf("hosted group = %v, want [api migrate orders] (the job is published, not dropped)", hosted)
	}
	if strings.Join(k8s, ",") != "worker" {
		t.Errorf("k8s-cluster group = %v, want [worker]", k8s)
	}
}

// TestDestinationOf pins the destination vocabulary: every deployable thing
// votes by its own runtime.
func TestDestinationOf(t *testing.T) {
	cp := &ControlPlaneEntity{Endpoint: "https://x"}
	cases := map[string]struct {
		e    *KCLEntities
		want string
	}{
		"hosted":  {&KCLEntities{ControlPlane: cp, Workloads: []WorkloadEntity{hostedWL("api")}}, "hosted"},
		"cluster": {&KCLEntities{Workloads: []WorkloadEntity{clusterWL("a", "k3d-x", "ns"), clusterWL("b", "k3d-x", "ns")}}, "cluster"},
		"compose": {&KCLEntities{Workloads: []WorkloadEntity{composeWL("pg", "docker-compose.yml")}}, "compose"},
		"host":    {&KCLEntities{Workloads: []WorkloadEntity{hostWL("api")}}, "host"},
		"static":  {&KCLEntities{Frontends: []FrontendEntity{{Runtime: FrontendRuntime{Type: FrontendRuntimeFirebase}}}}, "static"},
		"mixed":   {&KCLEntities{ControlPlane: cp, Workloads: []WorkloadEntity{hostedWL("api"), composeWL("pg", "docker-compose.yml")}}, "mixed"},
		"empty":   {&KCLEntities{}, "host"},
	}
	for name, tc := range cases {
		if got := destinationOf(tc.e); got != tc.want {
			t.Errorf("%s: destinationOf = %q, want %q", name, got, tc.want)
		}
	}
}

// TestResolveEnvDestinationNeverFabricatesAnID: a hosted env the control
// plane does not know has an endpoint and NO environment_id.
func TestResolveEnvDestinationNeverFabricatesAnID(t *testing.T) {
	e := &KCLEntities{ControlPlane: &ControlPlaneEntity{Type: "control_plane", Endpoint: "https://cp.example/"},
		Databases: []DatabaseEntity{{Name: "orders", Runtime: RuntimeHosted}}}
	got := resolveEnvDestination(context.Background(), "prod", e, func(context.Context, string, *KCLEntities) (deploytarget.HostedEnvStatus, error) {
		return deploytarget.HostedEnvStatus{}, deploytarget.ErrHostedEnvironmentNotFound
	})
	if got.Destination != "hosted" || got.ControlPlaneKind != "persistent" || got.Endpoint != "https://cp.example" || got.EnvironmentID != "" || got.Note == "" {
		t.Fatalf("destination = %+v", got)
	}
	var _ = cloud.DefaultTokenEnv
}

// TestHostedArtifactKey: the workload's own artifact name (`image`, what
// forge's build state records a push under) wins over the spec image's last
// segment, and the hosted group carries it — publishing the spec BY that
// artifact name — so the deploy pins by the same key the cut recorded.
func TestHostedArtifactKey(t *testing.T) {
	w := hostedWL("api", func(w *WorkloadEntity) {
		w.Image = "e2eh/abc/echo"
		w.Spec.Image = "localhost:5051/e2eh/abc/echo:t1"
	})
	if got := hostedArtifactKey(w); got != "e2eh/abc/echo" {
		t.Fatalf("key = %q, want the workload's artifact name", got)
	}
	e := &KCLEntities{ControlPlane: &ControlPlaneEntity{Endpoint: "https://x"}, Workloads: []WorkloadEntity{w}}
	groups, err := buildDeployGroups("prod", e, "")
	if err != nil || groups[0].Services[0].Hosted.Artifact != "e2eh/abc/echo" {
		t.Fatalf("group artifact = %+v err=%v", groups, err)
	}
	if img := groups[0].Services[0].Hosted.Workload.Image; img != "e2eh/abc/echo" {
		t.Errorf("published image = %q, want the artifact name the release pins", img)
	}
	w.Image = ""
	if got := hostedArtifactKey(w); got != "echo" {
		t.Fatalf("fallback key = %q, want echo", got)
	}
}
