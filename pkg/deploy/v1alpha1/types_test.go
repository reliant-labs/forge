package v1alpha1

import (
	"encoding/json"
	"os"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func ptr[T any](v T) *T { return &v }

const digestA = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const digestB = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// fullBackend sets EVERY field, so a round-trip that drops one is visible.
func fullBackend() *SimpleBackend {
	return &SimpleBackend{
		TypeMeta:   metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: "SimpleBackend"},
		ObjectMeta: metav1.ObjectMeta{Name: "api", Namespace: "acme-prod", Labels: map[string]string{LabelOrgID: "org_1"}},
		Spec: SimpleBackendSpec{
			Image:   "ghcr.io/acme/api:v1.4.2",
			Ports:   []int32{8080, 9090},
			Network: NetworkPublic,
			Domains: []string{"api.acme.com"},
			Env: []EnvVar{
				{Name: "LOG_LEVEL", Value: "info"},
				{Name: "DB_PASSWORD", SecretRef: &SecretKeyRef{Name: "db", Key: "password"}},
				{Name: "STRIPE_KEY", ManagedSecret: "STRIPE_KEY"},
				{Name: "DATABASE_URL", DatabaseRef: &DatabaseRef{Name: "orders", Key: DatabaseKeyURI}},
			},
			Resources:   Resources{CPURequestMillicores: 500, CPULimitMillicores: 1000, MemoryRequestBytes: 2 << 30, MemoryLimitBytes: 2 << 30},
			HealthCheck: &HealthCheck{Port: 8080, Path: "/healthz", InitialDelaySeconds: 7, PeriodSeconds: 10, TimeoutSeconds: 3, FailureThreshold: 3},
			StorageGiB:  20,
		},
		Status: SimpleBackendStatus{
			WorkloadStatus: WorkloadStatus{Phase: PhaseReady, ObservedGeneration: 3, Hostname: "lively-ferret.apps.example", URL: "https://lively-ferret.apps.example", Message: "ok",
				Conditions: []metav1.Condition{{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Ready", LastTransitionTime: metav1.Unix(1700000000, 0)}}},
			ServiceName: "api", WorkloadName: "api", ReadyReplicas: 1, ObservedImage: "ghcr.io/acme/api:v1.4.2", LastReadyAt: ptr(metav1.Unix(1700000000, 0)),
		},
	}
}

func fullSite() *StaticSite {
	return &StaticSite{
		TypeMeta:   metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: "StaticSite"},
		ObjectMeta: metav1.ObjectMeta{Name: "web"},
		Spec: StaticSiteSpec{
			Bucket: "gs://acme-web", BasePath: "/admin", LiveDigest: digestA, PreviousDigest: digestB,
			RetainedDigests: []string{digestA}, KeepReleases: ptr(int32(0)), Entrypoints: []string{"/index.html"},
			CDN:     &StaticSiteCDN{URLMap: "acme-lb", Invalidate: InvalidateAll, ExtraInvalidatePaths: []string{"/sw.js"}},
			Domains: []string{"www.acme.com"},
		},
		Status: StaticSiteStatus{WorkloadStatus: WorkloadStatus{Phase: PhaseProgressing}, BucketPrefix: "sites/web", LiveDigest: digestB, PreviousDigest: digestA, ReleaseCount: 4, LastSyncedAt: ptr(metav1.Unix(1700000000, 0))},
	}
}

func fullDatabase() *ManagedDatabase {
	return &ManagedDatabase{
		TypeMeta:   metav1.TypeMeta{APIVersion: GroupVersion.String(), Kind: "ManagedDatabase"},
		ObjectMeta: metav1.ObjectMeta{Name: "orders"},
		Spec:       ManagedDatabaseSpec{Engine: EnginePostgres, Instances: 2, StorageGiB: 50, DeletionPolicy: DeletionPolicyDelete},
		Status: ManagedDatabaseStatus{Phase: PhaseLocked, ObservedGeneration: 2, ClusterName: "orders", DatabaseName: "orders", RoleName: "orders_app",
			SecretName: "orders-app", EngineVersion: "16.4", ReadyAt: ptr(metav1.Unix(1700000000, 0)), Message: "released"},
	}
}

// TestTierTypesRoundTripJSON: every kind survives encode→decode unchanged, and
// the encoding re-encodes byte-identically (no field lost to a tag mismatch).
func TestTierTypesRoundTripJSON(t *testing.T) {
	for name, obj := range map[string]runtime.Object{"SimpleBackend": fullBackend(), "StaticSite": fullSite(), "ManagedDatabase": fullDatabase()} {
		t.Run(name, func(t *testing.T) {
			b, err := json.Marshal(obj)
			if err != nil {
				t.Fatal(err)
			}
			fresh := reflect.New(reflect.TypeOf(obj).Elem()).Interface()
			if err := json.Unmarshal(b, fresh); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(obj, fresh) {
				t.Fatalf("round-trip changed the object:\nbefore %+v\nafter  %+v", obj, fresh)
			}
			b2, _ := json.Marshal(fresh)
			if string(b) != string(b2) {
				t.Fatalf("re-encoding differs:\n%s\n%s", b, b2)
			}
		})
	}
}

// TestSpecCarriesNoHostedIdentity pins the design rule that ownership is
// labels, never spec: an author-writable org id is the defect it prevents.
func TestSpecCarriesNoHostedIdentity(t *testing.T) {
	forbidden := regexp.MustCompile(`(?i)^(org|tena|environment|deployment|namespace|cluster|replicas)`)
	for _, typ := range []reflect.Type{reflect.TypeOf(SimpleBackendSpec{}), reflect.TypeOf(StaticSiteSpec{}), reflect.TypeOf(ManagedDatabaseSpec{})} {
		for i := 0; i < typ.NumField(); i++ {
			if f := typ.Field(i); forbidden.MatchString(f.Name) {
				t.Errorf("%s.%s: hosted identity, target coordinates and replicas are not spec", typ.Name(), f.Name)
			}
		}
	}
}

// TestSchemeRegistersAllKinds guards the registration trap: an unregistered
// kind decodes as "no kind registered" only at runtime.
func TestSchemeRegistersAllKinds(t *testing.T) {
	s := runtime.NewScheme()
	if err := AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"SimpleBackend", "SimpleBackendList", "StaticSite", "StaticSiteList", "ManagedDatabase", "ManagedDatabaseList"} {
		if !s.Recognizes(GroupVersion.WithKind(k)) {
			t.Errorf("%s not registered under %s", k, GroupVersion)
		}
	}
	if GroupVersion.Group != "forge.dev" {
		t.Errorf("group = %q, want forge.dev", GroupVersion.Group)
	}
}

// TestDeepCopyIsIndependent: mutating a copy's nested pointer/slice must not
// reach the original.
func TestDeepCopyIsIndependent(t *testing.T) {
	a := fullBackend()
	b := a.DeepCopy()
	b.Spec.Env[3].DatabaseRef.Name = "changed"
	b.Spec.Ports[0] = 1
	b.Spec.HealthCheck.Port = 1
	if a.Spec.Env[3].DatabaseRef.Name != "orders" || a.Spec.Ports[0] != 8080 || a.Spec.HealthCheck.Port != 8080 {
		t.Fatal("DeepCopy shares memory with the original")
	}
}

func mustFail(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil {
		t.Fatalf("want an error containing %q, got nil", want)
	}
	if !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not contain %q", err, want)
	}
}

func TestSimpleBackendValidate(t *testing.T) {
	if err := fullBackend().Spec.Validate(); err != nil {
		t.Fatalf("the full fixture must validate: %v", err)
	}
	minimal := SimpleBackendSpec{Image: "ghcr.io/acme/api@sha256:" + strings.Repeat("a", 64), Ports: []int32{8080}}
	if err := minimal.Validate(); err != nil {
		t.Fatalf("a minimal private backend with defaults must validate (working defaults): %v", err)
	}
	// A public backend no longer needs a domain: the hostname is allocated.
	pub := minimal
	pub.Network = NetworkPublic
	if err := pub.Validate(); err != nil {
		t.Fatalf("public with no domains must validate (hostname is allocated): %v", err)
	}

	cases := map[string]struct {
		mut  func(*SimpleBackendSpec)
		want string
	}{
		"bare image":        {func(s *SimpleBackendSpec) { s.Image = "api:v1" }, "registry host"},
		"unpinned":          {func(s *SimpleBackendSpec) { s.Image = "ghcr.io/acme/api" }, "pinned"},
		"port-in-host only": {func(s *SimpleBackendSpec) { s.Image = "localhost:5000/api" }, "pinned"},
		"bad network":       {func(s *SimpleBackendSpec) { s.Network = "internet" }, "network"},
		"private no ports":  {func(s *SimpleBackendSpec) { s.Ports = nil }, "at least one port"},
		"none with ports":   {func(s *SimpleBackendSpec) { s.Network = NetworkNone }, "no ports"},
		"domain on private": {func(s *SimpleBackendSpec) { s.Domains = []string{"a.example.com"} }, "only meaningful for network public"},
		"bad domain":        {func(s *SimpleBackendSpec) { s.Network = NetworkPublic; s.Domains = []string{"Not A Host"} }, "not a lowercase DNS hostname"},
		"two env channels":  {func(s *SimpleBackendSpec) { s.Env = []EnvVar{{Name: "X", Value: "1", ManagedSecret: "X"}} }, "more than one"},
		"dup env":           {func(s *SimpleBackendSpec) { s.Env = []EnvVar{{Name: "X"}, {Name: "X"}} }, "declared twice"},
		"traversal secret":  {func(s *SimpleBackendSpec) { s.Env = []EnvVar{{Name: "X", ManagedSecret: "other/secret"}} }, "bare logical name"},
		"db ref bad key": {func(s *SimpleBackendSpec) {
			s.Env = []EnvVar{{Name: "X", DatabaseRef: &DatabaseRef{Name: "db", Key: "dsn"}}}
		}, "databaseRef.key"},
		"limit below req": {func(s *SimpleBackendSpec) {
			s.Resources = Resources{CPURequestMillicores: 500, CPULimitMillicores: 250}
		}, "at least the request"},
		"negative resources": {func(s *SimpleBackendSpec) { s.Resources.MemoryRequestBytes = -1 }, "must not be negative"},
		"bad probe port":     {func(s *SimpleBackendSpec) { s.HealthCheck = &HealthCheck{Port: 0} }, "healthCheck.port"},
		"negative storage":   {func(s *SimpleBackendSpec) { s.StorageGiB = -1 }, "storageGiB"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			s := minimal
			c.mut(&s)
			mustFail(t, s.Validate(), c.want)
		})
	}

	// Collected, not first-error-only: one pass surfaces every mistake.
	bad := SimpleBackendSpec{Image: "api", Network: "x", StorageGiB: -1}
	if n := strings.Count(bad.Validate().Error(), "\n") + 1; n < 3 {
		t.Errorf("want every violation reported together, got %d: %v", n, bad.Validate())
	}
}

func TestStaticSiteValidateAndRetention(t *testing.T) {
	if err := fullSite().Spec.Validate(); err != nil {
		t.Fatal(err)
	}
	mustFail(t, StaticSiteSpec{LiveDigest: "latest"}.Validate(), "never a tag")
	mustFail(t, StaticSiteSpec{KeepReleases: ptr(int32(1))}.Validate(), "rollback needs")
	mustFail(t, StaticSiteSpec{CDN: &StaticSiteCDN{}}.Validate(), "urlMap")
	mustFail(t, StaticSiteSpec{CDN: &StaticSiteCDN{URLMap: "m", Invalidate: "some"}}.Validate(), "cdn.invalidate")
	mustFail(t, StaticSiteSpec{BasePath: "admin"}.Validate(), "basePath")

	for _, c := range []struct {
		keep *int32
		want int32
	}{{nil, DefaultKeepReleases}, {ptr(int32(0)), 0}, {ptr(int32(1)), MinKeepReleases}, {ptr(int32(5)), 5}} {
		if got := (StaticSiteSpec{KeepReleases: c.keep}).EffectiveKeepReleases(); got != c.want {
			t.Errorf("EffectiveKeepReleases(%v) = %d, want %d", c.keep, got, c.want)
		}
	}
	if (StaticSiteCDN{}).EffectiveInvalidate() != InvalidateEntrypoints {
		t.Error("an unset invalidate policy must mean entrypoints, never all")
	}
}

func TestManagedDatabaseValidateAndDefaults(t *testing.T) {
	d := ManagedDatabaseSpec{}.WithDefaults()
	if d.Engine != EnginePostgres || d.Instances != 1 || d.StorageGiB != 10 || d.DeletionPolicy != DeletionPolicyRetain {
		t.Fatalf("defaults = %+v; retain MUST be the deletion default", d)
	}
	if err := (ManagedDatabaseSpec{}).Validate(); err != nil {
		t.Fatalf("an empty spec must validate via defaults: %v", err)
	}
	// Refused, not clamped: a 50-instance declaration must not deploy as 3.
	mustFail(t, ManagedDatabaseSpec{Instances: 50}.Validate(), "instances 50")
	mustFail(t, ManagedDatabaseSpec{StorageGiB: 501}.Validate(), "storageGiB")
	mustFail(t, ManagedDatabaseSpec{Engine: "mysql"}.Validate(), "engine")
	mustFail(t, ManagedDatabaseSpec{DeletionPolicy: "purge"}.Validate(), "deletionPolicy")
	if err := ValidateDatabaseName("orders-db"); err != nil {
		t.Fatal(err)
	}
	mustFail(t, ValidateDatabaseName(strings.Repeat("a", 41)), "at most 40")
	mustFail(t, ValidateDatabaseName("Orders"), "RFC-1123")
}

// TestResourcesLimitDefaultsToRequest: "set a request, leave the limit" is the
// most common resources block anyone writes, and it must deploy. An unset
// limit is the (defaulted) request, never the static entry shape: a static
// 250m limit under a 500m request is a spec forge itself then refuses.
func TestResourcesLimitDefaultsToRequest(t *testing.T) {
	cases := map[string]struct{ in, want Resources }{
		"empty is the entry shape": {Resources{}, Resources{
			CPURequestMillicores: DefaultCPUMillicores, CPULimitMillicores: DefaultCPUMillicores,
			MemoryRequestBytes: DefaultMemoryBytes, MemoryLimitBytes: DefaultMemoryBytes,
		}},
		"cpu request only": {Resources{CPURequestMillicores: 500}, Resources{
			CPURequestMillicores: 500, CPULimitMillicores: 500,
			MemoryRequestBytes: DefaultMemoryBytes, MemoryLimitBytes: DefaultMemoryBytes,
		}},
		"memory request only": {Resources{MemoryRequestBytes: 2 << 30}, Resources{
			CPURequestMillicores: DefaultCPUMillicores, CPULimitMillicores: DefaultCPUMillicores,
			MemoryRequestBytes: 2 << 30, MemoryLimitBytes: 2 << 30,
		}},
		"explicit limit kept": {Resources{CPURequestMillicores: 500, CPULimitMillicores: 2000}, Resources{
			CPURequestMillicores: 500, CPULimitMillicores: 2000,
			MemoryRequestBytes: DefaultMemoryBytes, MemoryLimitBytes: DefaultMemoryBytes,
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if got := c.in.WithDefaults(); got != c.want {
				t.Errorf("WithDefaults(%+v) = %+v, want %+v", c.in, got, c.want)
			}
			if err := c.in.Validate(); err != nil {
				t.Errorf("Validate(%+v): %v", c.in, err)
			}
		})
	}
	// A limit that is SET below its request is still the authoring mistake
	// Kubernetes rejects at admission.
	mustFail(t, Resources{CPURequestMillicores: 500, CPULimitMillicores: 250}.Validate(), "cpuLimitMillicores (250) must be at least the request (500)")
	mustFail(t, Resources{MemoryRequestBytes: 2 << 30, MemoryLimitBytes: 1 << 30}.Validate(), "memoryLimitBytes")
}

// TestDefaultMarkersMatchGoDefaults: the +kubebuilder:default markers (what the
// API server and the generated KCL apply) must equal the Go defaults (what
// WithDefaults and Render apply), or the two destinations run different shapes.
//
// The resource LIMITS must carry NO marker. Their Go default is "equal to
// the (defaulted) request", which is not a static value. A static marker
// would make the API server stamp 250m under a 500m request, and the
// operator's Validate would then refuse the CR. The generated KCL would
// inherit the same wrong default. So for these fields the only correct
// marker is none.
func TestDefaultMarkersMatchGoDefaults(t *testing.T) {
	mustHaveNoMarker := []string{"CPULimitMillicores", "MemoryLimitBytes"}
	want := map[string]string{
		"CPURequestMillicores": strconv.FormatInt(DefaultCPUMillicores, 10),
		"MemoryRequestBytes":   strconv.FormatInt(DefaultMemoryBytes, 10),
		"InitialDelaySeconds":  strconv.Itoa(int(DefaultProbeInitialDelaySeconds)),
		"PeriodSeconds":        strconv.Itoa(int(DefaultProbePeriodSeconds)),
		"TimeoutSeconds":       strconv.Itoa(int(DefaultProbeTimeoutSeconds)),
		"FailureThreshold":     strconv.Itoa(int(DefaultProbeFailureThreshold)),
		"Instances":            strconv.Itoa(int(DefaultDatabaseInstances)),
		"StorageGiB":           strconv.Itoa(int(DefaultDatabaseStorageGiB)),
		"DeletionPolicy":       string(DeletionPolicyRetain),
		"Engine":               string(EnginePostgres),
		"Network":              string(NetworkPrivate),
		"Invalidate":           string(InvalidateEntrypoints),
	}
	// A default marker applies to the next non-comment line, which is the field.
	marker := regexp.MustCompile(`^\s*// \+kubebuilder:default=(\S+)$`)
	fieldLine := regexp.MustCompile(`^\s*(\w+)\s`)
	found := map[string]string{}
	for _, f := range []string{"common.go", "simplebackend_types.go", "staticsite_types.go", "manageddatabase_types.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		pending := ""
		for _, line := range strings.Split(string(b), "\n") {
			if m := marker.FindStringSubmatch(line); m != nil {
				pending = m[1]
				continue
			}
			if pending != "" && !strings.HasPrefix(strings.TrimSpace(line), "//") {
				if m := fieldLine.FindStringSubmatch(line); m != nil {
					found[m[1]] = pending
				}
				pending = ""
			}
		}
	}
	for _, field := range mustHaveNoMarker {
		if d, ok := found[field]; ok {
			t.Errorf("%s has +kubebuilder:default=%s; it must have none — its default is its request, which no static marker can express", field, d)
		}
	}
	for field, w := range want {
		if found[field] != w {
			t.Errorf("%s: marker default %q, Go default %q", field, found[field], w)
		}
	}
	for field := range found {
		if _, ok := want[field]; !ok {
			t.Errorf("%s has a default marker with no Go-default pin here — add it", field)
		}
	}
}
