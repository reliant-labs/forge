package cli

import (
	"strings"
	"testing"

	deployv1alpha1 "github.com/reliant-labs/forge/pkg/deploy/v1alpha1"
	"github.com/reliant-labs/forge/pkg/release"
	"github.com/reliant-labs/forge/pkg/telemetry"
)

func covDWorkloadWithSecret(name string, refs ...string) WorkloadEntity {
	w := WorkloadEntity{Name: name}
	for i, ref := range refs {
		w.Spec.Env = append(w.Spec.Env, deployv1alpha1.EnvVar{
			Name:      "VAR_" + string(rune('A'+i)),
			SecretRef: &deployv1alpha1.SecretKeyRef{Name: ref, Key: "k"},
		})
	}
	return w
}

func TestCovDShapeSecretsOf_GroupsReadersByAnnotatedSecretName(t *testing.T) {
	e := &KCLEntities{
		Workloads: []WorkloadEntity{
			covDWorkloadWithSecret("api", "db-url", "stripe"),
			covDWorkloadWithSecret("worker", "db-url"),
			{Name: "static"},
		},
		RequiredSecrets: []ExternalSecretEntity{{Name: "tls-ca"}, {Name: "stripe"}},
		SecretProvider:  &SecretProviderEntity{Type: "GSM"},
	}
	got := shapeSecretsOf(e)
	want := []release.ShapeSecret{
		{Name: "db-url", Provider: "gsm", DeclaredBy: []string{"api", "worker"}},
		{Name: "stripe", Provider: "gsm", DeclaredBy: []string{"api"}},
		{Name: "tls-ca", Provider: "gsm", DeclaredBy: []string{}},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Name != want[i].Name || got[i].Provider != want[i].Provider || strings.Join(got[i].DeclaredBy, ",") != strings.Join(want[i].DeclaredBy, ",") {
			t.Errorf("secret %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if shapeSecretsOf(nil) != nil {
		t.Error("a nil render must project no secrets")
	}
}

func TestCovDShapeProjections_NilAndEmptyAreNone(t *testing.T) {
	if shapeIdentityOf(nil) != nil || shapeIdentitiesOf(nil) != nil || shapeIdentitiesOf(&KCLEntities{}) != nil {
		t.Error("no entities must project no identity")
	}
	if shapeDomainsOf(nil) != nil || shapeStatefulWorkloadsOf(nil) != nil {
		t.Error("no entities must project no domains or stateful workloads")
	}
	if shapeSecretProviderOf(nil) != "" || shapeSecretProviderOf(&KCLEntities{}) != "" {
		t.Error("no provider declared must project an empty provider")
	}
	if shapeApplierOf(nil) != "" || shapeApplierOf(&KCLEntities{Applier: "something-else"}) != "" {
		t.Error("an undeclared applier must stay empty")
	}
	if shapeStatusPageOf(nil) != nil {
		t.Error("no entities must project no status page")
	}
	var nilEntities *KCLEntities
	if nilEntities.AllIdentities() != nil {
		t.Error("a nil entity set has no identities")
	}
}

func TestCovDShapeIdentity_PassThroughAndAll(t *testing.T) {
	one := &release.ShapeIdentity{Name: "login"}
	e := &KCLEntities{Identity: one}
	if shapeIdentityOf(e) != one {
		t.Error("shapeIdentityOf must pass the resolved identity through")
	}
	if got := e.AllIdentities(); len(got) != 1 || got[0].Name != "login" {
		t.Errorf("AllIdentities = %+v", got)
	}
	many := &KCLEntities{Identities: []release.ShapeIdentity{{Name: "a"}, {Name: "b"}}}
	if got := shapeIdentitiesOf(many); len(got) != 2 {
		t.Errorf("shapeIdentitiesOf = %+v", got)
	}
	if got := many.AllIdentities(); len(got) != 2 {
		t.Errorf("AllIdentities = %+v", got)
	}
}

func TestCovDShapeApplierOf_MapsDeclaredAppliers(t *testing.T) {
	if got := shapeApplierOf(&KCLEntities{Applier: applierForge}); got != release.ApplierForge {
		t.Errorf("forge applier = %q", got)
	}
	if got := shapeApplierOf(&KCLEntities{Applier: applierFlux}); got != release.ApplierFlux {
		t.Errorf("flux applier = %q", got)
	}
}

func TestCovDShapeDomainsOf_CollectsEveryHostOnce(t *testing.T) {
	e := &KCLEntities{
		Gateways: []GatewayEntity{{
			Host:      "app.example.com",
			Listeners: []GatewayListenerEntity{{Hostname: "api.example.com"}, {Hostname: "app.example.com"}, {}},
		}},
		HTTPRoutes: []HTTPRouteEntity{{Host: "www.example.com"}, {Host: "api.example.com"}},
		GRPCRoutes: []GRPCRouteEntity{{Host: "grpc.example.com"}},
	}
	got := strings.Join(shapeDomainsOf(e), ",")
	want := "app.example.com,api.example.com,www.example.com,grpc.example.com"
	if got != want {
		t.Errorf("domains = %s, want %s", got, want)
	}
}

func TestCovDShapeStatefulWorkloadsOf_NamesEachManagedDatabase(t *testing.T) {
	e := &KCLEntities{Databases: []DatabaseEntity{{Name: "main"}, {Name: "analytics"}}}
	if got := strings.Join(shapeStatefulWorkloadsOf(e), ","); got != "main,analytics" {
		t.Errorf("stateful workloads = %s", got)
	}
}

func TestCovDShortCommit_TruncatesToTwelve(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"abc", "abc"},
		{"0123456789ab", "0123456789ab"},
		{"0123456789abcdef", "0123456789ab"},
	} {
		if got := shortCommit(c.in); got != c.want {
			t.Errorf("shortCommit(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestCovDDescribeShapeProvenance(t *testing.T) {
	cases := []struct {
		name string
		p    release.Provenance
		want string
	}{
		{"no git", release.Provenance{ForgeVersion: "v1.2.3"}, "no git source (forge v1.2.3)"},
		{"no git no version", release.Provenance{}, "no git source (forge unknown)"},
		{"clean commit", release.Provenance{Commit: "0123456789abcdef", Tree: "t", ForgeVersion: "v1"}, "0123456789ab (forge v1)"},
		{"dirty branch unhashed", release.Provenance{Commit: "abc", Branch: "main", Dirty: true}, "abc on main DIRTY tree unhashed (forge unknown)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := describeShapeProvenance(c.p); got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestCovDWriteIdentitySummary_DescribesRuntimeSharingAndDeliveries(t *testing.T) {
	var b strings.Builder
	writeIdentitySummary(&b, &release.ShapeIdentity{
		Name: "login", Runtime: "hosted", ShareWith: "prod",
		Login:        release.ShapeIdentityLogin{Mode: release.IdentityLoginHosted},
		Applications: []release.ShapeIdentityApplication{{Name: "web", Kind: "spa", Workload: "frontend"}},
		Roles:        []release.ShapeIdentityRole{{Key: "admin"}, {Key: "viewer"}},
		Consumers:    []release.ShapeIdentityConsumer{{Workload: "api"}, {Workload: "worker"}},
		Deliveries:   []release.ShapeIdentityDelivery{{Kind: "identity", Name: "id", Namespace: "ns", Cluster: "c1", ConnectedCluster: "cc"}},
	})
	out := b.String()
	for _, want := range []string{
		"identity:  login (hosted, sharing prod's users, " + release.IdentityLoginHosted + " sign-in, public)",
		"application web [spa] brokered by frontend",
		"roles: admin, viewer",
		"consumed by: api, worker",
		"delivers ns/id [identity] on c1 via cc",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}

	b.Reset()
	writeIdentitySummary(&b, &release.ShapeIdentity{Name: "i", Runtime: "host", Domain: "login.acme.com", Login: release.ShapeIdentityLogin{Mode: "brokered"}})
	if !strings.Contains(b.String(), "(host, issuer on login.acme.com, brokered sign-in, private)") {
		t.Errorf("custom-domain private identity summary: %s", b.String())
	}
}

func TestCovDWriteShapeSummary_RendersEverySection(t *testing.T) {
	cpu := 1
	doc := envShapeDoc{
		Project: "",
		Env:     "prod",
		Kind:    "persistent",
		Shape: release.Shape{
			Clusters:  []string{"c1", "c2"},
			Workloads: []release.ShapeWorkload{{Name: "api", Runtime: "cluster", Cluster: "c1", Replicas: &cpu}, {Name: "site", Runtime: "hosted"}},
			Secrets: []release.ShapeSecret{
				{Name: "db", Provider: "gsm", DeclaredBy: []string{"api", "worker"}},
				{Name: "plain"},
			},
			Applier:             release.ApplierForge,
			ControlPlaneSecrets: []release.ShapeControlPlaneSecret{{Name: "tok", Namespace: "ns", Kind: "telemetry_token", Cluster: "c1", ConnectedCluster: "cc"}},
			Identity:            &release.ShapeIdentity{Name: "login", Runtime: "hosted", Login: release.ShapeIdentityLogin{Mode: "brokered"}},
			ExternalDatabases:   []release.ShapeExternalDatabase{{Name: "ext", Database: "d", Username: "u", Service: "pg"}},
			Telemetry: []release.ShapeTelemetry{{Target: telemetry.Target{
				ServiceName: "svc", Source: "src", Metrics: []telemetry.MetricsScrape{{}},
			}}},
			Alerts: []release.ShapeAlert{
				{Alert: "a", Source: release.AlertSourceUser},
				{Alert: "b", Source: release.AlertSourceDefault},
				{Alert: "c", Source: release.AlertSourceDefault},
			},
			AlertRoutes: []release.ShapeAlertRoute{{}},
			Domains:     []string{"a.com", "b.com"},
			Buckets: []release.ShapeBucket{
				{Name: "assets", RuntimeKind: "hosted", Bucket: "bkt", PublicRead: true},
				{Name: "private", RuntimeKind: "host"},
			},
			Changes: map[string]release.ShapeChange{"flag": {Type: release.ChangeTypeBool, Default: "off"}},
			Objects: make([]release.ShapeObject, 3),
		},
		Provenance:    release.Provenance{Commit: "0123456789abcdef", Tree: "tree", Branch: "main", ForgeVersion: "v1"},
		changeOrigins: map[string]string{"flag": "pkg@v1"},
		entities: &KCLEntities{FluxDelivery: &FluxDeliveryEntity{
			Cluster: "c1", Namespace: "flux-ns", ResourceSet: "rs", Chart: "platform",
			Signing: FluxDeliveryControl{Requested: "enforce", Effective: "audit"},
		}},
	}
	var b strings.Builder
	writeShapeSummary(&b, doc)
	out := b.String()
	for _, want := range []string{
		"env prod (project (unnamed)): persistent",
		"clusters:  c1, c2",
		"workloads: 2",
		"api (cluster on c1)",
		"site (hosted)",
		"secrets:   2 (names only",
		"db [gsm] ← api, worker",
		"applied by: " + release.ApplierForge,
		"delivered by: the in-cluster Flux on c1 (namespace flux-ns, ResourceSet rs; bootstrap: --target platform)",
		"signing enforce -> audit",
		"delivered by the control plane: 1",
		"ns/tok [telemetry_token] on c1 via cc",
		"identity:  login",
		"external databases: 1",
		"ext: d as u via (no namespace)/pg on (no cluster)",
		"telemetry: 1 hosted target(s)",
		"svc (src): metrics",
		"alerts:    3 (1 yours, 2 generated defaults), 1 route(s)",
		"domains:   a.com, b.com",
		"assets [hosted] → bkt (public read)",
		"private [host]",
		"changes:   1 declared in " + "changes.yaml" + " and the pinned sources",
		"flag [bool]: 0 variant(s), default off (from pkg@v1)",
		"objects:   3",
		"rendered from: 0123456789ab on main (forge v1)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "plain [") {
		t.Errorf("a secret with no provider must not print an empty provider tag:\n%s", out)
	}
}

func TestCovDEnvShapeDoc_ReconciledUnits(t *testing.T) {
	d := envShapeDoc{Shape: release.Shape{Units: []release.ShapeUnit{
		{Name: "hub-a", Applier: release.UnitApplierHub},
		{Name: "spoke", Applier: "other"},
		{Name: "hub-b", Applier: release.UnitApplierHub},
	}}}
	if got := strings.Join(d.reconciledUnits(), ","); got != "hub-a,hub-b" {
		t.Errorf("reconciledUnits = %q", got)
	}
	d.Lifecycle = "local"
	if d.reconciledUnits() != nil {
		t.Error("a lifecycle-declared env reconciles no units")
	}
	d.Lifecycle = ""
	d.Shape.Applier = release.ApplierForge
	if d.reconciledUnits() != nil {
		t.Error("an env with a declared applier reconciles no units")
	}
}
