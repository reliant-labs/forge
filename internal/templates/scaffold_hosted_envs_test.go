package templates_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
	"github.com/reliant-labs/forge/internal/kclrender"
)

// hostedConfigGenStub is the pipeline-generated config projection a fresh
// scaffold's envs import, in the exact shape codegen.GenerateConfigKCL emits
// (see TestScaffoldedIngressEvaluates): a sensitive DATABASE_URL reaches only
// the workloads that list it in `config_secrets`.
const hostedConfigGenStub = `import forge

schema ConfigSecretRef:
    name: str
    key: str

schema AppConfig:
    port: int = 8080
    database_url: ConfigSecretRef = ConfigSecretRef { name = "app-secrets", key = "database_url" }

APP_CONFIG_SENSITIVE_ENV: [str] = ["DATABASE_URL"]

appConfigEnvMap = lambda c: AppConfig, config_secrets: [str] -> {str: str | forge.SecretRef} {
    _sensitive: {str: forge.SecretRef} = {
        "DATABASE_URL" = forge.SecretRef {name = c.database_url.name, key = c.database_url.key, store_key = "DATABASE_URL"}
    }
    assert all _n in config_secrets { _n in _sensitive }, "unknown config_secrets name"
    {
        "PORT" = str(c.port)
    } | {_k: _sensitive[_k] for _k in _sensitive if _k in config_secrets}
}
`

// scaffoldForRender generates a fresh service project and stubs the config
// trio `forge generate` would write, so every env can be rendered offline
// through forge's own evaluation seam.
func scaffoldForRender(t *testing.T, name string, services []string, frontend string) string {
	t.Helper()
	tmp := t.TempDir()
	g := generator.NewProjectGenerator(name, tmp, "github.com/acme/"+name)
	g.Kind = config.ProjectKindService
	g.ApplyKindFeatureDefaults(config.ProjectKindService)
	if len(services) > 0 {
		g.ServiceName, g.AdditionalServices = services[0], services[1:]
	}
	g.FrontendName = frontend
	if err := g.Generate(); err != nil {
		t.Fatalf("Generate: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "deploy/kcl/config_gen.k"), []byte(hostedConfigGenStub), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, env := range []string{"dev", "staging", "prod"} {
		stub := "import config_gen\n\napp_config: config_gen.AppConfig = {\n}\n"
		if err := os.WriteFile(filepath.Join(tmp, "deploy/kcl", env, "config.k"), []byte(stub), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return tmp
}

// renderEnvOutput renders one env and returns its `output` contract.
func renderEnvOutput(t *testing.T, root, env string) map[string]any {
	t.Helper()
	out, err := kclrender.Run(root, filepath.Join(root, "deploy/kcl", env), []string{"env=" + env})
	if err != nil {
		t.Fatalf("render %s env: %v", env, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatalf("unmarshal %s: %v\n%s", env, err, out)
	}
	parsed, ok := doc["output"].(map[string]any)
	if !ok {
		t.Fatalf("%s render has no `output` contract:\n%s", env, out)
	}
	return parsed
}

// TestFreshScaffoldRendersEveryEnvHosted is the guard for hosting being the
// default deploy target: a project straight out of `forge project new`
// renders EVERY env it was born with — dev locally, staging and prod on the
// forge control plane — with nothing left to fill in first.
//
// Before, staging and prod bound every workload to a cluster the author had
// to name, and the frontend to a REPLACE_ME bucket; the first deploy stopped
// at "declared context not in kubeconfig". Now each deployed env publishes
// every workload, a managed database, the secrets and the static site to the
// control plane, and the cluster/bucket binders sit beside them unused.
//
// Asserted on the RENDER, because that is what every forge command reads; a
// string match on the template would pass a file that does not evaluate.
func TestFreshScaffoldRendersEveryEnvHosted(t *testing.T) {
	for _, tc := range []struct {
		name     string
		services []string
		frontend string
	}{
		{"two services and a frontend", []string{"orders", "billing"}, "web"},
		// The `forge project new` default: no service yet, no frontend.
		{"bare", nil, ""},
		// A site with no API yet: its runtime_config names no workload.
		{"frontend only", nil, "web"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := scaffoldForRender(t, "shop", tc.services, tc.frontend)

			if dev := renderEnvOutput(t, root, "dev"); dev["lifecycle"] != "local" {
				t.Errorf("dev must stay the local loop, got lifecycle %#v", dev["lifecycle"])
			}

			for _, env := range []string{"staging", "prod"} {
				raw, err := os.ReadFile(filepath.Join(root, "deploy/kcl", env, "main.k"))
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(raw), "REPLACE_ME") {
					t.Errorf("%s/main.k is born with a placeholder:\n%s", env, raw)
				}

				out := renderEnvOutput(t, root, env)
				if _, ok := out["control_plane"].(map[string]any); !ok {
					t.Errorf("%s: no control_plane — a hosted env publishes to one", env)
				}
				if sp, _ := out["secret_provider"].(map[string]any); sp["type"] != "hosted" {
					t.Errorf("%s: secret_provider = %#v, want the control plane's managed store", env, out["secret_provider"])
				}
				dbs, _ := out["databases"].([]any)
				if len(dbs) != 1 {
					t.Fatalf("%s: databases = %#v, want the one managed database", env, out["databases"])
				}
				if db, _ := dbs[0].(map[string]any); db["name"] != "shop" || runtimeType(db) != "hosted" {
					t.Errorf("%s: database = %#v, want `shop` on the hosted runtime", env, db)
				}

				workloads, _ := out["workloads"].([]any)
				// migrate, and the one API workload once there is a service
				// for it to run (TestHostedFrontendReachesEveryService).
				wantWorkloads := 1
				if len(tc.services) > 0 {
					wantWorkloads++
				}
				if len(workloads) != wantWorkloads {
					t.Fatalf("%s: %d workloads, want %d:\n%#v", env, len(workloads), wantWorkloads, workloads)
				}
				for _, w := range workloads {
					wm, _ := w.(map[string]any)
					if runtimeType(wm) != "hosted" {
						t.Errorf("%s: workload %v runs on %q, want hosted", env, wm["name"], runtimeType(wm))
					}
					// The platform admits only its own registry, so a hosted
					// image names no host (ADR-0003 F1).
					if img, _ := wm["image"].(string); img != "shop" {
						t.Errorf("%s: workload %v image = %q, want the bare project image", env, wm["name"], img)
					}
				}

				frontends, _ := out["frontends"].([]any)
				if tc.frontend == "" {
					if len(frontends) != 0 {
						t.Errorf("%s: no frontend was scaffolded, got %#v", env, frontends)
					}
					continue
				}
				if len(frontends) != 1 {
					t.Fatalf("%s: frontends = %#v, want the scaffolded one", env, frontends)
				}
				f, _ := frontends[0].(map[string]any)
				if runtimeType(f) != "hosted" || f["image"] != "web" || f["public_dir"] != "out" {
					t.Errorf("%s: frontend = %#v, want web on platform static hosting, bare image, out/", env, f)
				}
				spec, _ := f["runtime_config_spec"].(map[string]any)
				if len(tc.services) == 0 {
					if len(spec) != 0 {
						t.Errorf("%s: a site with no API names one: %#v", env, spec)
					}
					continue
				}
				// API_URL is a reference the control plane resolves to the
				// hosted API's allocated URL.
				api, _ := spec["API_URL"].(map[string]any)
				ref, _ := api["workloadURL"].(map[string]any)
				if ref["name"] != "api" {
					t.Errorf("%s: frontend runtime_config API_URL = %#v, want a workloadURL to api", env, spec["API_URL"])
				}
			}
		})
	}
}

// TestHostedFrontendReachesEveryService: in a hosted env, the ONE origin the
// frontend is given serves every service the project has.
//
// One origin is not a detail of the transport that a per-service URL map
// could route around. The scaffolded frontend dials one base URL — a Connect
// call is `/<package>.<Service>/<Method>`, so one origin can serve them all —
// and sign-in answers with an HttpOnly session cookie the browser returns to
// that origin only. The hosted platform gives every workload its own
// hostname and routes no paths between them.
//
// So #518, which hosted each service as its own workload and pointed API_URL
// at the first, shipped a frontend that could call one of three services. The
// fix is the topology: the env runs the binary's `server` — every service on
// one Connect mux — as one workload, and API_URL names it. Asserted on the
// render: the referenced workload runs `server`, and no other hosted service
// exists for the browser to miss.
func TestHostedFrontendReachesEveryService(t *testing.T) {
	root := scaffoldForRender(t, "shop", []string{"alpha", "beta", "gamma"}, "web")
	for _, env := range []string{"staging", "prod"} {
		out := renderEnvOutput(t, root, env)

		frontends, _ := out["frontends"].([]any)
		if len(frontends) != 1 {
			t.Fatalf("%s: frontends = %#v", env, frontends)
		}
		f, _ := frontends[0].(map[string]any)
		spec, _ := f["runtime_config_spec"].(map[string]any)
		apiURL, _ := spec["API_URL"].(map[string]any)
		ref, _ := apiURL["workloadURL"].(map[string]any)
		target, _ := ref["name"].(string)
		if target == "" {
			t.Fatalf("%s: the hosted frontend's API_URL references no workload: %#v", env, spec)
		}

		var hostedServices []string
		var api map[string]any
		for _, w := range out["workloads"].([]any) {
			wm := w.(map[string]any)
			if wm["kind"] == "service" && runtimeType(wm) == "hosted" {
				hostedServices = append(hostedServices, wm["name"].(string))
			}
			if wm["name"] == target {
				api = wm
			}
		}
		if api == nil {
			t.Fatalf("%s: API_URL names %q, which this env does not run", env, target)
		}
		spec, _ = api["spec"].(map[string]any)
		if args, _ := spec["args"].([]any); len(args) != 1 || args[0] != "server" {
			t.Errorf("%s: API_URL names %q, which runs %v — one service's subcommand mounts only that service, so the "+
				"frontend reaches one of alpha/beta/gamma; want the binary's `server`, which mounts them all", env, target, spec["args"])
		}
		if len(hostedServices) != 1 || hostedServices[0] != target {
			t.Errorf("%s: hosted services %v — every one but the API_URL target (%s) is a hostname the frontend never calls", env, hostedServices, target)
		}
		vars := map[string]map[string]any{}
		for _, e := range spec["env"].([]any) {
			em := e.(map[string]any)
			vars[em["name"].(string)] = em
		}
		if cors := vars["CORS_ORIGINS"]["workloadURL"]; cors == nil {
			t.Errorf("%s/%s: the API does not accept the site's browser calls (CORS_ORIGINS = %#v)", env, target, vars["CORS_ORIGINS"])
		}
		if db := vars["DATABASE_URL"]["databaseRef"]; db == nil {
			t.Errorf("%s/%s: the API does not read the managed database (DATABASE_URL = %#v)", env, target, vars["DATABASE_URL"])
		}
	}
}

// TestFreshOperatorRendersUntilOnlyTheClusterIsMissing: `forge scaffold
// operator` declares the operator before it has a CRD (`forge scaffold crd`
// is the next step), so its workload says `crds = []`. That declaration must
// render: in dev, where the operator runs on k3d, outright; in a hosted env,
// where it is bound `_on_cluster`, refused ONLY for the cluster the env has
// not declared — the one fix the author has to make there.
//
// The workload schema used to require at least one CRD of every operator, so
// the fresh declaration failed the render of EVERY env, dev included, with a
// schema error naming neither the operator nor the next step.
func TestFreshOperatorRendersUntilOnlyTheClusterIsMissing(t *testing.T) {
	root := scaffoldForRender(t, "shop", []string{"orders"}, "")
	op := config.ComponentConfig{Name: "reaper", Kind: config.ComponentKindOperator, Group: "shop.io", Version: "v1alpha1"}
	workloads := filepath.Join(root, codegen.WorkloadsKCLRelPath)
	raw, err := os.ReadFile(workloads)
	if err != nil {
		t.Fatal(err)
	}
	stanza := codegen.WorkloadStanza("github.com/acme/shop", "shop", op)
	if !strings.Contains(stanza, "crds = []") {
		t.Fatalf("precondition: a fresh operator's stanza is not the CRD-less one:\n%s", stanza)
	}
	if err := os.WriteFile(workloads, []byte(string(raw)+"\n"+stanza), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, env := range []string{"dev", "staging", "prod"} {
		res, err := codegen.AppendEnvBinding(root, "shop", env, codegen.WorkloadKindOperator, op.Name)
		if err != nil || !res.Applied {
			t.Fatalf("bind reaper in %s: %+v, %v", env, res, err)
		}
	}

	dev := renderEnvOutput(t, root, "dev")
	var devOp bool
	for _, w := range dev["workloads"].([]any) {
		devOp = devOp || w.(map[string]any)["name"] == "reaper"
	}
	if !devOp {
		t.Errorf("dev renders no reaper workload: %#v", dev["workloads"])
	}

	for _, env := range []string{"staging", "prod"} {
		_, err := kclrender.Run(root, filepath.Join(root, "deploy/kcl", env), []string{"env=" + env})
		if err == nil {
			t.Fatalf("%s: an operator bound to an undeclared cluster rendered", env)
		}
		for _, want := range []string{"workload 'reaper' is bound to _on_cluster", "declares no `_cluster`", "the hosted platform refuses operators"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: the refusal does not say %q:\n%v", env, want, err)
			}
		}
		if strings.Contains(err.Error(), "CRD") {
			t.Errorf("%s: refused over the operator's CRDs, not the missing cluster:\n%v", env, err)
		}
	}
}

// runtimeType reads an entity's runtime: a workload or frontend carries the
// runtime object, a database entity just its type.
func runtimeType(entity map[string]any) string {
	if s, ok := entity["runtime"].(string); ok {
		return s
	}
	rt, _ := entity["runtime"].(map[string]any)
	s, _ := rt["type"].(string)
	return s
}

// TestScaffoldedClusterBinderRefusesUntilDeclared pins the behavior for a
// workload bound to the cluster binder of an env that declares no cluster —
// which is what `forge scaffold` writes for a kind the hosted platform
// refuses (a cron, an operator). It must fail the RENDER, naming the
// workload and the fix, rather than render an env that deploys nowhere.
// Declaring `_cluster` is the whole fix: the env then renders, mixed.
func TestScaffoldedClusterBinderRefusesUntilDeclared(t *testing.T) {
	root := scaffoldForRender(t, "shop", []string{"orders"}, "")
	path := filepath.Join(root, "deploy/kcl/prod/main.k")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The API — `server`, running orders — is the workload to rebind.
	src := strings.Replace(string(raw), "    _hosted(_api)\n", "    _on_cluster(_api)\n", 1)
	if src == string(raw) {
		t.Fatalf("precondition: prod/main.k does not bind _api `_hosted`:\n%s", raw)
	}
	if err := os.WriteFile(path, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = kclrender.Run(root, filepath.Join(root, "deploy/kcl/prod"), []string{"env=prod"})
	if err == nil {
		t.Fatal("an `_on_cluster` binding with no `_cluster` declared rendered")
	}
	for _, want := range []string{"workload 'api' is bound to _on_cluster", "declares no `_cluster`"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("render error does not say %q:\n%v", want, err)
		}
	}

	declared := strings.Replace(src, "\n_cluster = None\n",
		"\n_cluster = forge.ClusterTarget {cluster = \"gke_acme_prod\", connected_cluster = \"shop-prod\", namespace = \"shop-prod\", platform = \"amd64\"}\n", 1)
	if err := os.WriteFile(path, []byte(declared), 0o644); err != nil {
		t.Fatal(err)
	}
	out := renderEnvOutput(t, root, "prod")
	for _, w := range out["workloads"].([]any) {
		wm := w.(map[string]any)
		want := "hosted"
		if wm["name"] == "api" {
			want = "cluster"
		}
		if runtimeType(wm) != want {
			t.Errorf("workload %v runs on %q, want %s", wm["name"], runtimeType(wm), want)
		}
	}
}
