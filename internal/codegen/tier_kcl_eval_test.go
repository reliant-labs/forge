package codegen

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/kcltest"
	"github.com/reliant-labs/forge/pkg/deploy/v1alpha1"
)

// runTierKCL evaluates main.k in a throwaway project depending on forge's KCL
// module, and returns kcl's JSON output — or, on failure, its error text (the
// negative cases assert on that).
//
// kcltest.Run, not exec+CombinedOutput: under a parallel `go test ./...` kcl
// prints "waiting for package-cache lock..." on STDOUT ahead of the JSON, and
// the round-trip decode then failed with `invalid character 'w'` against a
// document that was correct. See internal/kcltest.
func runTierKCL(t *testing.T, main string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("kcl"); err != nil {
		t.Skip("kcl not on PATH")
	}
	dir := t.TempDir()
	mod := "[package]\nname = \"tiercheck\"\nedition = \"v0.11.0\"\nversion = \"0.0.1\"\n\n[dependencies]\nforge = { path = \"" + filepath.Join(forgeRepoRoot(t), "kcl") + "\" }\n"
	for f, c := range map[string]string{"kcl.mod": mod, "main.k": "import forge.tiers\n\n" + main} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, err := kcltest.Run(t.Context(), dir, "run", "main.k", "--format", "json")
	if err != nil {
		return string(out) + "\n" + err.Error(), err
	}
	return string(out), nil
}

// TestTierKCLRoundTripsIntoGo evaluates declarations written against the
// GENERATED schemas, projects them with the generated _json lambdas, decodes
// the result into the Go spec types, and runs Validate(). This is the "one
// declaration" claim, end to end: what an author writes in KCL IS a valid Go
// spec, and therefore a valid CR spec.
func TestTierKCLRoundTripsIntoGo(t *testing.T) {
	if testing.Short() {
		t.Skip("runs kcl; full mode only")
	}
	out, err := runTierKCL(t, `
_sb = tiers.SimpleBackend {
    image = "ghcr.io/acme/api:v1.4.2"
    ports = [8080]
    network = "public"
    domains = ["api.acme.com"]
    storageGiB = 5
    healthCheck = tiers.HealthCheck { port = 8080, path = "/healthz" }
    env = [
        tiers.EnvVar { name = "LOG_LEVEL", value = "info" }
        tiers.EnvVar { name = "DATABASE_URL", databaseRef = tiers.DatabaseRef { name = "orders" } }
        tiers.EnvVar { name = "STRIPE_KEY", managedSecret = "STRIPE_KEY" }
        tiers.EnvVar { name = "PW", secretRef = tiers.SecretKeyRef { name = "s", key = "k" } }
    ]
}
_min = tiers.SimpleBackend { image = "ghcr.io/acme/w:v1", ports = [3000] }
_req = tiers.SimpleBackend { image = "ghcr.io/acme/w:v1", ports = [3000], resources = tiers.Resources { cpuRequestMillicores = 500, memoryRequestBytes = 2147483648 } }
_ss = tiers.StaticSite { bucket = "gs://acme", keepReleases = 0, cdn = tiers.StaticSiteCDN { urlMap = "lb" } }
_db = tiers.ManagedDatabase {}

sb = tiers.simple_backend_json(_sb)
min = tiers.simple_backend_json(_min)
req = tiers.simple_backend_json(_req)
ss = tiers.static_site_json(_ss)
db = tiers.managed_database_json(_db)
`)
	if err != nil {
		t.Fatalf("kcl: %v\n%s", err, out)
	}
	var doc struct {
		SB  v1alpha1.SimpleBackendSpec   `json:"sb"`
		Min v1alpha1.SimpleBackendSpec   `json:"min"`
		Req v1alpha1.SimpleBackendSpec   `json:"req"`
		SS  v1alpha1.StaticSiteSpec      `json:"ss"`
		DB  v1alpha1.ManagedDatabaseSpec `json:"db"`
	}
	dec := json.NewDecoder(strings.NewReader(out))
	dec.DisallowUnknownFields() // a key the Go type does not know is a generator bug
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("decode KCL output into the Go specs: %v\n%s", err, out)
	}
	for name, v := range map[string]interface{ Validate() error }{"sb": doc.SB, "min": doc.Min, "req": doc.Req, "ss": doc.SS, "db": doc.DB} {
		if err := v.Validate(); err != nil {
			t.Errorf("%s: KCL-authored spec fails Go Validate: %v", name, err)
		}
	}
	// The working defaults reached Go: an omitted resources block is the
	// entry shape, not zero, and the database defaults to RETAIN.
	if r := doc.Min.Resources; r.CPURequestMillicores != 250 || r.MemoryRequestBytes != 1<<30 {
		t.Errorf("omitted resources = %+v, want the 250m/1Gi default shape", r)
	}
	// Request-only: KCL must leave the limits UNSET (a static default cannot
	// say "equals the request"), so Go's WithDefaults resolves them to the
	// requests at render time.
	if r := doc.Req.Resources; r.CPULimitMillicores != 0 || r.MemoryLimitBytes != 0 {
		t.Errorf("request-only resources = %+v, want the limits left unset for Go to default", r)
	}
	if r := doc.Req.Resources.WithDefaults(); r.CPULimitMillicores != 500 || r.MemoryLimitBytes != 2<<30 {
		t.Errorf("defaulted request-only resources = %+v, want limits equal to the requests", r)
	}
	if doc.Min.Network != v1alpha1.NetworkPrivate {
		t.Errorf("network default = %q, want private", doc.Min.Network)
	}
	if doc.DB.DeletionPolicy != v1alpha1.DeletionPolicyRetain || doc.DB.Instances != 1 || doc.DB.StorageGiB != 10 {
		t.Errorf("database defaults = %+v", doc.DB)
	}
	// keepReleases = 0 must survive as an explicit 0 ("retain everything"),
	// not collapse to unset (which means the default of 10).
	if doc.SS.KeepReleases == nil || *doc.SS.KeepReleases != 0 {
		t.Errorf("keepReleases = %v, want an explicit 0", doc.SS.KeepReleases)
	}
	if doc.SS.CDN == nil || doc.SS.CDN.Invalidate != v1alpha1.InvalidateEntrypoints {
		t.Errorf("cdn = %+v, want invalidate defaulted to entrypoints", doc.SS.CDN)
	}
	if doc.SB.Env[1].DatabaseRef == nil || doc.SB.Env[1].DatabaseRef.EffectiveKey() != v1alpha1.DatabaseKeyURI {
		t.Errorf("databaseRef = %+v", doc.SB.Env[1].DatabaseRef)
	}
	// Unset optionals are ABSENT, not null. A null would clear a CR field.
	if strings.Contains(out, "null") {
		t.Errorf("projection emitted a null:\n%s", out)
	}
}

// TestTierKCLRejects: each constraint a declaration can break at author time
// fails `kcl run` with a message naming the field, and the schemas are closed.
func TestTierKCLRejects(t *testing.T) {
	if testing.Short() {
		t.Skip("runs kcl; full mode only")
	}
	cases := map[string]struct{ decl, want string }{
		"closed schema: replicas":       {`x = tiers.SimpleBackend { image = "ghcr.io/a/b:v1", replicas = 3 }`, "replicas"},
		"closed schema: cluster":        {`x = tiers.SimpleBackend { image = "ghcr.io/a/b:v1", cluster = "c" }`, "cluster"},
		"closed schema: config_map_ref": {`x = tiers.EnvVar { name = "A", config_map_ref = "c" }`, "config_map_ref"},
		"closed schema: org":            {`x = tiers.ManagedDatabase { orgId = "o" }`, "orgId"},
		"network enum":                  {`x = tiers.SimpleBackend { image = "ghcr.io/a/b:v1", network = "internet" }`, "SimpleBackend.network must be one of"},
		"port range":                    {`x = tiers.SimpleBackend { image = "ghcr.io/a/b:v1", ports = [70000] }`, "SimpleBackend.ports[] must be at most 65535"},
		"env name pattern":              {`x = tiers.EnvVar { name = "1BAD" }`, "EnvVar.name must match"},
		"managed secret no path":        {`x = tiers.EnvVar { name = "A", managedSecret = "other/secret" }`, "EnvVar.managedSecret must match"},
		"db key enum":                   {`x = tiers.DatabaseRef { name = "db", key = "dsn" }`, "DatabaseRef.key must be one of"},
		"digest not tag":                {`x = tiers.StaticSite { liveDigest = "latest" }`, "StaticSite.liveDigest must match"},
		"invalidate enum":               {`x = tiers.StaticSiteCDN { urlMap = "m", invalidate = "some" }`, "StaticSiteCDN.invalidate must be one of"},
		"instances ceiling":             {`x = tiers.ManagedDatabase { instances = 50 }`, "ManagedDatabase.instances must be at most 3"},
		"deletion policy enum":          {`x = tiers.ManagedDatabase { deletionPolicy = "purge" }`, "ManagedDatabase.deletionPolicy must be one of"},
		"probe port required":           {`x = tiers.HealthCheck { path = "/h" }`, "port"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			out, err := runTierKCL(t, c.decl)
			if err == nil {
				t.Fatalf("kcl accepted an invalid declaration:\n%s", out)
			}
			if !strings.Contains(out, c.want) {
				t.Fatalf("error does not mention %q:\n%s", c.want, out)
			}
		})
	}
}
