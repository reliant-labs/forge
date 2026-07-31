package doctor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// envWithRender builds an Environment whose KCL render is pre-seeded,
// so the deploy checks are exercised against known manifests without
// evaluating KCL. The seeding closes the sync.Once, which is exactly
// what production does after the first check renders.
func envWithRender(renders []envRender) *Environment {
	e := &Environment{ProjectName: "test", ProjectDir: "/nonexistent"}
	e.deployOnce.Do(func() { e.deployCache = renders })
	return e
}

// renderFromJSON parses a KCL-shaped render body into an envRender the
// way renderDeployEnvs does, so the tests exercise the real parser.
func renderFromJSON(t *testing.T, env, body string) envRender {
	t.Helper()
	r := parseRender([]byte(body))
	if r.err != nil {
		t.Fatalf("parseRender(%s): %v", env, r.err)
	}
	r.env = env
	return r
}

// deployJSON renders a one-container Deployment with the supplied
// container body spliced in, so each test states only the field under
// test.
func deployJSON(container string) string {
	return `{"manifests":[{"apiVersion":"apps/v1","kind":"Deployment",` +
		`"metadata":{"name":"api","namespace":"proj-prod"},` +
		`"spec":{"template":{"spec":{"containers":[` + container + `]}}}}]}`
}

const probedResourcedContainer = `{"name":"api",
	"ports":[{"containerPort":8080,"name":"http","protocol":"TCP"}],
	"readinessProbe":{"httpGet":{"path":"/readyz","port":8080}},
	"livenessProbe":{"httpGet":{"path":"/healthz","port":8080}},
	"resources":{"requests":{"cpu":"500m","memory":"512Mi"},"limits":{"cpu":"2","memory":"1Gi"}}}`

func TestParseRender(t *testing.T) {
	tests := []struct {
		name        string
		body        string
		wantObjects int
		wantRoot    bool
		wantStrays  []string
		wantInvalid int
	}{
		{
			name:        "bare list is applyable",
			body:        `[{"apiVersion":"v1","kind":"Service","metadata":{"name":"s"}}]`,
			wantObjects: 1,
			wantRoot:    true,
		},
		{
			name:        "single object is applyable",
			body:        `{"apiVersion":"v1","kind":"Service","metadata":{"name":"s"}}`,
			wantObjects: 1,
			wantRoot:    true,
		},
		{
			name:        "manifests-only mapping",
			body:        `{"manifests":[{"apiVersion":"v1","kind":"Service","metadata":{"name":"s"}}]}`,
			wantObjects: 1,
			wantRoot:    true,
		},
		{
			// The dual-output shape IS the forge env deploy contract: the
			// pipeline selects `-S manifests`, and `output` is the JSON
			// entity contract `forge build` / `forge env deploy` read
			// from the same render. It carries no k8s objects, so it is
			// not a stray root.
			name: "documented output sibling is not a stray root",
			body: `{"manifests":[{"apiVersion":"v1","kind":"Service","metadata":{"name":"s"}}],` +
				`"output":{"services":[]}}`,
			wantObjects: 1,
			wantRoot:    true,
		},
		{
			// A second list of k8s objects under its own key is the real
			// trap: nothing selects it, so it renders, reviews clean, and
			// never reaches a cluster.
			name: "k8s objects under a non-reserved root are unreachable",
			body: `{"manifests":[{"apiVersion":"v1","kind":"Service","metadata":{"name":"s"}}],` +
				`"extra_manifests":[{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"c"}}]}`,
			wantObjects: 1,
			wantRoot:    true,
			wantStrays:  []string{"extra_manifests"},
		},
		{
			name:     "no manifests root means nothing deploys",
			body:     `{"output":{"services":[]}}`,
			wantRoot: false,
		},
		{
			name: "manifest entries missing apiVersion/kind are counted",
			body: `{"manifests":[{"apiVersion":"v1","kind":"Service","metadata":{"name":"s"}},` +
				`{"metadata":{"name":"broken"}}]}`,
			wantObjects: 2,
			wantRoot:    true,
			wantInvalid: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := parseRender([]byte(tt.body))
			if r.err != nil {
				t.Fatalf("unexpected error: %v", r.err)
			}
			if len(r.objects) != tt.wantObjects {
				t.Errorf("objects = %d, want %d", len(r.objects), tt.wantObjects)
			}
			if r.hasManifestRoot != tt.wantRoot {
				t.Errorf("hasManifestRoot = %v, want %v", r.hasManifestRoot, tt.wantRoot)
			}
			if strings.Join(r.strayRoots, ",") != strings.Join(tt.wantStrays, ",") {
				t.Errorf("strayRoots = %v, want %v", r.strayRoots, tt.wantStrays)
			}
			if len(r.invalid) != tt.wantInvalid {
				t.Errorf("invalid = %v, want %d entries", r.invalid, tt.wantInvalid)
			}
		})
	}
}

func TestCheckDeployManifests(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		want     Status
		evidence string
	}{
		{
			name: "applyable manifest list passes",
			body: `{"manifests":[{"apiVersion":"v1","kind":"Service","metadata":{"name":"s"}}]}`,
			want: StatusPass,
		},
		{
			// The dual-output shape is the deploy contract, not a defect:
			// the pipeline selects `-S manifests` and `forge build` reads
			// `output` from the same render.
			name: "documented output sibling passes",
			body: `{"manifests":[{"apiVersion":"v1","kind":"Service","metadata":{"name":"s"}}],` +
				`"output":{"services":[]}}`,
			want: StatusPass,
		},
		{
			name: "k8s objects under an unreachable root fail and name it",
			body: `{"manifests":[{"apiVersion":"v1","kind":"Service","metadata":{"name":"s"}}],` +
				`"extra_manifests":[{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"c"}}]}`,
			want:     StatusFail,
			evidence: "extra_manifests",
		},
		{
			name:     "no manifests root fails",
			body:     `{"output":{"services":[]}}`,
			want:     StatusFail,
			evidence: "no `manifests` root",
		},
		{
			name: "manifest kubectl would reject fails and names it",
			body: `{"manifests":[{"metadata":{"name":"broken"}}]}`,
			want: StatusFail,
			// kubectl's own words for this render: "apiVersion not set, kind not set".
			evidence: "no apiVersion, kind",
		},
		{
			name:     "render with no objects fails",
			body:     `{"manifests":[]}`,
			want:     StatusFail,
			evidence: "no k8s objects",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := envWithRender([]envRender{renderFromJSON(t, "prod", tt.body)})
			got := CheckDeployManifests(context.Background(), env)
			if got.Status != tt.want {
				t.Fatalf("status = %q, want %q (msg=%s)", got.Status, tt.want, got.Message)
			}
			if tt.evidence != "" && !strings.Contains(got.Evidence, tt.evidence) {
				t.Errorf("evidence %q does not mention %q", got.Evidence, tt.evidence)
			}
		})
	}
}

func TestCheckDeployProbes(t *testing.T) {
	tests := []struct {
		name      string
		container string
		want      Status
		evidence  string
	}{
		{
			name:      "both probes present",
			container: probedResourcedContainer,
			want:      StatusPass,
		},
		{
			name:      "serving container with no probes fails",
			container: `{"name":"api","ports":[{"containerPort":8080}]}`,
			want:      StatusFail,
			evidence:  "readinessProbe or livenessProbe",
		},
		{
			// A container that declares no port has no endpoint to probe.
			// An HTTP probe against it is a guaranteed CrashLoopBackOff,
			// so demanding one would push forge to emit a wrong manifest.
			name:      "container that serves no port is exempt",
			container: `{"name":"oneshot"}`,
			want:      StatusPass,
		},
		{
			name:      "readiness only still fails",
			container: `{"name":"api","ports":[{"containerPort":8080}],"readinessProbe":{"httpGet":{"path":"/readyz"}}}`,
			want:      StatusFail,
			evidence:  "livenessProbe",
		},
		{
			// Half a probe pair is always a defect, ports or not.
			name:      "one probe without ports still fails",
			container: `{"name":"api","livenessProbe":{"httpGet":{"path":"/healthz"}}}`,
			want:      StatusFail,
			evidence:  "readinessProbe",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := envWithRender([]envRender{renderFromJSON(t, "prod", deployJSON(tt.container))})
			got := CheckDeployProbes(context.Background(), env)
			if got.Status != tt.want {
				t.Fatalf("status = %q, want %q (msg=%s)", got.Status, tt.want, got.Message)
			}
			if tt.evidence != "" && !strings.Contains(got.Evidence, tt.evidence) {
				t.Errorf("evidence %q does not mention %q", got.Evidence, tt.evidence)
			}
		})
	}
}

func TestCheckDeployResources(t *testing.T) {
	tests := []struct {
		name      string
		container string
		want      Status
		evidence  string
	}{
		{
			name:      "requests and limits for cpu and memory",
			container: probedResourcedContainer,
			want:      StatusPass,
		},
		{
			name:      "no resources block",
			container: `{"name":"api"}`,
			want:      StatusFail,
			evidence:  "requests, limits",
		},
		{
			name:      "limits without memory",
			container: `{"name":"api","resources":{"requests":{"cpu":"1","memory":"1Gi"},"limits":{"cpu":"2"}}}`,
			want:      StatusFail,
			evidence:  "limits.memory",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := envWithRender([]envRender{renderFromJSON(t, "prod", deployJSON(tt.container))})
			got := CheckDeployResources(context.Background(), env)
			if got.Status != tt.want {
				t.Fatalf("status = %q, want %q (msg=%s)", got.Status, tt.want, got.Message)
			}
			if tt.evidence != "" && !strings.Contains(got.Evidence, tt.evidence) {
				t.Errorf("evidence %q does not mention %q", got.Evidence, tt.evidence)
			}
		})
	}
}

func TestCheckDeploySecrets(t *testing.T) {
	envEntry := func(entries string) string {
		return `{"name":"api","env":[` + entries + `]}`
	}

	tests := []struct {
		name      string
		container string
		want      Status
		evidence  string
	}{
		{
			name:      "secretKeyRef passes",
			container: envEntry(`{"name":"DATABASE_URL","valueFrom":{"secretKeyRef":{"name":"db","key":"url"}}}`),
			want:      StatusPass,
		},
		{
			name:      "plaintext DSN fails",
			container: envEntry(`{"name":"DATABASE_URL","value":"postgres://u:p@h/db"}`),
			want:      StatusFail,
			evidence:  "DATABASE_URL",
		},
		{
			name:      "empty plaintext slot still fails",
			container: envEntry(`{"name":"DATABASE_URL","value":""}`),
			want:      StatusFail,
			evidence:  "DATABASE_URL",
		},
		{
			name:      "boolean toggle whose name contains CREDENTIAL is not a secret",
			container: envEntry(`{"name":"CORS_ALLOW_CREDENTIALS","value":"false"}`),
			want:      StatusPass,
		},
		{
			name:      "numeric toggle is not a secret",
			container: envEntry(`{"name":"TOKEN_TTL_SECONDS","value":"3600"}`),
			want:      StatusPass,
		},
		{
			name:      "no credential-shaped names at all",
			container: envEntry(`{"name":"LOG_LEVEL","value":"info"}`),
			want:      StatusPass,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := envWithRender([]envRender{renderFromJSON(t, "prod", deployJSON(tt.container))})
			got := CheckDeploySecrets(context.Background(), env)
			if got.Status != tt.want {
				t.Fatalf("status = %q, want %q (msg=%s ev=%s)", got.Status, tt.want, got.Message, got.Evidence)
			}
			if tt.evidence != "" && !strings.Contains(got.Evidence, tt.evidence) {
				t.Errorf("evidence %q does not mention %q", got.Evidence, tt.evidence)
			}
		})
	}
}

func TestCheckDeployServiceAccount(t *testing.T) {
	const sa = `{"apiVersion":"v1","kind":"ServiceAccount","metadata":{"name":"api","namespace":"proj-prod"}}`
	deployWith := func(podExtra string) string {
		return `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"api","namespace":"proj-prod"},` +
			`"spec":{"template":{"spec":{` + podExtra + `"containers":[{"name":"api"}]}}}}`
	}

	tests := []struct {
		name     string
		body     string
		want     Status
		evidence string
	}{
		{
			name: "bound service account passes",
			body: `{"manifests":[` + sa + `,` + deployWith(`"serviceAccountName":"api",`) + `]}`,
			want: StatusPass,
		},
		{
			name:     "rendered but unbound fails",
			body:     `{"manifests":[` + sa + `,` + deployWith("") + `]}`,
			want:     StatusFail,
			evidence: "serviceAccountName",
		},
		{
			name: "no service account skips",
			body: `{"manifests":[` + deployWith("") + `]}`,
			want: StatusSkip,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			env := envWithRender([]envRender{renderFromJSON(t, "prod", tt.body)})
			got := CheckDeployServiceAccount(context.Background(), env)
			if got.Status != tt.want {
				t.Fatalf("status = %q, want %q (msg=%s)", got.Status, tt.want, got.Message)
			}
			if tt.evidence != "" && !strings.Contains(got.Evidence, tt.evidence) {
				t.Errorf("evidence %q does not mention %q", got.Evidence, tt.evidence)
			}
		})
	}
}

// A project with no deploy/kcl at all must stay quiet rather than fail:
// --kind cli and --kind library legitimately ship no manifests.
func TestDeployChecksSkipWithoutEnvironments(t *testing.T) {
	env := envWithRender(nil)
	for name, fn := range map[string]CheckFunc{
		"manifests": CheckDeployManifests,
		"probes":    CheckDeployProbes,
		"resources": CheckDeployResources,
		"secrets":   CheckDeploySecrets,
		"sa":        CheckDeployServiceAccount,
	} {
		if got := fn(context.Background(), env); got.Status != StatusSkip {
			t.Errorf("%s: status = %q, want %q", name, got.Status, StatusSkip)
		}
	}
}

// A render that failed to evaluate must fail loudly, not silently pass
// because there were no objects to inspect.
func TestDeployChecksFailOnBrokenRender(t *testing.T) {
	env := envWithRender([]envRender{{env: "prod", err: errRenderTest}})
	got := CheckDeployManifests(context.Background(), env)
	if got.Status != StatusFail {
		t.Fatalf("status = %q, want %q", got.Status, StatusFail)
	}
	if !strings.Contains(got.Evidence, "boom") {
		t.Errorf("evidence %q does not carry the render error", got.Evidence)
	}
}

var errRenderTest = errors.New("boom: kcl evaluation failed")

// A project that ships SQL migrations must have SOME way of applying
// them. The generated cloud envs correctly set AUTO_MIGRATE=false —
// three replicas racing to migrate on startup is not a strategy — but
// nothing replaced it, so a schema-changing release deployed new code
// against the old schema and the first request touching a new column
// was the discovery mechanism.
// TestCheckDeploySecrets_InitContainers: the migration initContainer runs
// with the SAME env as the app container, so a credential carried as a
// literal there leaks exactly the same way. The check descends into
// initContainers for that reason — and a leak in EITHER container is a
// leak.
func TestCheckDeploySecrets_InitContainers(t *testing.T) {
	appEnv := `{"name":"api","env":[{"name":"DATABASE_URL","valueFrom":{"secretKeyRef":{"name":"db","key":"url"}}}]}`

	deployWithInit := func(initContainer string) string {
		return `{"manifests":[{"apiVersion":"apps/v1","kind":"Deployment",` +
			`"metadata":{"name":"api","namespace":"proj-prod"},` +
			`"spec":{"template":{"spec":{"initContainers":[` + initContainer + `],` +
			`"containers":[` + appEnv + `]}}}}]}`
	}

	t.Run("secret-sourced init container passes", func(t *testing.T) {
		body := deployWithInit(`{"name":"migrate","command":["/app/demo","db","migrate","up"],` +
			`"env":[{"name":"DATABASE_URL","valueFrom":{"secretKeyRef":{"name":"db","key":"url"}}}]}`)
		env := envWithRender([]envRender{renderFromJSON(t, "prod", body)})
		if got := CheckDeploySecrets(context.Background(), env); got.Status != StatusPass {
			t.Fatalf("status = %q, want pass (msg=%s evidence=%s)", got.Status, got.Message, got.Evidence)
		}
	})

	t.Run("plaintext DSN in an init container fails", func(t *testing.T) {
		body := deployWithInit(`{"name":"migrate","command":["/app/demo","db","migrate","up"],` +
			`"env":[{"name":"DATABASE_URL","value":"postgres://u:p@h/db"}]}`)
		env := envWithRender([]envRender{renderFromJSON(t, "prod", body)})
		got := CheckDeploySecrets(context.Background(), env)
		if got.Status != StatusFail {
			t.Fatalf("status = %q, want fail — a literal credential in an initContainer "+
				"is the same leak (msg=%s)", got.Status, got.Message)
		}
		if !strings.Contains(got.Evidence, "migrate") {
			t.Errorf("evidence %q does not name the offending container", got.Evidence)
		}
	})
}

func TestCheckDeployMigrations(t *testing.T) {
	const plainContainer = `{"name":"api","env":[{"name":"AUTO_MIGRATE","value":"false"}]}`

	tests := []struct {
		name       string
		migrations []string
		body       string
		want       Status
		evidence   string
	}{
		{
			name:       "no migrations dir skips",
			migrations: nil,
			body:       deployJSON(plainContainer),
			want:       StatusSkip,
		},
		{
			name:       "migrations with no way to apply them fails",
			migrations: []string{"0001_init.up.sql"},
			body:       deployJSON(plainContainer),
			want:       StatusFail,
			evidence:   "no migration Job",
		},
		{
			// The gate must stay honest for a project that genuinely has no
			// path. An UNRELATED init container (waiting on a dependency,
			// templating a config) used to satisfy the check by existing.
			name:       "an unrelated initContainer is NOT a migration path",
			migrations: []string{"0001_init.up.sql"},
			body: `{"manifests":[{"apiVersion":"apps/v1","kind":"Deployment",` +
				`"metadata":{"name":"api","namespace":"proj-prod"},` +
				`"spec":{"template":{"spec":{"initContainers":[` +
				`{"name":"wait-for-db","command":["sh","-c","until nc -z db 5432; do sleep 1; done"]}` +
				`],"containers":[` + plainContainer + `]}}}}]}`,
			want:     StatusFail,
			evidence: "no migration Job",
		},
		{
			name:       "AUTO_MIGRATE=true is a way to apply them",
			migrations: []string{"0001_init.up.sql"},
			body:       deployJSON(`{"name":"api","env":[{"name":"AUTO_MIGRATE","value":"true"}]}`),
			want:       StatusPass,
		},
		{
			name:       "a migration Job is a way to apply them",
			migrations: []string{"0001_init.up.sql"},
			body: `{"manifests":[{"apiVersion":"batch/v1","kind":"Job",` +
				`"metadata":{"name":"db-migrate","namespace":"proj-prod"},` +
				`"spec":{"template":{"spec":{"containers":[{"name":"migrate"}]}}}},` +
				`{"apiVersion":"apps/v1","kind":"Deployment",` +
				`"metadata":{"name":"api","namespace":"proj-prod"},` +
				`"spec":{"template":{"spec":{"containers":[` + plainContainer + `]}}}}]}`,
			want: StatusPass,
		},
		{
			name:       "a migration initContainer is a way to apply them",
			migrations: []string{"0001_init.up.sql"},
			body: `{"manifests":[{"apiVersion":"apps/v1","kind":"Deployment",` +
				`"metadata":{"name":"api","namespace":"proj-prod"},` +
				`"spec":{"template":{"spec":{"initContainers":[{"name":"migrate"}],"containers":[` +
				plainContainer + `]}}}}]}`,
			want: StatusPass,
		},
		{
			// The shape forge itself renders: the init container is named
			// "migrate" AND runs `<binary> db migrate up`. Either half alone
			// identifies it, so a project that renames the container keeps
			// passing.
			name:       "the rendered migrate initContainer passes on its command alone",
			migrations: []string{"0001_init.up.sql"},
			body: `{"manifests":[{"apiVersion":"apps/v1","kind":"Deployment",` +
				`"metadata":{"name":"api","namespace":"proj-prod"},` +
				`"spec":{"template":{"spec":{"initContainers":[` +
				`{"name":"schema","command":["/app/demo","db","migrate","up"]}` +
				`],"containers":[` + plainContainer + `]}}}}]}`,
			want: StatusPass,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.migrations != nil {
				mig := filepath.Join(dir, "db", "migrations")
				if err := os.MkdirAll(mig, 0o755); err != nil {
					t.Fatal(err)
				}
				for _, name := range tt.migrations {
					if err := os.WriteFile(filepath.Join(mig, name), []byte("SELECT 1;"), 0o644); err != nil {
						t.Fatal(err)
					}
				}
			}
			env := envWithRender([]envRender{renderFromJSON(t, "prod", tt.body)})
			env.ProjectDir = dir

			got := CheckDeployMigrations(context.Background(), env)
			if got.Status != tt.want {
				t.Fatalf("status = %q, want %q (msg=%s)", got.Status, tt.want, got.Message)
			}
			if tt.evidence != "" && !strings.Contains(got.Evidence, tt.evidence) {
				t.Errorf("evidence %q does not mention %q", got.Evidence, tt.evidence)
			}
		})
	}
}
