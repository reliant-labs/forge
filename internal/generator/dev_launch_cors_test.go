package generator

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	yaml "gopkg.in/yaml.v3"

	"github.com/reliant-labs/forge/pkg/serverkit"
)

// A freshly scaffolded frontend must reach its freshly scaffolded backend in a
// browser with zero steps. Nothing else in the scaffold can deliver that:
// CORS_ORIGINS has no default and the frontend's dev port is not knowable to
// the server (forge hands a portless frontend a kernel-assigned one), so an
// allow-list cannot be pre-seeded. The ONLY switch is the runtime environment
// — serverkit.Config.CORSEnabled turns the layer on for `development` with an
// empty origin list, and the generated serve shim then selects the
// origin-reflecting dev policy.
//
// That switch has now been landed twice and lost once. The first fix seeded
// `cors-origins: "*"` into config-dev.yaml.tmpl; the KCL config migration
// deleted that template, taking the seed with it, and nothing went red. These
// tests exist so the next deletion cannot be silent: they scaffold a real
// project and evaluate the resulting posture through serverkit's own
// predicate, so they fail if a launcher stops setting the environment, if a
// launcher artifact disappears, or if serverkit's notion of development moves
// out from under the scaffold.

// devLaunchSite is one place in a scaffolded project that starts the app's
// server on a developer's machine, and the environment it hands the process.
type devLaunchSite struct {
	artifact string // project-relative path, for failure messages
	site     string // which launch site inside that artifact
	env      string // the ENVIRONMENT value the process will see ("" = unset)
}

// TestScaffold_EveryDevLauncherServesPermissiveCORS is the gate. It scaffolds
// a project, collects every dev launch site, and asserts each one resolves to
// a permissive CORS posture — judged by running serverkit's real gate, not by
// matching a string the scaffold happens to contain today.
func TestScaffold_EveryDevLauncherServesPermissiveCORS(t *testing.T) {
	root := scaffoldForDevLaunch(t, "corsapp")

	sites := collectDevLaunchSites(t, root, "corsapp")
	if len(sites) == 0 {
		t.Fatal("no dev launch sites found in the scaffold — the discovery below matched nothing, " +
			"so this test would pass vacuously; fix the discovery before trusting a green run")
	}

	for _, s := range sites {
		cfg := serverkit.Config{Environment: s.env}
		// CORSOrigins deliberately left empty: that is the fresh-scaffold
		// state, and the whole point is that it needs no curation.
		if !cfg.CORSEnabled() {
			t.Errorf("%s (%s) launches the server with ENVIRONMENT=%q, which serverkit does NOT treat as CORS-enabled — "+
				"a frontend started next to it gets no Access-Control-Allow-Origin and every page fails with 'Failed to fetch'",
				s.artifact, s.site, s.env)
			continue
		}
		// CORSEnabled alone would also be satisfied by a named allow-list.
		// The dev branch of the generated shim keys off the environment being
		// EXACTLY serverkit.EnvDevelopment, and only that branch reflects an
		// arbitrary origin.
		if s.env != serverkit.EnvDevelopment {
			t.Errorf("%s (%s) launches the server with ENVIRONMENT=%q; the origin-reflecting dev policy is selected only on %q",
				s.artifact, s.site, s.env, serverkit.EnvDevelopment)
		}
	}
}

// TestScaffold_ConfigDefaultEnvironmentStaysClosed pins the other half of the
// contract, and the reason the launchers above are load-bearing: the config
// field's DEFAULT must remain a deployed posture. Flipping it to development
// would make the test above pass everywhere and open every deployment that
// forgets to set ENVIRONMENT — a fail-open default is not a fix.
func TestScaffold_ConfigDefaultEnvironmentStaysClosed(t *testing.T) {
	root := scaffoldForDevLaunch(t, "closedapp")

	proto := readFile(t, filepath.Join(root, "proto", "config", "v1", "config.proto"))
	envField := fieldBlock(t, proto, "string environment")
	def := quotedOptionValue(t, envField, "default_value")

	if (serverkit.Config{Environment: def}).CORSEnabled() {
		t.Fatalf("the scaffolded config defaults ENVIRONMENT to %q, which serverkit treats as CORS-enabled — "+
			"a deployment that never sets ENVIRONMENT would serve a permissive edge", def)
	}
}

func scaffoldForDevLaunch(t *testing.T, name string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	if err := NewProjectGenerator(name, root, "example.com/"+name).Generate(); err != nil {
		t.Fatalf("Generate(): %v", err)
	}
	return root
}

// collectDevLaunchSites reads every artifact the scaffold ships for starting
// the server locally. Each reader REQUIRES its artifact to exist and to
// contain at least one launch site: an artifact that is deleted, renamed, or
// restructured fails loudly here rather than quietly contributing zero sites.
func collectDevLaunchSites(t *testing.T, root, name string) []devLaunchSite {
	t.Helper()
	var sites []devLaunchSite
	sites = append(sites, airLaunchSites(t, root, ".air.toml", name)...)
	sites = append(sites, airLaunchSites(t, root, ".air-debug.toml", name)...)
	sites = append(sites, composeLaunchSites(t, root)...)
	sites = append(sites, vscodeLaunchSites(t, root, name)...)
	return sites
}

// airLaunchSites parses the `full_bin` air runs. Air executes it through a
// shell, so leading KEY=VALUE tokens are the process environment.
func airLaunchSites(t *testing.T, root, file, name string) []devLaunchSite {
	t.Helper()
	content := readFile(t, filepath.Join(root, file))

	var fullBin string
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "full_bin"); ok {
			fullBin = strings.Trim(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(rest), "=")), `"`)
			break
		}
	}
	if fullBin == "" {
		t.Fatalf("%s declares no full_bin — air has no launch site to inspect", file)
	}
	if !strings.Contains(fullBin, name) {
		t.Fatalf("%s full_bin %q does not run the project binary %q", file, fullBin, name)
	}

	env := ""
	for _, tok := range strings.Fields(fullBin) {
		key, value, ok := strings.Cut(tok, "=")
		if !ok || strings.HasPrefix(key, "-") {
			break // first non-assignment token: the command starts here
		}
		if key == "ENVIRONMENT" {
			env = value
		}
	}
	return []devLaunchSite{{artifact: file, site: "full_bin", env: env}}
}

// composeLaunchSites reads every compose service that BUILDS this project (as
// opposed to pulling an image, like postgres) — those are the ones running the
// app's server.
func composeLaunchSites(t *testing.T, root string) []devLaunchSite {
	t.Helper()
	var compose struct {
		Services map[string]struct {
			Build       any               `yaml:"build"`
			Environment map[string]string `yaml:"environment"`
		} `yaml:"services"`
	}
	if err := yaml.Unmarshal([]byte(readFile(t, filepath.Join(root, "docker-compose.yml"))), &compose); err != nil {
		t.Fatalf("docker-compose.yml: %v", err)
	}

	var sites []devLaunchSite
	for svcName, svc := range compose.Services {
		if svc.Build == nil {
			continue // an infra image (postgres, lgtm), not this app
		}
		sites = append(sites, devLaunchSite{
			artifact: "docker-compose.yml",
			site:     "service " + svcName,
			env:      svc.Environment["ENVIRONMENT"],
		})
	}
	if len(sites) == 0 {
		t.Fatal("docker-compose.yml builds no service from this project — the app is not launched anywhere in it")
	}
	return sites
}

// vscodeLaunchSites reads every debug configuration that launches the app's
// own command tree.
func vscodeLaunchSites(t *testing.T, root, name string) []devLaunchSite {
	t.Helper()
	var launch struct {
		Configurations []struct {
			Name    string            `json:"name"`
			Program string            `json:"program"`
			Args    []string          `json:"args"`
			Env     map[string]string `json:"env"`
		} `json:"configurations"`
	}
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(root, ".vscode", "launch.json"))), &launch); err != nil {
		t.Fatalf(".vscode/launch.json: %v", err)
	}

	var sites []devLaunchSite
	for _, c := range launch.Configurations {
		if !strings.Contains(c.Program, filepath.Join("cmd", name)) {
			continue // attach-to-delve, debug-current-test, …
		}
		sites = append(sites, devLaunchSite{
			artifact: ".vscode/launch.json",
			site:     "configuration " + c.Name,
			env:      c.Env["ENVIRONMENT"],
		})
	}
	if len(sites) == 0 {
		t.Fatalf(".vscode/launch.json has no configuration launching cmd/%s — the app is not launched anywhere in it", name)
	}
	return sites
}

// fieldBlock returns the option block of the proto field whose declaration
// starts with decl, failing loudly when the field is gone.
func fieldBlock(t *testing.T, proto, decl string) string {
	t.Helper()
	i := strings.Index(proto, decl)
	if i < 0 {
		t.Fatalf("config.proto declares no %q field", decl)
	}
	rest := proto[i:]
	j := strings.Index(rest, "}];")
	if j < 0 {
		t.Fatalf("config.proto field %q has no option block", decl)
	}
	return rest[:j]
}

// quotedOptionValue pulls `key: "value"` out of a proto option block.
func quotedOptionValue(t *testing.T, block, key string) string {
	t.Helper()
	i := strings.Index(block, key+":")
	if i < 0 {
		t.Fatalf("option block declares no %q:\n%s", key, block)
	}
	rest := block[i+len(key)+1:]
	start := strings.Index(rest, `"`)
	if start < 0 {
		t.Fatalf("option %q has no quoted value:\n%s", key, block)
	}
	rest = rest[start+1:]
	end := strings.Index(rest, `"`)
	if end < 0 {
		t.Fatalf("option %q has an unterminated value:\n%s", key, block)
	}
	return rest[:end]
}
