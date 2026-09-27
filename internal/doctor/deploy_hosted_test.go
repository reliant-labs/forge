package doctor

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// shapedRender is renderFromJSON plus the deploy path's classification,
// applied the way renderDeployEnvs applies a shaper's answer.
func shapedRender(t *testing.T, env, body string, shape DeployShape) envRender {
	t.Helper()
	r := renderFromJSON(t, env, body)
	r.applyShape(shape)
	return r
}

// The houndersclub Deployability failure, at the unit level: a hosted env's
// `manifests` is empty by design, and the check used to call that "render
// produced no k8s objects". The shape says it is hosted and admitted.
func TestCheckDeployManifests_HostedEnvIsJudgedByItsDeployPath(t *testing.T) {
	empty := `{"manifests":[],"output":{"services":[]}}`
	cases := []struct {
		name     string
		shape    DeployShape
		body     string
		want     Status
		evidence string
	}{
		{
			name:  "admitted hosted env with an empty stream passes",
			shape: DeployShape{Destinations: []string{"hosted"}, Hosted: true, Workloads: 3},
			body:  empty,
			want:  StatusPass,
		},
		{
			name: "the deploy path's refusal is the finding",
			shape: DeployShape{Destinations: []string{"hosted"}, Hosted: true,
				Refusal: errors.New(`hosted env "prod": refusing to publish anything — 1 workload(s) are not admissible:\n  api: off the shape band`)},
			body:     empty,
			want:     StatusFail,
			evidence: "off the shape band",
		},
		{
			name:     "a hosted env rendering k8s objects fails: no hosted deploy applies them",
			shape:    DeployShape{Destinations: []string{"hosted"}, Hosted: true, Workloads: 1},
			body:     `{"manifests":[{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"stray"}}],"output":{}}`,
			want:     StatusFail,
			evidence: "a hosted deploy applies none of them",
		},
		{
			// An env the deploy path runs entirely on this machine applies
			// nothing to Kubernetes; its empty stream is correct.
			name:  "host-only env with an empty stream passes",
			shape: DeployShape{Destinations: []string{"host"}},
			body:  empty,
			want:  StatusPass,
		},
		{
			// A cluster env that renders nothing is still the defect.
			name:     "cluster env with an empty stream still fails",
			shape:    DeployShape{Destinations: []string{"cluster"}},
			body:     empty,
			want:     StatusFail,
			evidence: "no k8s objects",
		},
		{
			// Unclassified (no contract, or no shaper) keeps the strict
			// reading: nothing has shown it exempt.
			name:     "unclassified env with an empty stream still fails",
			shape:    DeployShape{},
			body:     `{"manifests":[]}`,
			want:     StatusFail,
			evidence: "no k8s objects",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := envWithRender([]envRender{shapedRender(t, "prod", tc.body, tc.shape)})
			got := CheckDeployManifests(context.Background(), env)
			if got.Status != tc.want {
				t.Fatalf("status = %s, want %s (%s)\n%s", got.Status, tc.want, got.Message, got.Evidence)
			}
			if tc.evidence != "" && !strings.Contains(got.Evidence, tc.evidence) {
				t.Errorf("evidence does not mention %q:\n%s", tc.evidence, got.Evidence)
			}
		})
	}
}

// A hosted env's platform objects are what RUNS, so the content checks read
// them — but they land in a namespace the platform allocates, so the checks
// about forge's own applies (object collisions) must never see them.
func TestHostedPlatformObjectsFeedContentChecksOnly(t *testing.T) {
	platform := `[{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"api","namespace":"hosted-platform"},` +
		`"spec":{"template":{"spec":{"containers":[{"name":"api","ports":[{"containerPort":8080}],` +
		`"resources":{"requests":{"cpu":"250m","memory":"1Gi"},"limits":{"cpu":"250m","memory":"1Gi"}}}]}}}}]`
	shape := DeployShape{Destinations: []string{"hosted"}, Hosted: true, Workloads: 1, PlatformObjects: []byte(platform)}
	// Two hosted envs of one project: their platform objects share a name
	// and a placeholder namespace, which is NOT a collision — the platform
	// gives each env its own namespace.
	env := envWithRender([]envRender{
		shapedRender(t, "prod", `{"manifests":[],"output":{}}`, shape),
		shapedRender(t, "staging", `{"manifests":[],"output":{}}`, shape),
	})

	probes := CheckDeployProbes(context.Background(), env)
	if probes.Status != StatusFail || !strings.Contains(probes.Evidence, "hosted tier") ||
		!strings.Contains(probes.Evidence, "healthCheck = tiers.HealthCheck") {
		t.Errorf("Deploy Probes did not judge the hosted backend the platform runs: %s\n%s", probes.Status, probes.Evidence)
	}
	if res := CheckDeployResources(context.Background(), env); res.Status != StatusPass {
		t.Errorf("Deploy Resources = %s: %s", res.Status, res.Message)
	}
	if res := CheckObjectCollision(context.Background(), env); res.Status == StatusFail {
		t.Errorf("Object Collision reported two hosted envs' platform objects as a collision: %s\n%s", res.Message, res.Evidence)
	}
}

// An undecodable platform object list is a forge defect; the env is reported
// unreadable (UNDETERMINED for content checks), never judged on half its
// workloads.
func TestHostedUndecodablePlatformObjectsAreUnread(t *testing.T) {
	r := shapedRender(t, "prod", `{"manifests":[],"output":{}}`,
		DeployShape{Destinations: []string{"hosted"}, Hosted: true, PlatformObjects: []byte("{not json")})
	if r.err == nil {
		t.Fatal("an undecodable platform object list was accepted")
	}
	env := envWithRender([]envRender{r})
	if res := CheckDeployProbes(context.Background(), env); res.Status != StatusUnknown {
		t.Errorf("Deploy Probes = %s over an env it could not read, want unknown", res.Status)
	}
}
