package deploy

import (
	"encoding/json"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/reliant-labs/forge/pkg/deploy/v1alpha1"
)

var ctx = Context{Namespace: "acme-prod", PartOf: "proj"}

const testImage = "ghcr.io/acme/app:v1"

func wl(name string, spec v1alpha1.WorkloadSpec) v1alpha1.Workload {
	if spec.Image == "" {
		spec.Image = testImage
	}
	w := v1alpha1.Workload{Spec: spec}
	w.Name = name
	return w
}

func svc(name string, ports ...v1alpha1.Port) v1alpha1.Workload {
	return wl(name, v1alpha1.WorkloadSpec{Kind: v1alpha1.KindService, Ports: ports})
}

func httpPort(expose bool) v1alpha1.Port {
	return v1alpha1.Port{Name: "http", Port: 8080, Expose: expose}
}

func render(t *testing.T, p v1alpha1.Profile, ws ...v1alpha1.Workload) []*unstructured.Unstructured {
	t.Helper()
	objs, err := RenderWorkloads(ws, p, ctx)
	if err != nil {
		t.Fatalf("RenderWorkloads: %v", err)
	}
	return objs
}

// objects keys every rendered object by kind/name, as decoded JSON.
func objects(t *testing.T, objs []*unstructured.Unstructured) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, o := range objs {
		b, err := json.Marshal(o.Object)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		out[o.GetKind()+"/"+o.GetName()] = m
	}
	return out
}

func keysOf(m map[string]map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func get(m any, path ...any) any {
	for _, p := range path {
		switch k := p.(type) {
		case string:
			mm, _ := m.(map[string]any)
			m = mm[k]
		case int:
			s, _ := m.([]any)
			if k >= len(s) {
				return nil
			}
			m = s[k]
		}
	}
	return m
}

func podOf(o map[string]any) any {
	if o["kind"] == "CronJob" {
		return get(o, "spec", "jobTemplate", "spec", "template", "spec")
	}
	return get(o, "spec", "template", "spec")
}

func mustErr(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("err = %v, want it to contain %q", err, want)
	}
}

// TestPerKindObjects pins the object set each kind renders (the per-kind
// table in RenderWorkloads' doc, expand.k:496-801).
func TestPerKindObjects(t *testing.T) {
	cases := []struct {
		w    v1alpha1.Workload
		want []string
	}{
		{svc("api", httpPort(true)), []string{"Deployment/api", "NetworkPolicy/api-ingress", "Service/api", "ServiceAccount/api"}},
		{wl("w", v1alpha1.WorkloadSpec{Kind: v1alpha1.KindWorker}), []string{"Deployment/w", "NetworkPolicy/w-ingress", "ServiceAccount/w"}},
		{wl("c", v1alpha1.WorkloadSpec{Kind: v1alpha1.KindCron, Schedule: "@daily", Args: []string{"sweep"}}), []string{"CronJob/c", "NetworkPolicy/c-ingress", "ServiceAccount/c"}},
		{wl("op", v1alpha1.WorkloadSpec{Kind: v1alpha1.KindOperator, CRDs: []string{"Widget"}}), []string{
			"ClusterRole/op-acme-prod-clusterrole", "ClusterRoleBinding/op-acme-prod-clusterrolebinding", "Deployment/op", "ServiceAccount/op",
		}},
		{wl("cli", v1alpha1.WorkloadSpec{Kind: v1alpha1.KindTool}), nil},
	}
	for _, c := range cases {
		t.Run(c.w.Name, func(t *testing.T) {
			got := keysOf(objects(t, render(t, v1alpha1.ProfileFull, c.w)))
			if len(got) == 0 {
				got = nil
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("objects = %v\nwant      %v", got, c.want)
			}
		})
	}
	// A standalone job's name carries its spec hash.
	objs := objects(t, render(t, v1alpha1.ProfileFull, wl("seed", v1alpha1.WorkloadSpec{Kind: v1alpha1.KindJob, Args: []string{"seed"}})))
	var job map[string]any
	for k, o := range objs {
		if strings.HasPrefix(k, "Job/seed-") {
			job = o
		}
	}
	if job == nil || len(objs) != 3 {
		t.Fatalf("standalone job: want Job/seed-<hash> + SA + NetworkPolicy, got %v", keysOf(objs))
	}
	if get(job, "metadata", "annotations", AnnotationDeployPhase) != "pre-rollout" || get(job, "metadata", "labels", LabelJobName) != "seed" {
		t.Errorf("job metadata = %v", get(job, "metadata"))
	}
	if get(job, "spec", "backoffLimit") != float64(6) || get(podOf(job), "restartPolicy") != "OnFailure" {
		t.Errorf("job spec = %v", get(job, "spec"))
	}
}

// TestServiceDefaultPort: a service that declares no ports serves `http`
// :8080 (expand.k:502), and is NOT made public by omission.
func TestServiceDefaultPort(t *testing.T) {
	o := objects(t, render(t, v1alpha1.ProfileFull, svc("api")))
	want := []any{map[string]any{"name": "http", "port": float64(8080), "targetPort": float64(8080), "protocol": "TCP"}}
	if got := get(o["Service/api"], "spec", "ports"); !reflect.DeepEqual(got, want) {
		t.Fatalf("service ports = %v", got)
	}
	if get(o["NetworkPolicy/api-ingress"], "spec", "ingress", 0, "from", 0) == nil {
		t.Error("the default port must be namespace-private, not exposed")
	}
	// The TCP default probe (EffectiveProbes) lands on it.
	if get(podOf(o["Deployment/api"]), "containers", 0, "readinessProbe", "tcpSocket", "port") != float64(8080) {
		t.Errorf("default probe = %v", get(podOf(o["Deployment/api"]), "containers", 0, "readinessProbe"))
	}
}

// TestProbesSplit: readiness and liveness are separate objects; liveness
// timings are Probes.Liveness(); TCP is a connect; nil means none.
func TestProbesSplit(t *testing.T) {
	api := svc("api", httpPort(true))
	api.Spec.Probes = &v1alpha1.Probes{}
	c := get(podOf(objects(t, render(t, v1alpha1.ProfileFull, api))["Deployment/api"]), "containers", 0)
	ready, live := get(c, "readinessProbe"), get(c, "livenessProbe")
	if get(ready, "httpGet", "path") != "/readyz" || get(live, "httpGet", "path") != "/healthz" {
		t.Fatalf("paths: readiness %v liveness %v", ready, live)
	}
	want := v1alpha1.Probes{}.Liveness()
	if get(live, "initialDelaySeconds") != float64(want.InitialDelaySeconds) || get(live, "periodSeconds") != float64(want.PeriodSeconds) {
		t.Errorf("liveness timings %v, want %+v", live, want)
	}
	r := v1alpha1.Probes{}.Readiness()
	if get(ready, "timeoutSeconds") != float64(r.TimeoutSeconds) || get(ready, "periodSeconds") != float64(r.PeriodSeconds) {
		t.Errorf("readiness timings %v, want %+v", ready, r)
	}

	pg := wl("pg", v1alpha1.WorkloadSpec{Kind: v1alpha1.KindService, Ports: []v1alpha1.Port{{Name: "pg", Port: 5432}}, Probes: &v1alpha1.Probes{TCP: true}})
	c = get(podOf(objects(t, render(t, v1alpha1.ProfileFull, pg))["Deployment/pg"]), "containers", 0)
	for _, probe := range []string{"readinessProbe", "livenessProbe"} {
		if get(c, probe, "tcpSocket", "port") != float64(5432) || get(c, probe, "httpGet") != nil {
			t.Errorf("%s: want a TCP connect on 5432, got %v", probe, get(c, probe))
		}
	}

	w := wl("w", v1alpha1.WorkloadSpec{Kind: v1alpha1.KindWorker})
	c = get(podOf(objects(t, render(t, v1alpha1.ProfileFull, w))["Deployment/w"]), "containers", 0)
	if get(c, "readinessProbe") != nil || get(c, "livenessProbe") != nil || get(c, "ports") != nil {
		t.Errorf("a worker with no probes and no ports must get neither: %v", c)
	}
	// A worker probed on probes.port states the port it answers on
	// (expand.k:445-452).
	w.Spec.Probes = &v1alpha1.Probes{Port: 9090}
	c = get(podOf(objects(t, render(t, v1alpha1.ProfileFull, w))["Deployment/w"]), "containers", 0)
	if get(c, "ports", 0, "containerPort") != float64(9090) || get(c, "readinessProbe", "httpGet", "port") != float64(9090) {
		t.Errorf("probed worker: %v", c)
	}
}

// TestGracePeriod ports expand.k:239-253 and its _duration_seconds parser.
func TestGracePeriod(t *testing.T) {
	for _, c := range []struct {
		env  []v1alpha1.EnvVar
		want float64
	}{
		{nil, 40},
		{[]v1alpha1.EnvVar{{Name: "PRE_STOP_DELAY", Value: "10s"}, {Name: "SHUTDOWN_TIMEOUT", Value: "1m"}}, 75},
		{[]v1alpha1.EnvVar{{Name: "SHUTDOWN_TIMEOUT", Value: "1h"}}, 3610},
		{[]v1alpha1.EnvVar{{Name: "SHUTDOWN_TIMEOUT", Value: "1m30s"}}, 40}, // not <int><unit>: fallback
		{[]v1alpha1.EnvVar{{Name: "PRE_STOP_DELAY", Value: "7"}}, 42},
		{[]v1alpha1.EnvVar{{Name: "SHUTDOWN_TIMEOUT", ManagedSecret: &v1alpha1.ManagedSecretRef{Name: "X"}}}, 40}, // a reference has no literal
	} {
		w := svc("api")
		w.Spec.Env = c.env
		pod := podOf(objects(t, render(t, v1alpha1.ProfileFull, w))["Deployment/api"])
		if got := get(pod, "terminationGracePeriodSeconds"); got != c.want {
			t.Errorf("env %v: grace = %v, want %v", c.env, got, c.want)
		}
	}
}

// TestReplicasSpreadAndPDB: replicas > 1 get soft spread and a PDB
// (expand.k:255-282); one replica gets neither.
func TestReplicasSpreadAndPDB(t *testing.T) {
	one := objects(t, render(t, v1alpha1.ProfileFull, svc("api")))
	if _, ok := one["PodDisruptionBudget/api-pdb"]; ok || get(podOf(one["Deployment/api"]), "topologySpreadConstraints") != nil {
		t.Error("one replica must get no PDB and no spread")
	}
	w := svc("api")
	w.Spec.Replicas = 3
	o := objects(t, render(t, v1alpha1.ProfileFull, w))
	pdb := o["PodDisruptionBudget/api-pdb"]
	if get(pdb, "spec", "maxUnavailable") != float64(1) || get(pdb, "spec", "selector", "matchLabels", LabelName) != "api" {
		t.Errorf("pdb = %v", pdb)
	}
	spread := get(podOf(o["Deployment/api"]), "topologySpreadConstraints", 0)
	if get(spread, "whenUnsatisfiable") != "ScheduleAnyway" || get(spread, "topologyKey") != "kubernetes.io/hostname" {
		t.Errorf("spread = %v", spread)
	}
}

// TestStorage: a PVC at /data, Recreate, fsGroup.
func TestStorage(t *testing.T) {
	w := svc("db")
	w.Spec.StorageGiB = 5
	o := objects(t, render(t, v1alpha1.ProfileFull, w))
	dep := o["Deployment/db"]
	pod := podOf(dep)
	if get(dep, "spec", "strategy", "type") != "Recreate" {
		t.Errorf("strategy = %v", get(dep, "spec", "strategy"))
	}
	if get(pod, "securityContext", "fsGroup") != float64(RunAsUser) {
		t.Error("storage needs fsGroup so the non-root container can write its volume")
	}
	if get(pod, "containers", 0, "volumeMounts", 1, "mountPath") != "/data" || get(pod, "volumes", 1, "persistentVolumeClaim", "claimName") != "db-data" {
		t.Errorf("mounts %v volumes %v", get(pod, "containers", 0, "volumeMounts"), get(pod, "volumes"))
	}
	pvc := o["PersistentVolumeClaim/db-data"]
	if get(pvc, "spec", "resources", "requests", "storage") != "5Gi" || get(pvc, "spec", "accessModes", 0) != "ReadWriteOnce" {
		t.Errorf("pvc = %v", pvc)
	}
	// Stateless: no fsGroup, no Recreate.
	o = objects(t, render(t, v1alpha1.ProfileFull, svc("api")))
	if get(podOf(o["Deployment/api"]), "securityContext", "fsGroup") != nil || get(o["Deployment/api"], "spec", "strategy") != nil {
		t.Error("a stateless workload gets neither fsGroup nor a Recreate strategy")
	}
}

// TestKeepsSemanticEmptyMaps guards the pruning trap: in Kubernetes an empty
// map is often the meaning.
func TestKeepsSemanticEmptyMaps(t *testing.T) {
	o := objects(t, render(t, v1alpha1.ProfileFull, svc("api", httpPort(false))))
	if peer := get(o["NetworkPolicy/api-ingress"], "spec", "ingress", 0, "from", 0); !reflect.DeepEqual(peer, map[string]any{"podSelector": map[string]any{}}) {
		t.Fatalf("private workload's same-namespace peer was lost: %#v", peer)
	}
	if ed := get(podOf(o["Deployment/api"]), "volumes", 0, "emptyDir"); !reflect.DeepEqual(ed, map[string]any{}) {
		t.Fatalf("/tmp emptyDir source was lost: %#v", ed)
	}
	for k, obj := range o {
		if get(obj, "status") != nil || get(obj, "metadata", "creationTimestamp") != nil || get(obj, "spec", "template", "metadata", "creationTimestamp") != nil {
			t.Errorf("%s carries server-owned fields", k)
		}
	}
}

// TestNetworkPolicy: namespace ingress on declared ports, any-source ingress
// on the exposed port, default-deny for a pod with no ports, none for an
// operator.
func TestNetworkPolicy(t *testing.T) {
	api := svc("api", httpPort(true), v1alpha1.Port{Name: "grpc", Port: 9090})
	np := objects(t, render(t, v1alpha1.ProfileFull, api))["NetworkPolicy/api-ingress"]
	if got := get(np, "spec", "policyTypes"); !reflect.DeepEqual(got, []any{"Ingress"}) {
		t.Errorf("policyTypes = %v; egress is the namespace's control", got)
	}
	private, public := get(np, "spec", "ingress", 0), get(np, "spec", "ingress", 1)
	if get(private, "ports", 0, "port") != float64(9090) || get(private, "from", 0) == nil || get(private, "ports", 1) != nil {
		t.Errorf("private rule = %v", private)
	}
	if get(public, "ports", 0, "port") != float64(8080) || get(public, "from") != nil {
		t.Errorf("public rule = %v (want from anywhere)", public)
	}
	np = objects(t, render(t, v1alpha1.ProfileFull, wl("w", v1alpha1.WorkloadSpec{Kind: v1alpha1.KindWorker})))["NetworkPolicy/w-ingress"]
	if np == nil || get(np, "spec", "ingress") != nil {
		t.Errorf("a worker with no ports must get an ingress default-deny policy, got %v", np)
	}
}

// TestEnvNetworkBundle ports kcl/lib/netpol.k. It is ENV scope, so it rides
// Context.Network and is emitted once, not per workload.
func TestEnvNetworkBundle(t *testing.T) {
	c := ctx
	c.Network = &EnvNetworkPolicy{EgressPorts: []int32{443, 5432}, TelemetryNamespace: "observability", IngressNamespace: "gw"}
	objs, err := RenderWorkloads([]v1alpha1.Workload{svc("a"), svc("b")}, v1alpha1.ProfileFull, c)
	if err != nil {
		t.Fatal(err)
	}
	o := objects(t, objs)
	for _, name := range []string{"default-deny", "allow-dns", "allow-internal", "allow-egress-external", "allow-egress-telemetry", "allow-ingress-controller"} {
		if o["NetworkPolicy/"+name] == nil {
			t.Errorf("missing NetworkPolicy/%s", name)
		}
	}
	if got := get(o["NetworkPolicy/default-deny"], "spec"); !reflect.DeepEqual(got, map[string]any{"podSelector": map[string]any{}, "policyTypes": []any{"Ingress", "Egress"}}) {
		t.Errorf("default-deny spec = %v", got)
	}
	if get(o["NetworkPolicy/allow-egress-external"], "spec", "egress", 0, "ports", 1, "port") != float64(5432) {
		t.Errorf("egress ports = %v", get(o["NetworkPolicy/allow-egress-external"], "spec", "egress"))
	}
	c.Network = &EnvNetworkPolicy{}
	objs, _ = RenderWorkloads([]v1alpha1.Workload{svc("a")}, v1alpha1.ProfileFull, c)
	o = objects(t, objs)
	if o["NetworkPolicy/allow-egress-external"] != nil || o["NetworkPolicy/allow-egress-telemetry"] != nil || o["NetworkPolicy/allow-ingress-controller"] != nil {
		t.Error("undeclared egress/telemetry/ingress must OMIT the policy, not render one against nothing")
	}
}

// TestEnvChannels pins every channel onto the source whose name is a
// contract with another component.
func TestEnvChannels(t *testing.T) {
	w := svc("api")
	w.Spec.Env = []v1alpha1.EnvVar{
		{Name: "PLAIN", Value: "v"},
		{Name: "EMPTY"},
		{Name: "SREF", SecretRef: &v1alpha1.SecretKeyRef{Name: "s", Key: "k"}},
		{Name: "SREF_OPT", SecretRef: &v1alpha1.SecretKeyRef{Name: "s", Key: "maybe", Optional: true}},
		{Name: "MANAGED", ManagedSecret: &v1alpha1.ManagedSecretRef{Name: "STRIPE_KEY"}},
		{Name: "MANAGED_OPT", ManagedSecret: &v1alpha1.ManagedSecretRef{Name: "SENTRY_DSN", Optional: true}},
		{Name: "DB", DatabaseRef: &v1alpha1.DatabaseRef{Name: "orders"}},
		{Name: "DBPASS", DatabaseRef: &v1alpha1.DatabaseRef{Name: "orders", Key: v1alpha1.DatabaseKeyPassword}},
		{Name: "CM", ConfigMapRef: &v1alpha1.ConfigMapKeyRef{Name: "features", Key: "map"}},
		{Name: "POD_IP", FieldRef: &v1alpha1.FieldRef{FieldPath: "status.podIP"}},
	}
	env := get(podOf(objects(t, render(t, v1alpha1.ProfileFull, w))["Deployment/api"]), "containers", 0, "env").([]any)
	want := []any{
		map[string]any{"name": "PLAIN", "value": "v"},
		map[string]any{"name": "EMPTY"},
		map[string]any{"name": "SREF", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "s", "key": "k"}}},
		map[string]any{"name": "SREF_OPT", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "s", "key": "maybe", "optional": true}}},
		map[string]any{"name": "MANAGED", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": v1alpha1.ManagedSecretsSecretName, "key": "STRIPE_KEY"}}},
		map[string]any{"name": "MANAGED_OPT", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": v1alpha1.ManagedSecretsSecretName, "key": "SENTRY_DSN", "optional": true}}},
		map[string]any{"name": "DB", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "orders-app", "key": "uri"}}},
		map[string]any{"name": "DBPASS", "valueFrom": map[string]any{"secretKeyRef": map[string]any{"name": "orders-app", "key": "password"}}},
		map[string]any{"name": "CM", "valueFrom": map[string]any{"configMapKeyRef": map[string]any{"name": "features", "key": "map"}}},
		map[string]any{"name": "POD_IP", "valueFrom": map[string]any{"fieldRef": map[string]any{"fieldPath": "status.podIP"}}},
	}
	if !reflect.DeepEqual(env, want) {
		gb, _ := json.MarshalIndent(env, "", " ")
		t.Fatalf("env =\n%s", gb)
	}
}

// TestIdentity: one SA per workload; no token unless RBAC; the namespaced
// Role carries the default config-read rules; an operator's ClusterRole
// REPLACES a Role; annotations ride the SA only; an override renders none.
func TestIdentity(t *testing.T) {
	o := objects(t, render(t, v1alpha1.ProfileFull, svc("api")))
	if get(o["ServiceAccount/api"], "automountServiceAccountToken") != false || get(podOf(o["Deployment/api"]), "automountServiceAccountToken") != false {
		t.Error("a workload with no RBAC must mount no token, on the SA and the pod")
	}
	if get(podOf(o["Deployment/api"]), "serviceAccountName") != "api" {
		t.Error("pod must bind its own SA, never the namespace default")
	}

	rule := v1alpha1.PolicyRule{APIGroups: []string{"example.com"}, Resources: []string{"widgets"}, Verbs: []string{"list"}}
	w := wl("reaper", v1alpha1.WorkloadSpec{Kind: v1alpha1.KindWorker, NamespacedRBAC: []v1alpha1.PolicyRule{rule}})
	o = objects(t, render(t, v1alpha1.ProfileFull, w))
	role := o["Role/reaper-role"]
	if len(get(role, "rules").([]any)) != 2 || get(role, "rules", 0, "resources", 1) != "secrets" || get(role, "rules", 1, "apiGroups", 0) != "example.com" {
		t.Errorf("role rules = %v (want lib/rbac.k defaults + declared)", get(role, "rules"))
	}
	if get(o["RoleBinding/reaper-rolebinding"], "subjects", 0, "name") != "reaper" {
		t.Error("RoleBinding must bind the workload's SA")
	}
	if get(podOf(o["Deployment/reaper"]), "automountServiceAccountToken") != true || get(o["ServiceAccount/reaper"], "automountServiceAccountToken") != nil {
		t.Error("a workload WITH RBAC needs its token")
	}

	op := wl("mgr", v1alpha1.WorkloadSpec{
		Kind: v1alpha1.KindOperator, CRDs: []string{"Widget"}, ClusterRBAC: []v1alpha1.PolicyRule{rule},
		ServiceAccountAnnotations: map[string]string{"iam.gke.io/gcp-service-account": "m@p.iam.gserviceaccount.com"},
	})
	o = objects(t, render(t, v1alpha1.ProfileFull, op))
	for k := range o {
		if strings.HasPrefix(k, "Role/") || strings.HasPrefix(k, "RoleBinding/") {
			t.Errorf("operator rendered %s: the cluster tier REPLACES the namespaced one", k)
		}
	}
	if get(o["ClusterRoleBinding/mgr-acme-prod-clusterrolebinding"], "subjects", 0, "namespace") != "acme-prod" {
		t.Error("cluster binding subject must be the env's namespace")
	}
	if get(o["ServiceAccount/mgr"], "metadata", "annotations", "iam.gke.io/gcp-service-account") == nil {
		t.Error("SA annotations lost")
	}
	if get(o["Deployment/mgr"], "metadata", "annotations") != nil || get(o["Deployment/mgr"], "spec", "template", "metadata", "annotations") != nil {
		t.Error("SA annotations must ride the SA only")
	}
	if get(podOf(o["Deployment/mgr"]), "containers", 0, "env", 0) == nil {
		t.Error("operator must get LEADER_ELECTION")
	}
	off := false
	op.Spec.LeaderElection = &off
	if get(podOf(objects(t, render(t, v1alpha1.ProfileFull, op))["Deployment/mgr"]), "containers", 0, "env") != nil {
		t.Error("leaderElection=false must render no LEADER_ELECTION")
	}

	ov := svc("api")
	ov.Spec.ServiceAccount = "reliant-cloudsql"
	o = objects(t, render(t, v1alpha1.ProfileFull, ov))
	if o["ServiceAccount/api"] != nil {
		t.Error("an SA override must render no ServiceAccount: forge does not own it")
	}
	pod := podOf(o["Deployment/api"])
	if get(pod, "serviceAccountName") != "reliant-cloudsql" || get(pod, "automountServiceAccountToken") != nil {
		t.Errorf("override pod = sa %v automount %v (the SA's owner decides)", get(pod, "serviceAccountName"), get(pod, "automountServiceAccountToken"))
	}
}

// TestBefore: broadcast gating, the gated-kinds rules, ordering, and the
// cross-workload refusals.
func TestBefore(t *testing.T) {
	migrate := wl("migrate", v1alpha1.WorkloadSpec{Kind: v1alpha1.KindJob, Command: []string{"/app/x", "migrate"}, Before: []string{v1alpha1.BeforeAll}})
	migrate.Spec.Image = "ghcr.io/acme/migrate:v1"
	seed := wl("seed", v1alpha1.WorkloadSpec{Kind: v1alpha1.KindJob, Args: []string{"seed"}, Before: []string{"api"}})
	provision := wl("provision", v1alpha1.WorkloadSpec{Kind: v1alpha1.KindJob, Args: []string{"provision"}})
	cron := wl("nightly", v1alpha1.WorkloadSpec{Kind: v1alpha1.KindCron, Schedule: "@daily", Args: []string{"x"}})
	op := wl("mgr", v1alpha1.WorkloadSpec{Kind: v1alpha1.KindOperator, CRDs: []string{"W"}})
	worker := wl("w", v1alpha1.WorkloadSpec{Kind: v1alpha1.KindWorker})
	tool := wl("cli", v1alpha1.WorkloadSpec{Kind: v1alpha1.KindTool})

	// seed declared BEFORE migrate: migrate still runs first on api,
	// because migrate gates seed.
	o := objects(t, render(t, v1alpha1.ProfileFull, seed, svc("api"), migrate, provision, cron, op, worker, tool))
	inits := func(key string) []string {
		var names []string
		for _, c := range asSlice(get(podOf(o[key]), "initContainers")) {
			names = append(names, get(c, "name").(string))
		}
		return names
	}
	var provisionJob string
	for k := range o {
		if strings.HasPrefix(k, "Job/provision-") {
			provisionJob = k
		}
		if strings.HasPrefix(k, "Job/migrate") || strings.HasPrefix(k, "Job/seed") || k == "ServiceAccount/migrate" || k == "ServiceAccount/seed" {
			t.Errorf("a gating job must render nothing of its own, got %s", k)
		}
	}
	for key, want := range map[string][]string{
		"Deployment/api":  {"migrate", "seed"},
		"Deployment/w":    {"migrate"},
		"Deployment/mgr":  {"migrate"},
		provisionJob:      {"migrate"},
		"CronJob/nightly": nil,
	} {
		if got := inits(key); !reflect.DeepEqual(got, want) {
			t.Errorf("%s initContainers = %v, want %v", key, got, want)
		}
	}
	init := get(podOf(o["Deployment/api"]), "initContainers", 0)
	if get(init, "image") != "ghcr.io/acme/migrate:v1" || !reflect.DeepEqual(get(init, "command"), []any{"/app/x", "migrate"}) {
		t.Errorf("init runs the JOB's image and command: %v", init)
	}
	if get(init, "volumeMounts", 0, "mountPath") != "/tmp" || get(init, "securityContext", "readOnlyRootFilesystem") != true {
		t.Errorf("init hardening/tmp: %v", init)
	}

	for _, c := range []struct {
		name string
		ws   []v1alpha1.Workload
		want string
	}{
		{"dangling", []v1alpha1.Workload{svc("api"), wl("j", v1alpha1.WorkloadSpec{Kind: v1alpha1.KindJob, Args: []string{"x"}, Before: []string{"apii"}})}, `names "apii", which does not exist`},
		{"cron target", []v1alpha1.Workload{cron, wl("j", v1alpha1.WorkloadSpec{Kind: v1alpha1.KindJob, Args: []string{"x"}, Before: []string{"nightly"}})}, "cannot be gated"},
		{"cycle", []v1alpha1.Workload{
			wl("a", v1alpha1.WorkloadSpec{Kind: v1alpha1.KindJob, Args: []string{"x"}, Before: []string{"b"}}),
			wl("b", v1alpha1.WorkloadSpec{Kind: v1alpha1.KindJob, Args: []string{"x"}, Before: []string{"a"}}),
		}, "cycle"},
		{"gating job rbac", []v1alpha1.Workload{svc("api"), wl("j", v1alpha1.WorkloadSpec{Kind: v1alpha1.KindJob, Args: []string{"x"}, Before: []string{"api"},
			NamespacedRBAC: []v1alpha1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"}}}})}, "namespacedRBAC is not allowed on a job with before"},
		{"duplicate", []v1alpha1.Workload{svc("api"), svc("api")}, "declared twice"},
	} {
		t.Run(c.name, func(t *testing.T) {
			objs, err := RenderWorkloads(c.ws, v1alpha1.ProfileFull, ctx)
			mustErr(t, err, c.want)
			if objs != nil {
				t.Error("a refused set must emit nothing")
			}
		})
	}
}

// TestErrorsReportedOnce: RenderWorkloads admits through Workload.Validate
// and does not re-check what it already checks, so each violation appears in
// the error exactly once. A duplicated message reads as two problems, and an
// author fixing the first finds the second was the same one.
func TestErrorsReportedOnce(t *testing.T) {
	gating := wl("j", v1alpha1.WorkloadSpec{Kind: v1alpha1.KindJob, Args: []string{"x"}, Before: []string{"api"},
		NamespacedRBAC: []v1alpha1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"}}}})
	long := wl(strings.Repeat("s", 53), v1alpha1.WorkloadSpec{Kind: v1alpha1.KindJob, Args: []string{"x"}})
	bad := svc("Bad_Name")
	_, err := RenderWorkloads([]v1alpha1.Workload{svc("api"), gating, long, bad}, v1alpha1.ProfileFull, ctx)
	if err == nil {
		t.Fatal("want an error")
	}
	for _, want := range []string{
		"namespacedRBAC is not allowed on a job with before",
		"a standalone job's name is at most 52 characters",
		`name "Bad_Name" must be an RFC-1123 label`,
	} {
		if n := strings.Count(err.Error(), want); n != 1 {
			t.Errorf("%q appears %d times in the error, want exactly once:\n%v", want, n, err)
		}
	}
}

func asSlice(v any) []any { s, _ := v.([]any); return s }

// TestPodEscapeHatches: sidecars get the same hardening, volumes mount on
// the main container, placement lands on the pod.
func TestPodEscapeHatches(t *testing.T) {
	w := svc("api", httpPort(true))
	w.Spec.Sidecars = []v1alpha1.Container{{Name: "proxy", Image: "gcr.io/cloud-sql-connectors/cloud-sql-proxy:2.14.1", Args: []string{"--port=5432"}}}
	w.Spec.Volumes = []v1alpha1.Volume{
		{Name: "certs", MountPath: "/etc/certs", ReadOnly: true, Source: v1alpha1.VolumeSource{Secret: &v1alpha1.SecretVolumeSource{Name: "tls"}}},
		{Name: "shared", MountPath: "/shared", Source: v1alpha1.VolumeSource{PVC: &v1alpha1.PVCVolumeSource{ClaimName: "shared"}}},
	}
	w.Spec.NodeSelector = map[string]string{"pool": "general"}
	w.Spec.Tolerations = []v1alpha1.Toleration{{Key: "dedicated", Operator: v1alpha1.TolerationOpEqual, Value: "api", Effect: v1alpha1.TaintNoSchedule}}
	w.Spec.PodAnnotations = map[string]string{"prometheus.io/scrape": "true"}
	w.Spec.Ports[0].AppProtocol = "h2c"
	o := objects(t, render(t, v1alpha1.ProfileFull, w))
	dep := o["Deployment/api"]
	pod := podOf(dep)
	side := get(pod, "containers", 1)
	if !reflect.DeepEqual(get(side, "securityContext"), get(pod, "containers", 0, "securityContext")) {
		t.Errorf("sidecar hardening differs from main: %v", get(side, "securityContext"))
	}
	if get(pod, "containers", 0, "volumeMounts", 1, "mountPath") != "/etc/certs" || get(pod, "containers", 0, "volumeMounts", 1, "readOnly") != true {
		t.Errorf("mounts = %v", get(pod, "containers", 0, "volumeMounts"))
	}
	if get(pod, "securityContext", "fsGroup") != float64(RunAsUser) {
		t.Error("a mounted PVC needs fsGroup")
	}
	if get(pod, "nodeSelector", "pool") != "general" || get(pod, "tolerations", 0, "effect") != "NoSchedule" {
		t.Errorf("placement = %v %v", get(pod, "nodeSelector"), get(pod, "tolerations"))
	}
	if get(dep, "spec", "template", "metadata", "annotations", "prometheus.io/scrape") != "true" {
		t.Error("pod annotations lost")
	}
	if get(o["Service/api"], "spec", "ports", 0, "appProtocol") != "h2c" {
		t.Error("appProtocol lost")
	}
}

// TestRestrictedKindSet renders the widest set ProfileRestricted accepts and
// asserts the EXACT object kinds: only what reaches the tenant's own pods.
// No RBAC, ever.
func TestRestrictedKindSet(t *testing.T) {
	objs := render(t, v1alpha1.ProfileRestricted, restrictedSet()...)
	got := map[string]bool{}
	for _, o := range objs {
		got[o.GetKind()] = true
	}
	if !reflect.DeepEqual(got, RestrictedKinds) {
		t.Fatalf("restricted kinds = %v\nwant exactly     %v", sortedKindNames(got), sortedKindNames(RestrictedKinds))
	}
	for k, o := range objects(t, objs) {
		if strings.HasPrefix(k, "ServiceAccount/") && get(o, "automountServiceAccountToken") != false {
			t.Errorf("%s mounts a token under restricted", k)
		}
		pod := podOf(o)
		if pod == nil {
			continue
		}
		// None of the Full-only pod features may appear.
		for _, f := range []string{"nodeSelector", "tolerations", "affinity", "hostNetwork", "hostPID", "hostIPC", "runtimeClassName", "priorityClassName"} {
			if get(pod, f) != nil {
				t.Errorf("%s: %s under restricted", k, f)
			}
		}
		if get(pod, "automountServiceAccountToken") != false {
			t.Errorf("%s pod mounts a token under restricted", k)
		}
		name := get(o, "metadata", "name").(string)
		if strings.HasPrefix(k, "Job/") {
			name = get(o, "metadata", "labels", LabelJobName).(string)
		}
		if get(pod, "serviceAccountName") != name {
			t.Errorf("%s runs as %v, want its own generated SA", k, get(pod, "serviceAccountName"))
		}
		if len(asSlice(get(pod, "containers"))) != 1 {
			t.Errorf("%s has sidecars under restricted", k)
		}
		for _, v := range asSlice(get(pod, "volumes")) {
			vn := get(v, "name")
			if vn != v1alpha1.TmpVolumeName && vn != v1alpha1.DataVolumeName {
				t.Errorf("%s: declared volume %v under restricted", k, vn)
			}
		}
		if get(o, "spec", "template", "metadata", "annotations") != nil {
			t.Errorf("%s: pod annotations under restricted", k)
		}
	}
}

func restrictedSet() []v1alpha1.Workload {
	img := "ghcr.io/acme/app:v1"
	web := wl("web", v1alpha1.WorkloadSpec{Kind: v1alpha1.KindService, Image: img, Replicas: 3, Ports: []v1alpha1.Port{httpPort(true)},
		Probes: &v1alpha1.Probes{}, Env: []v1alpha1.EnvVar{{Name: "DB", DatabaseRef: &v1alpha1.DatabaseRef{Name: "orders"}}, {Name: "KEY", ManagedSecret: &v1alpha1.ManagedSecretRef{Name: "KEY"}}, {Name: "DSN", ManagedSecret: &v1alpha1.ManagedSecretRef{Name: "DSN", Optional: true}}}})
	store := wl("store", v1alpha1.WorkloadSpec{Kind: v1alpha1.KindService, Image: img, StorageGiB: 10})
	worker := wl("jobs", v1alpha1.WorkloadSpec{Kind: v1alpha1.KindWorker, Image: img, Args: []string{"work"}})
	migrate := wl("migrate", v1alpha1.WorkloadSpec{Kind: v1alpha1.KindJob, Image: img, Args: []string{"db", "migrate", "up"}, Before: []string{v1alpha1.BeforeAll}})
	report := wl("report", v1alpha1.WorkloadSpec{Kind: v1alpha1.KindJob, Image: img, Args: []string{"report"}, DeployPhase: v1alpha1.DeployPhasePostRollout})
	return []v1alpha1.Workload{migrate, web, store, worker, report}
}

// TestRestrictedRefusesFullOnly: Validate(Restricted) runs first, so a set
// carrying any Full-only feature renders NOTHING.
func TestRestrictedRefusesFullOnly(t *testing.T) {
	rule := []v1alpha1.PolicyRule{{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get"}}}
	for name, mutate := range map[string]func(*v1alpha1.WorkloadSpec){
		"namespacedRBAC": func(s *v1alpha1.WorkloadSpec) { s.NamespacedRBAC = rule },
		"sidecars": func(s *v1alpha1.WorkloadSpec) {
			s.Sidecars = []v1alpha1.Container{{Name: "x", Image: "ghcr.io/a/b:v1"}}
		},
		"volumes": func(s *v1alpha1.WorkloadSpec) {
			s.Volumes = []v1alpha1.Volume{{Name: "v", MountPath: "/v", Source: v1alpha1.VolumeSource{EmptyDir: &v1alpha1.EmptyDirVolumeSource{}}}}
		},
		"serviceAccount": func(s *v1alpha1.WorkloadSpec) { s.ServiceAccount = "x" },
		"nodeSelector":   func(s *v1alpha1.WorkloadSpec) { s.NodeSelector = map[string]string{"a": "b"} },
		"tolerations": func(s *v1alpha1.WorkloadSpec) {
			s.Tolerations = []v1alpha1.Toleration{{Operator: v1alpha1.TolerationOpExists}}
		},
		"podAnnotations": func(s *v1alpha1.WorkloadSpec) { s.PodAnnotations = map[string]string{"a": "b"} },
		"secretRef": func(s *v1alpha1.WorkloadSpec) {
			s.Env = []v1alpha1.EnvVar{{Name: "X", SecretRef: &v1alpha1.SecretKeyRef{Name: "a", Key: "b"}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			ws := restrictedSet()
			mutate(&ws[1].Spec)
			objs, err := RenderWorkloads(ws, v1alpha1.ProfileRestricted, ctx)
			mustErr(t, err, name)
			if objs != nil {
				t.Error("a refused set must emit nothing")
			}
		})
	}
	op := wl("mgr", v1alpha1.WorkloadSpec{Kind: v1alpha1.KindOperator, CRDs: []string{"W"}})
	_, err := RenderWorkloads([]v1alpha1.Workload{op}, v1alpha1.ProfileRestricted, ctx)
	mustErr(t, err, `kind "operator" is not allowed`)
}

// TestEveryPodIsPSARestricted checks every pod template RenderWorkloads
// emits, under both profiles, against the Pod Security `restricted`
// standard the control plane enforces on tenant namespaces (and forge's own
// namespaces label): runAsNonRoot, seccomp RuntimeDefault, and for every
// container (main, sidecar, init) no privilege escalation and ALL
// capabilities dropped; no hostPath and no host namespaces.
func TestEveryPodIsPSARestricted(t *testing.T) {
	full := append(restrictedSet(),
		wl("nightly", v1alpha1.WorkloadSpec{Kind: v1alpha1.KindCron, Schedule: "@daily", Args: []string{"x"}}),
		wl("mgr", v1alpha1.WorkloadSpec{Kind: v1alpha1.KindOperator, CRDs: []string{"W"}}),
	)
	full[1].Spec.Sidecars = []v1alpha1.Container{{Name: "proxy", Image: "gcr.io/p/proxy:1"}}
	for profile, ws := range map[v1alpha1.Profile][]v1alpha1.Workload{v1alpha1.ProfileFull: full, v1alpha1.ProfileRestricted: restrictedSet()} {
		pods := 0
		for k, o := range objects(t, render(t, profile, ws...)) {
			pod := podOf(o)
			if pod == nil {
				continue
			}
			pods++
			if msg := psaRestrictedViolation(pod); msg != "" {
				t.Errorf("%s %s: %s", profile, k, msg)
			}
		}
		if pods == 0 {
			t.Fatalf("%s: no pod templates checked", profile)
		}
	}
}

func psaRestrictedViolation(pod any) string {
	if get(pod, "securityContext", "runAsNonRoot") != true {
		return "pod runAsNonRoot is not true"
	}
	if get(pod, "securityContext", "seccompProfile", "type") != "RuntimeDefault" {
		return "pod seccomp is not RuntimeDefault"
	}
	for _, f := range []string{"hostNetwork", "hostPID", "hostIPC"} {
		if get(pod, f) == true {
			return f
		}
	}
	for _, v := range asSlice(get(pod, "volumes")) {
		if get(v, "hostPath") != nil {
			return "hostPath volume"
		}
	}
	containers := append(asSlice(get(pod, "containers")), asSlice(get(pod, "initContainers"))...)
	for _, c := range containers {
		sc := get(c, "securityContext")
		switch {
		case sc == nil:
			return "container " + get(c, "name").(string) + " has no securityContext"
		case get(sc, "allowPrivilegeEscalation") != false:
			return "container " + get(c, "name").(string) + " allows privilege escalation"
		case !slices.Equal(anyStrings(get(sc, "capabilities", "drop")), []string{"ALL"}):
			return "container " + get(c, "name").(string) + " does not drop ALL"
		case get(sc, "capabilities", "add") != nil:
			return "container " + get(c, "name").(string) + " adds capabilities"
		case get(sc, "privileged") == true:
			return "container " + get(c, "name").(string) + " is privileged"
		case get(sc, "runAsNonRoot") == false:
			return "container " + get(c, "name").(string) + " overrides runAsNonRoot"
		}
	}
	return ""
}

func anyStrings(v any) []string {
	var out []string
	for _, s := range asSlice(v) {
		out = append(out, s.(string))
	}
	return out
}

// TestPullPolicy ports expand.k:73-79.
func TestPullPolicy(t *testing.T) {
	for img, want := range map[string]string{
		"ghcr.io/a/b:v1": "IfNotPresent", "ghcr.io/a/b:latest": "Always", "ghcr.io/a/b": "Always",
		"ghcr.io/a/b:abc123-dirty": "Always", "localhost:5000/b": "Always", "localhost:5000/b:v2": "IfNotPresent",
	} {
		if got := string(pullPolicy(img)); got != want {
			t.Errorf("pullPolicy(%s) = %s, want %s", img, got, want)
		}
	}
}

// TestEnvStamp ports kcl/lib/labels.k: forge.dev/env on every object and
// every pod template, never on a selector.
func TestEnvStamp(t *testing.T) {
	c := ctx
	c.Env = "prod"
	c.Network = &EnvNetworkPolicy{}
	ws := append(restrictedSet(), wl("nightly", v1alpha1.WorkloadSpec{Kind: v1alpha1.KindCron, Schedule: "@daily", Args: []string{"x"}}))
	objs, err := RenderWorkloads(ws, v1alpha1.ProfileFull, c)
	if err != nil {
		t.Fatal(err)
	}
	for k, o := range objects(t, objs) {
		if get(o, "metadata", "labels", LabelEnv) != "prod" {
			t.Errorf("%s: no env stamp", k)
		}
		if pod := podOf(o); pod != nil {
			tmpl := get(o, "spec", "template", "metadata", "labels", LabelEnv)
			if o["kind"] == "CronJob" {
				tmpl = get(o, "spec", "jobTemplate", "spec", "template", "metadata", "labels", LabelEnv)
			}
			if tmpl != "prod" {
				t.Errorf("%s: pod template not stamped", k)
			}
		}
		if get(o, "spec", "selector", "matchLabels", LabelEnv) != nil || get(o, "spec", "selector", LabelEnv) != nil {
			t.Errorf("%s: env stamp on a selector", k)
		}
	}
}

// TestRenderIsDeterministic: same input, byte-identical output. Two
// executors apply this against the same objects.
func TestRenderIsDeterministic(t *testing.T) {
	first := ""
	for i := 0; i < 30; i++ {
		objs, err := RenderWorkloads(goldenCases()[len(goldenCases())-1].ws, v1alpha1.ProfileFull, ctx)
		if err != nil {
			t.Fatal(err)
		}
		db, _ := RenderManagedDatabase("orders", v1alpha1.ManagedDatabaseSpec{}, ctx)
		b, _ := json.Marshal(append(objs, db...))
		if i == 0 {
			first = string(b)
		} else if string(b) != first {
			t.Fatal("RenderWorkloads produced different bytes for the same input")
		}
	}
}

// TestRenderRefusesInvalid: validation runs before anything is emitted.
func TestRenderRefusesInvalid(t *testing.T) {
	_, err := RenderWorkloads([]v1alpha1.Workload{svc("api")}, v1alpha1.ProfileFull, Context{})
	mustErr(t, err, "namespace")
	_, err = RenderWorkloads([]v1alpha1.Workload{svc("Bad_Name")}, v1alpha1.ProfileFull, ctx)
	mustErr(t, err, "RFC-1123")
	bare := svc("api")
	bare.Spec.Image = "api:latest"
	if _, err := RenderWorkloads([]v1alpha1.Workload{bare}, v1alpha1.ProfileFull, ctx); err != nil {
		t.Errorf("a cluster the author operates may run a bare imported image: %v", err)
	}
	_, err = RenderWorkloads([]v1alpha1.Workload{bare}, v1alpha1.ProfileRestricted, ctx)
	mustErr(t, err, "registry host")
	long := wl(strings.Repeat("j", 55), v1alpha1.WorkloadSpec{Kind: v1alpha1.KindJob, Args: []string{"x"}})
	_, err = RenderWorkloads([]v1alpha1.Workload{long}, v1alpha1.ProfileFull, ctx)
	mustErr(t, err, "at most 52 characters")
	if _, err := RenderManagedDatabase("orders", v1alpha1.ManagedDatabaseSpec{Instances: 9}, ctx); err == nil {
		t.Error("out-of-range instances must be refused, not clamped")
	}
}

// TestJobHashTracksSpec: an unchanged spec keeps its Job name (a re-apply is
// a no-op); a changed one gets a new name.
func TestJobHashTracksSpec(t *testing.T) {
	name := func(args ...string) string {
		for _, o := range render(t, v1alpha1.ProfileFull, wl("seed", v1alpha1.WorkloadSpec{Kind: v1alpha1.KindJob, Args: args})) {
			if o.GetKind() == "Job" {
				return o.GetName()
			}
		}
		return ""
	}
	a, b, c := name("seed"), name("seed"), name("seed", "--all")
	if a != b || a == c || !strings.HasPrefix(a, "seed-") || len(a) != len("seed-")+v1alpha1.StandaloneJobHashLength {
		t.Errorf("job names %q %q %q", a, b, c)
	}
}

// TestJobHasNoTTL: a finished Job must persist. It is the record that this
// spec already ran; if the TTL controller deletes it, the next apply (a GitOps
// reconciler, the hosted operator, a redeploy) re-creates it and the
// migration runs again. Superseded Jobs go away by spec-hash name + prune.
func TestJobHasNoTTL(t *testing.T) {
	for _, o := range render(t, v1alpha1.ProfileFull, wl("migrate", v1alpha1.WorkloadSpec{Kind: v1alpha1.KindJob, Args: []string{"migrate"}})) {
		if o.GetKind() != "Job" {
			continue
		}
		if ttl := get(objects(t, []*unstructured.Unstructured{o})["Job/"+o.GetName()], "spec", "ttlSecondsAfterFinished"); ttl != nil {
			t.Errorf("Job %s sets ttlSecondsAfterFinished=%v: a garbage-collected Job is re-created by the next apply and RUNS AGAIN", o.GetName(), ttl)
		}
		return
	}
	t.Fatal("no Job rendered for a standalone job workload")
}

// TestRenderManagedDatabase pins control-plane's live-proven CNPG Cluster
// shape (manageddatabase.BuildClusterPlan) and the names DatabaseRef depends
// on.
func TestRenderManagedDatabase(t *testing.T) {
	objs, err := RenderManagedDatabase("order-db", v1alpha1.ManagedDatabaseSpec{Instances: 2, StorageGiB: 50}, ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(objs) != 1 || objs[0].GetKind() != "Cluster" || objs[0].GetAPIVersion() != "postgresql.cnpg.io/v1" {
		t.Fatalf("want exactly one postgresql.cnpg.io/v1 Cluster, got %v", objs)
	}
	c := objects(t, objs)["Cluster/order-db"]
	want := map[string]any{
		"instances":             float64(2),
		"enableSuperuserAccess": false,
		"bootstrap":             map[string]any{"initdb": map[string]any{"database": "order_db", "owner": "order_db_app"}},
		"storage":               map[string]any{"size": "50Gi"},
	}
	if !reflect.DeepEqual(c["spec"], want) {
		t.Errorf("Cluster spec = %#v\nwant %#v", c["spec"], want)
	}
	if get(c, "metadata", "annotations", AnnotationDeletionPolicy) != "retain" {
		t.Errorf("deletion policy annotation = %v, want retain", get(c, "metadata", "annotations", AnnotationDeletionPolicy))
	}
	if DatabaseCredentialSecretName("order-db") != "order-db-app" {
		t.Error("CNPG publishes <cluster>-app; DatabaseRef reads it")
	}
}

// TestRenderDispatch: Render handles the per-object tiers and REFUSES a
// Workload, which must be rendered with its set.
func TestRenderDispatch(t *testing.T) {
	md := &v1alpha1.ManagedDatabase{}
	md.Name = "orders"
	if objs, err := Render(md, ctx); err != nil || len(objs) != 1 {
		t.Fatalf("ManagedDatabase: %v %d", err, len(objs))
	}
	ss := &v1alpha1.StaticSite{}
	ss.Name = "web"
	if objs, err := Render(ss, ctx); err != nil || len(objs) != 0 {
		t.Fatalf("StaticSite: want no objects and no error, got %v %d", err, len(objs))
	}
	w := svc("api")
	_, err := Render(&w, ctx)
	mustErr(t, err, "RenderWorkloads")
	if _, err := Render(&v1alpha1.WorkloadList{}, ctx); err == nil {
		t.Error("a non-tier object must be refused")
	}
}
