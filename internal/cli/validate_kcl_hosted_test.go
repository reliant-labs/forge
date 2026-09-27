package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/doctor"
)

// hostedValidateBundle is hounders' prod shape (deploy/kcl/prod/main.k at
// houndersclub PR #1): a SimpleBackend this project builds, a StaticSite and
// a ManagedDatabase, all through a control plane, with cross-workload
// references in both directions and a managed secret. `api` is spliced in so
// a test can break exactly one thing.
func hostedValidateBundle(api string) string {
	return `    project = "hounders"
    env = "prod"
    control_plane = forge.ControlPlane { endpoint = "http://127.0.0.1:8090" }
    secret_provider = forge.HostedSecrets {}
    services = [forge.RenderedWorkload {
        name = "api"
        image = "hounders"
        build = forge.build_of({type = "go", cmd = "./cmd/hounders", output_name = "hounders"})
        deploy = forge.SimpleBackend {
            spec = ` + api + `
        }
    }]
    databases = [forge.ManagedDatabase {
        name = "hounders"
        spec = tiers.ManagedDatabase {storageGiB = 1}
    }]
    frontends = [forge.Frontend {
        name = "web"
        path = "frontends/web"
        deploy = forge.StaticSite {public_dir = "out"}
        runtime_config = {
            API_URL = forge.WorkloadURL {workload = "api"}
            SITE_URL = forge.WorkloadURL {workload = "web"}
        }
    }]`
}

const hostedValidateAPI = `tiers.SimpleBackend {
                image = "hounders"
                ports = [8080]
                network = "public"
                healthCheck = tiers.HealthCheck {port = 8080, path = "/healthz", initialDelaySeconds = 10}
                env = [
                    tiers.EnvVar {name = "AUTO_MIGRATE", value = "true"}
                    tiers.EnvVar {name = "CORS_ORIGINS", workloadURL = forge.WorkloadURL {workload = "web"}}
                    tiers.EnvVar {name = "DATABASE_URL", databaseRef = tiers.DatabaseRef {name = "hounders"}}
                    tiers.EnvVar {name = "STRIPE_SECRET_KEY", managedSecret = "STRIPE_SECRET_KEY"}
                ]
            }`

// TestValidateKCL_HostedEnvIsJudgedByItsDeployPath is the houndersclub
// Deployability failure: `forge ci validate-kcl` reported
//
//	prod: render produced no k8s objects
//
// for a hosted env, whose empty manifest stream is CORRECT — the control
// plane runs it. It must pass, and say what it validated instead.
func TestValidateKCL_HostedEnvIsJudgedByItsDeployPath(t *testing.T) {
	if testing.Short() {
		t.Skip("renders KCL; skipped in -short")
	}
	dir := workloadURLProject(t, "prod", hostedValidateBundle(hostedValidateAPI))
	t.Chdir(dir)

	out, err := runForge(t, "ci", "validate-kcl")
	if err != nil {
		t.Fatalf("validate-kcl refused a hosted env the deploy path admits: %v\n%s", err, out)
	}
	if !strings.Contains(out, "1 hosted env(s), 3 tier workload(s) the control plane would admit") {
		t.Errorf("validate-kcl passed without saying what it validated for the hosted env:\n%s", out)
	}
}

// TestValidateKCL_BrokenHostedEnvIsStillCaught: judging a hosted env by its
// deploy path must not turn into skipping it. Each case is something
// `forge env deploy` would refuse, and each must fail validate-kcl with the
// deploy path's own words.
func TestValidateKCL_BrokenHostedEnvIsStillCaught(t *testing.T) {
	if testing.Short() {
		t.Skip("renders KCL; skipped in -short")
	}
	cases := []struct {
		name, api, want string
	}{
		{
			// Off the hosted shape band: planHosted's CheckShapeBand.
			name: "off-band resources",
			api: `tiers.SimpleBackend {
                image = "hounders"
                ports = [8080]
                network = "public"
                resources = tiers.Resources {cpuRequestMillicores = 250, memoryRequestBytes = 134217728}
            }`,
			want: "shape band",
		},
		{
			// A databaseRef nothing publishes: the pod would never start.
			name: "dangling databaseRef",
			api: `tiers.SimpleBackend {
                image = "hounders"
                ports = [8080]
                network = "public"
                env = [tiers.EnvVar {name = "DATABASE_URL", databaseRef = tiers.DatabaseRef {name = "orders"}}]
            }`,
			want: `databaseRef "orders"`,
		},
		{
			// The site's API_URL names a private backend: the control
			// plane's resolver refuses it permanently.
			name: "workloadURL to a private backend",
			api: `tiers.SimpleBackend {
                image = "hounders"
                ports = [8080]
                network = "private"
            }`,
			want: "only a public backend has a URL",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := workloadURLProject(t, "prod", hostedValidateBundle(tc.api))
			t.Chdir(dir)
			res := doctor.CheckDeployManifests(context.Background(), &doctor.Environment{ProjectDir: dir, DeployShaper: deployShapeOf})
			if res.Status != doctor.StatusFail {
				t.Fatalf("status = %s (%s), want fail: a hosted env the deploy path refuses passed validation", res.Status, res.Message)
			}
			if !strings.Contains(res.Evidence, "would refuse it before publishing anything") || !strings.Contains(res.Evidence, tc.want) {
				t.Errorf("evidence does not carry the deploy path's refusal (want %q):\n%s", tc.want, res.Evidence)
			}
			if _, err := runForge(t, "ci", "validate-kcl"); err == nil {
				t.Error("`forge ci validate-kcl` exited 0 on a hosted env `forge env deploy` would refuse")
			}
		})
	}
}

// TestValidateKCL_HostedEnvRenderingManifestsFails: a hosted deploy applies
// NOTHING, so a k8s object in a hosted env's `manifests` is one no deploy
// will ever create — the silent-drop shape CheckDeployManifests exists for.
func TestValidateKCL_HostedEnvRenderingManifestsFails(t *testing.T) {
	if testing.Short() {
		t.Skip("renders KCL; skipped in -short")
	}
	dir := workloadURLProject(t, "prod", hostedValidateBundle(hostedValidateAPI))
	rewriteFile(t, filepath.Join(dir, "deploy", "kcl", "prod", "main.k"), func(body string) string {
		i := strings.Index(body, "manifests = ")
		if i < 0 {
			t.Fatal("fixture main.k has no manifests root")
		}
		return body[:i] + "manifests = [{apiVersion = \"v1\", kind = \"ConfigMap\", metadata = {name = \"stray\"}}]\n"
	})
	t.Chdir(dir)
	res := doctor.CheckDeployManifests(context.Background(), &doctor.Environment{ProjectDir: dir, DeployShaper: deployShapeOf})
	if res.Status != doctor.StatusFail || !strings.Contains(res.Evidence, "a hosted deploy applies none of them") {
		t.Fatalf("status = %s, evidence:\n%s\nwant a failure naming the object no hosted deploy applies", res.Status, res.Evidence)
	}
}

// TestDoctorDeploy_HostedEnvContentChecksReadThePlatformObjects: the content
// checks judge a hosted env by what the control plane RUNS (pkg/deploy
// .Render of each tier), never by its empty stream — so a hosted backend
// with no healthCheck fails Deploy Probes, and one with a /healthz check
// for both probes (hounders' deliberate choice: a DB-backed readiness probe
// as liveness would restart every replica on a DB blip) passes.
func TestDoctorDeploy_HostedEnvContentChecksReadThePlatformObjects(t *testing.T) {
	if testing.Short() {
		t.Skip("renders KCL; skipped in -short")
	}
	unprobed := strings.Replace(hostedValidateAPI,
		`healthCheck = tiers.HealthCheck {port = 8080, path = "/healthz", initialDelaySeconds = 10}`, "", 1)
	for _, tc := range []struct {
		name, api string
		want      doctor.Status
	}{
		{"healthz for both probes passes", hostedValidateAPI, doctor.StatusPass},
		{"no healthCheck fails", unprobed, doctor.StatusFail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := workloadURLProject(t, "prod", hostedValidateBundle(tc.api))
			// A real migration, so Deploy Migrations has to FIND the hosted
			// backend's AUTO_MIGRATE rather than skip for want of SQL.
			writeTestFile(t, filepath.Join(dir, "db", "migrations", "00001_init.up.sql"), "CREATE TABLE t (id TEXT PRIMARY KEY);\n")
			t.Chdir(dir)
			env := &doctor.Environment{ProjectDir: dir, DeployShaper: deployShapeOf}
			probes := doctor.CheckDeployProbes(context.Background(), env)
			if probes.Status != tc.want {
				t.Fatalf("Deploy Probes = %s (%s)\n%s", probes.Status, probes.Message, probes.Evidence)
			}
			if tc.want == doctor.StatusFail && !strings.Contains(probes.Evidence, "healthCheck = tiers.HealthCheck") {
				t.Errorf("a hosted probe failure must name the tier field to set:\n%s", probes.Evidence)
			}
			// Everything else the platform runs for this tier is on the
			// happy path: forge's renderer sets requests/limits, sources
			// every credential from a Secret and binds its own SA.
			for name, check := range map[string]doctor.CheckFunc{
				"resources":       doctor.CheckDeployResources,
				"secrets":         doctor.CheckDeploySecrets,
				"service account": doctor.CheckDeployServiceAccount,
				"migrations":      doctor.CheckDeployMigrations,
			} {
				res := check(context.Background(), env)
				if res.Status == doctor.StatusFail || res.Status == doctor.StatusUnknown {
					t.Errorf("%s = %s on a well-formed hosted env: %s\n%s", name, res.Status, res.Message, res.Evidence)
				}
			}
			if res := doctor.CheckDeploySecrets(context.Background(), env); !strings.Contains(res.Message, "all sourced from a Secret") {
				t.Errorf("Deploy Secrets did not read the hosted backend's credentials (managedSecret, databaseRef): %s", res.Message)
			}
			if res := doctor.CheckDeployMigrations(context.Background(), env); res.Status != doctor.StatusPass {
				t.Errorf("Deploy Migrations = %s: the hosted backend's AUTO_MIGRATE was not read: %s", res.Status, res.Message)
			}
		})
	}
}

func rewriteFile(t *testing.T, path string, edit func(string) string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	writeTestFile(t, path, edit(string(b)))
}
