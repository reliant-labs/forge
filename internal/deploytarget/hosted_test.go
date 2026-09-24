package deploytarget

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/cluster"
	"github.com/reliant-labs/forge/pkg/deploy/v1alpha1"
)

const (
	digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

// fakeCP is a control plane that answers with canned JSON and records every
// call in order. It round-trips request bodies through JSON so assertions see
// exactly the wire document, not the Go value.
type fakeCP struct {
	mu    sync.Mutex
	calls []fakeCall
	// status returns the GetStatus body for the nth GetStatus call.
	status func(n int) string
	envs   string
	proms  string
	nStat  int
	// pushBase is EnsureEnvironment's imagePushBase. Empty means the
	// default "ghcr.io/acme" (hostedGroup's images); "-" means the control
	// plane reports none.
	pushBase string
}

type fakeCall struct {
	Proc string
	Body map[string]any
}

func (f *fakeCP) Call(_ context.Context, proc string, req, out any) error {
	raw, _ := json.Marshal(req)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	f.mu.Lock()
	f.calls = append(f.calls, fakeCall{Proc: proc, Body: body})
	f.mu.Unlock()
	short := proc[strings.LastIndex(proc, "/")+1:]
	var reply string
	switch short {
	case "EnsureEnvironment":
		f.mu.Lock()
		base := f.pushBase
		f.mu.Unlock()
		if base == "" {
			base = "ghcr.io/acme"
		}
		if base == "-" {
			base = ""
		}
		reply = fmt.Sprintf(`{"environment":{"id":"env-1","name":"prod","namespace":"env-env-1","imagePushBase":%q},"created":true}`, base)
	case "EnsureDeployment":
		reply = fmt.Sprintf(`{"deployment":{"id":"dep-%s","name":%q},"created":true}`, body["name"], body["name"])
	case "PublishDeploymentConfig":
		reply = `{"digest":"sha256:cfg","reference":"reg/cfg@sha256:cfg"}`
	case "GetStatus":
		f.mu.Lock()
		f.nStat++
		n := f.nStat
		f.mu.Unlock()
		reply = f.status(n)
	case "ListEnvironments":
		reply = f.envs
	case "ListPromotions":
		reply = f.proms
	case "Rollback":
		reply = `{"promotion":{}}`
	default:
		return fmt.Errorf("unexpected procedure %s", proc)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal([]byte(reply), out)
}

func (f *fakeCP) procs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.calls))
	for _, c := range f.calls {
		out = append(out, c.Proc[strings.LastIndex(c.Proc, "/")+1:])
	}
	return out
}

func readyStatus(digest string) func(int) string {
	return func(int) string {
		return fmt.Sprintf(`{"environmentVerdict":"DEPLOY_VERDICT_CONVERGING","deployments":[
		 {"deployment":{"id":"dep-api","name":"api","observed":{"state":"DEPLOY_OBSERVED_STATE_READY","imageDigest":%q}},"verdict":"DEPLOY_VERDICT_CONVERGING","desiredDigest":%q},
		 {"deployment":{"id":"dep-orders","name":"orders","observed":{"state":"DEPLOY_OBSERVED_STATE_READY"}},"verdict":"DEPLOY_VERDICT_CONVERGED"}]}`, digest, digest)
	}
}

func hostedGroup(release string, digests map[string]string, resources v1alpha1.Resources) ServiceGroup {
	return ServiceGroup{
		Env:        "prod",
		ProviderID: HostedProviderID,
		Hosted:     &HostedTarget{Endpoint: "https://cp.example", Release: release, Digests: digests},
		Services: []ResolvedService{
			{Name: "api", Hosted: &HostedWorkload{Tier: HostedTierBackend, Backend: &v1alpha1.SimpleBackendSpec{
				Image: "ghcr.io/acme/api:v1", Ports: []int32{8080}, Resources: resources,
			}}},
			{Name: "orders", Hosted: &HostedWorkload{Tier: HostedTierDatabase, Database: &v1alpha1.ManagedDatabaseSpec{}}},
		},
	}
}

// TestHostedDeployCallOrderAndBoundDigest pins the fixed order — ensure env,
// ensure every deployment, publish every deployment, then status — and that
// the backend's spec carries the BOUND digest, not the declared tag.
// Mutation: swapping the ensure and publish loops, or publishing spec.Image
// unpinned, fails this test.
func TestHostedDeployCallOrderAndBoundDigest(t *testing.T) {
	cp := &fakeCP{status: readyStatus(digestA)}
	var envSeen string
	var outcomes []cluster.RolloutObservation
	p := HostedProvider{Client: cp, PollInterval: time.Millisecond,
		OnEnvironment: func(id string) { envSeen = id },
		OnRollout:     func(o cluster.RolloutObservation) { outcomes = append(outcomes, o) }}
	if err := p.Deploy(context.Background(), hostedGroup("v1", map[string]string{"api": digestA}, v1alpha1.Resources{})); err != nil {
		t.Fatalf("deploy: %v", err)
	}
	want := []string{"EnsureEnvironment", "EnsureDeployment", "EnsureDeployment", "PublishDeploymentConfig", "PublishDeploymentConfig", "GetStatus"}
	if got := cp.procs(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("call order = %v, want %v", got, want)
	}
	env := cp.calls[0].Body["spec"].(map[string]any)
	if env["name"] != "prod" || env["kind"] != "DEPLOY_ENVIRONMENT_KIND_PERSISTENT" {
		t.Errorf("EnsureEnvironment spec = %v", env)
	}
	api := cp.calls[1].Body
	if api["environmentId"] != "env-1" || api["name"] != "api" || api["tier"] != "DEPLOY_TIER_BACKEND" {
		t.Errorf("EnsureDeployment(api) = %v", api)
	}
	if img := api["spec"].(map[string]any)["image"]; img != "ghcr.io/acme/api@"+digestA {
		t.Errorf("published image = %v, want the bound digest ghcr.io/acme/api@%s", img, digestA)
	}
	if cp.calls[2].Body["tier"] != "DEPLOY_TIER_DATABASE" {
		t.Errorf("EnsureDeployment(orders) tier = %v", cp.calls[2].Body["tier"])
	}
	if cp.calls[3].Body["deploymentId"] != "dep-api" || cp.calls[4].Body["deploymentId"] != "dep-orders" {
		t.Errorf("publish bodies = %v / %v", cp.calls[3].Body, cp.calls[4].Body)
	}
	if cp.calls[5].Body["environmentId"] != "env-1" {
		t.Errorf("GetStatus must be env-scoped, got %v", cp.calls[5].Body)
	}
	if envSeen != "env-1" {
		t.Errorf("OnEnvironment got %q", envSeen)
	}
	if len(outcomes) != 2 || outcomes[0].State != cluster.RolloutStateReady {
		t.Errorf("rollout outcomes = %+v", outcomes)
	}
}

// TestHostedOffBandRefusedWithZeroRPCs: a spec off the 4 GiB/vCPU band is
// refused BEFORE any write. Mutation: moving CheckShapeBand after
// EnsureEnvironment makes the call count non-zero.
func TestHostedOffBandRefusedWithZeroRPCs(t *testing.T) {
	cp := &fakeCP{status: readyStatus(digestA)}
	offBand := v1alpha1.Resources{CPURequestMillicores: 500, MemoryRequestBytes: 1 << 30}
	err := HostedProvider{Client: cp}.Deploy(context.Background(), hostedGroup("v1", map[string]string{"api": digestA}, offBand))
	if err == nil || !strings.Contains(err.Error(), "shape band") {
		t.Fatalf("err = %v, want a shape-band refusal", err)
	}
	if n := len(cp.procs()); n != 0 {
		t.Fatalf("%d RPC(s) made before the refusal: %v", n, cp.procs())
	}
}

// TestHostedImagePushBase: the control plane's imagePushBase (read off
// EnsureEnvironment) decides, BEFORE any EnsureDeployment or publish, whether
// a bound backend image is one the platform will publish.
//
// MUTATIONS VERIFIED RED:
//   - deleting the checkImagePushBase call from Deploy → "foreign" and
//     "no base" make EnsureDeployment/Publish calls;
//   - moving it after p.publish's EnsureDeployment loop → non-zero writes;
//   - a bare strings.HasPrefix(repo, base) without the "/" → the
//     adjacent-prefix case ("ghcr.io/acme-evil") is admitted;
//   - dropping the `base == ""` refusal → "no base" publishes.
func TestHostedImagePushBase(t *testing.T) {
	writes := func(cp *fakeCP) []string {
		var out []string
		for _, p := range cp.procs() {
			if p == "EnsureDeployment" || p == "PublishDeploymentConfig" {
				out = append(out, p)
			}
		}
		return out
	}
	deploy := func(base string) (*fakeCP, error) {
		cp := &fakeCP{status: readyStatus(digestA), pushBase: base}
		err := HostedProvider{Client: cp, PollInterval: time.Millisecond}.Deploy(context.Background(),
			hostedGroup("v1", map[string]string{"api": digestA}, v1alpha1.Resources{}))
		return cp, err
	}

	t.Run("foreign registry refused with zero writes", func(t *testing.T) {
		cp, err := deploy("registry.reliant.dev/org-1")
		if err == nil {
			t.Fatal("an image outside the push base was published")
		}
		for _, want := range []string{
			"ghcr.io/acme/api@" + digestA,                      // the image
			"registry.reliant.dev/org-1",                       // the base
			"push the image to registry.reliant.dev/org-1/api", // the fix
			"forge release cut", "forge env promote",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal does not name %q:\n%v", want, err)
			}
		}
		if w := writes(cp); len(w) != 0 {
			t.Fatalf("writes before the push-base refusal: %v", w)
		}
	})

	t.Run("adjacent prefix refused", func(t *testing.T) {
		cp, err := deploy("ghcr.io/ac")
		if err == nil || len(writes(cp)) != 0 {
			t.Fatalf("ghcr.io/acme/api admitted under base ghcr.io/ac (err=%v, writes=%v)", err, writes(cp))
		}
	})

	t.Run("no base refused", func(t *testing.T) {
		cp, err := deploy("-")
		if err == nil || !strings.Contains(err.Error(), "no image push base") {
			t.Fatalf("err = %v, want the no-push-base refusal", err)
		}
		if w := writes(cp); len(w) != 0 {
			t.Fatalf("writes with no push base: %v", w)
		}
	})

	t.Run("in-base image accepted", func(t *testing.T) {
		cp, err := deploy("ghcr.io/acme")
		if err != nil {
			t.Fatalf("an image under the push base was refused: %v", err)
		}
		if w := writes(cp); len(w) != 4 {
			t.Fatalf("writes = %v, want 2 ensures + 2 publishes", w)
		}
	})
}

// TestHostedUnboundRefused: no promoted release means no digest to ship; the
// refusal names the promote fix and makes no call.
func TestHostedUnboundRefused(t *testing.T) {
	cp := &fakeCP{}
	err := HostedProvider{Client: cp}.Deploy(context.Background(), hostedGroup("", nil, v1alpha1.Resources{}))
	if err == nil || !strings.Contains(err.Error(), "forge env promote") {
		t.Fatalf("err = %v, want the forge env promote fix", err)
	}
	if len(cp.procs()) != 0 {
		t.Fatalf("RPCs made for an unbound env: %v", cp.procs())
	}
}

// TestHostedReleaseMissingArtifactRefused: a release that pins no digest for a
// backend's image refuses, naming the artifact.
func TestHostedReleaseMissingArtifactRefused(t *testing.T) {
	cp := &fakeCP{}
	err := HostedProvider{Client: cp}.Deploy(context.Background(), hostedGroup("v1", map[string]string{"other": digestA}, v1alpha1.Resources{}))
	if err == nil || !strings.Contains(err.Error(), `pins no artifact "api"`) {
		t.Fatalf("err = %v", err)
	}
	if len(cp.procs()) != 0 {
		t.Fatalf("RPCs made: %v", cp.procs())
	}
}

// TestHostedReadinessTimeoutIsNotSuccess: a workload that never becomes ready
// fails the deploy as TIMED OUT. Mutation: returning nil at the deadline makes
// this pass wrongly — the test fails then.
func TestHostedReadinessTimeoutIsNotSuccess(t *testing.T) {
	cp := &fakeCP{status: func(int) string {
		return `{"environmentVerdict":"DEPLOY_VERDICT_CONVERGING","deployments":[
		 {"deployment":{"id":"dep-api","name":"api","observed":{"state":"DEPLOY_OBSERVED_STATE_PROGRESSING","imageDigest":""}},"verdict":"DEPLOY_VERDICT_CONVERGING"},
		 {"deployment":{"id":"dep-orders","name":"orders","observed":{"state":"DEPLOY_OBSERVED_STATE_READY"}},"verdict":"DEPLOY_VERDICT_CONVERGED"}]}`
	}}
	var states []cluster.RolloutState
	p := HostedProvider{Client: cp, PollInterval: time.Millisecond,
		Rollout:   cluster.RolloutPolicy{Timeout: 20 * time.Millisecond},
		OnRollout: func(o cluster.RolloutObservation) { states = append(states, o.State) }}
	err := p.Deploy(context.Background(), hostedGroup("v1", map[string]string{"api": digestA}, v1alpha1.Resources{}))
	if err == nil || !strings.Contains(err.Error(), "TIMED OUT") || !strings.Contains(err.Error(), "api: verdict converging, observed progressing") {
		t.Fatalf("err = %v, want TIMED OUT naming api", err)
	}
	if len(states) != 2 || states[0] != cluster.RolloutStateTimedOut || states[1] != cluster.RolloutStateReady {
		t.Fatalf("outcomes = %v, want [timed_out ready]", states)
	}
}

// TestHostedReadyNeedsTheDesiredDigest: observed READY on the OLD digest is a
// rollout still in flight, not readiness.
func TestHostedReadyNeedsTheDesiredDigest(t *testing.T) {
	st := wireDeploymentStatus{Deployment: wireDeployment{Observed: &wireObserved{State: wireObservedReady, ImageDigest: digestB}}, Verdict: "DEPLOY_VERDICT_CONVERGING"}
	if hostedDeploymentReady(st, digestA) {
		t.Fatal("READY on a stale digest counted as ready")
	}
	st.Deployment.Observed.ImageDigest = digestA
	if !hostedDeploymentReady(st, digestA) {
		t.Fatal("READY on the desired digest not counted as ready")
	}
}

// TestHostedRollbackRecordsThenRepublishes: rollback reads the ledger, records
// a Rollback to the previous release, and republishes that release's digests.
func TestHostedRollbackRecordsThenRepublishes(t *testing.T) {
	cp := &fakeCP{
		status: readyStatus(digestA),
		envs:   `{"environments":[{"id":"env-1","name":"prod","imagePushBase":"ghcr.io/acme"}]}`,
		proms: fmt.Sprintf(`{"promotions":[{"releaseVersion":"v2","resolvedArtifacts":{"api":%q}},
		                                    {"releaseVersion":"v1","resolvedArtifacts":{"api":%q}}]}`, digestB, digestA),
	}
	p := HostedProvider{Client: cp, PollInterval: time.Millisecond}
	if err := p.Rollback(context.Background(), hostedGroup("", nil, v1alpha1.Resources{}), ""); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	got := cp.procs()
	want := []string{"ListEnvironments", "ListPromotions", "Rollback", "EnsureDeployment", "EnsureDeployment", "PublishDeploymentConfig", "PublishDeploymentConfig", "GetStatus"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("call order = %v, want %v", got, want)
	}
	if cp.calls[2].Body["version"] != "v1" {
		t.Errorf("rolled back to %v, want v1", cp.calls[2].Body["version"])
	}
	if img := cp.calls[3].Body["spec"].(map[string]any)["image"]; img != "ghcr.io/acme/api@"+digestA {
		t.Errorf("republished image = %v, want v1's digest", img)
	}
}

// TestHostedObserveMapping pins the observation table.
func TestHostedObserveMapping(t *testing.T) {
	cases := []struct {
		name    string
		w       HostedWorkloadStatus
		present bool
		want    Health
	}{
		{"ready converged", HostedWorkloadStatus{ObservedState: "ready", Verdict: "converged", ObservedDigest: digestA}, true, HealthHealthy},
		{"ready converging", HostedWorkloadStatus{ObservedState: "ready", Verdict: "converging"}, true, HealthHealthy},
		{"progressing", HostedWorkloadStatus{ObservedState: "progressing", Verdict: "converging"}, true, HealthDegraded},
		{"pending", HostedWorkloadStatus{ObservedState: "pending", Verdict: "unknown"}, true, HealthDegraded},
		{"degraded", HostedWorkloadStatus{ObservedState: "degraded", Verdict: "degraded", LastError: "CrashLoopBackOff"}, true, HealthDegraded},
		{"suspended", HostedWorkloadStatus{ObservedState: "suspended"}, true, HealthAbsent},
		{"unknown", HostedWorkloadStatus{ObservedState: "unknown"}, true, HealthUnknown},
		{"absent", HostedWorkloadStatus{}, false, HealthAbsent},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			item := observedItemFor("api", tc.w, tc.present)
			if item.Health != tc.want {
				t.Fatalf("health = %v, want %v", item.Health, tc.want)
			}
			if item.Health != HealthHealthy && item.Detail == "" {
				t.Fatal("non-healthy item without Detail")
			}
			if tc.w.LastError != "" && !strings.Contains(item.Detail, tc.w.LastError) {
				t.Fatalf("detail %q lost the last error", item.Detail)
			}
		})
	}
}

// TestHostedObserveReadsEnvStatus: end to end through ReadHostedStatus.
func TestHostedObserveReadsEnvStatus(t *testing.T) {
	cp := &fakeCP{envs: `{"environments":[{"id":"env-1","name":"prod"}]}`, status: readyStatus(digestA)}
	obs, err := HostedProvider{Client: cp}.Observe(context.Background(), hostedGroup("v1", nil, v1alpha1.Resources{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(obs.Items) != 2 || obs.Items[0].Health != HealthHealthy || obs.Items[0].Digest != digestA {
		t.Fatalf("items = %+v", obs.Items)
	}
	// Never deployed: absent, a real answer, no error.
	cp2 := &fakeCP{envs: `{"environments":[]}`}
	obs, err = HostedProvider{Client: cp2}.Observe(context.Background(), hostedGroup("v1", nil, v1alpha1.Resources{}))
	if err != nil || obs.Items[0].Health != HealthAbsent {
		t.Fatalf("never-deployed: %+v %v", obs.Items, err)
	}
	// A failed read is unknown with the reason, and an error.
	cp3 := errCaller{}
	obs, err = HostedProvider{Client: cp3}.Observe(context.Background(), hostedGroup("v1", nil, v1alpha1.Resources{}))
	if err == nil || obs.Items[0].Health != HealthUnknown || !strings.Contains(obs.Items[0].Detail, "boom") {
		t.Fatalf("failed read: %+v %v", obs.Items, err)
	}
}

type errCaller struct{}

func (errCaller) Call(context.Context, string, any, any) error { return errors.New("boom") }

// TestHostedArtifactName pins the key the cut and the deploy share.
func TestHostedArtifactName(t *testing.T) {
	for in, want := range map[string]string{
		"ghcr.io/acme/api:v1":            "api",
		"ghcr.io/acme/api@" + digestA:    "api",
		"localhost:5051/e2eh/run/whoami": "whoami",
		"registry.local:5000/x/y:t":      "y",
	} {
		if got := HostedArtifactName(in); got != want {
			t.Errorf("HostedArtifactName(%q) = %q, want %q", in, got, want)
		}
	}
	if got := HostedImageRepository("localhost:5051/a/b:tag"); got != "localhost:5051/a/b" {
		t.Errorf("repository = %q", got)
	}
}
