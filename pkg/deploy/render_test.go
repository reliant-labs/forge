package deploy

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/reliant-labs/forge/pkg/deploy/v1alpha1"
)

var ctx = Context{Namespace: "acme-prod", PartOf: "proj"}

func byKind(t *testing.T, objs []*unstructured.Unstructured) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, o := range objs {
		b, err := json.Marshal(o.Object)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		out[o.GetKind()] = m
	}
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

func backend(net v1alpha1.Network, ports ...int32) v1alpha1.SimpleBackendSpec {
	return v1alpha1.SimpleBackendSpec{Image: "ghcr.io/acme/api:v1", Network: net, Ports: ports}
}

// TestRenderNetworkPolicy pins delta 5: ingress default-deny with allows
// shaped by network mode.
func TestRenderNetworkPolicy(t *testing.T) {
	for _, c := range []struct {
		net       v1alpha1.Network
		ports     []int32
		wantRules bool
		wantPeer  any // nil = from anywhere
	}{
		{v1alpha1.NetworkPrivate, []int32{8080}, true, map[string]any{"podSelector": map[string]any{}}},
		{v1alpha1.NetworkPublic, []int32{8080}, true, nil},
		{v1alpha1.NetworkNone, nil, false, nil},
	} {
		t.Run(string(c.net), func(t *testing.T) {
			objs, err := RenderSimpleBackend("api", backend(c.net, c.ports...), ctx)
			if err != nil {
				t.Fatal(err)
			}
			np := byKind(t, objs)["NetworkPolicy"]
			if np == nil {
				t.Fatal("no NetworkPolicy rendered")
			}
			if got := get(np, "spec", "policyTypes"); !reflect.DeepEqual(got, []any{"Ingress"}) {
				t.Errorf("policyTypes = %v; egress must stay unrestricted (it is the namespace's control)", got)
			}
			if got := get(np, "spec", "podSelector", "matchLabels", LabelName); got != "api" {
				t.Errorf("policy selects %v, want the backend's own pods", got)
			}
			rule := get(np, "spec", "ingress", 0)
			if !c.wantRules {
				if rule != nil {
					t.Fatalf("network none must deny all ingress, got rule %v", rule)
				}
				return
			}
			if got := get(rule, "ports", 0, "port"); got != float64(8080) {
				t.Errorf("allowed port = %v, want 8080", got)
			}
			if got := get(rule, "from", 0); !reflect.DeepEqual(got, c.wantPeer) {
				t.Errorf("from = %#v, want %#v", got, c.wantPeer)
			}
		})
	}
}

// TestRenderKeepsSemanticEmptyMaps guards the pruning trap: in Kubernetes an
// empty map is often the meaning. Dropping podSelector:{} widens a private
// backend to allow-from-anywhere; dropping emptyDir:{} leaves a volume with no
// source.
func TestRenderKeepsSemanticEmptyMaps(t *testing.T) {
	objs, err := RenderSimpleBackend("api", backend(v1alpha1.NetworkPrivate, 8080), ctx)
	if err != nil {
		t.Fatal(err)
	}
	k := byKind(t, objs)
	if peer := get(k["NetworkPolicy"], "spec", "ingress", 0, "from", 0); !reflect.DeepEqual(peer, map[string]any{"podSelector": map[string]any{}}) {
		t.Fatalf("private backend's same-namespace peer was lost: %#v", peer)
	}
	if ed := get(k["Deployment"], "spec", "template", "spec", "volumes", 0, "emptyDir"); !reflect.DeepEqual(ed, map[string]any{}) {
		t.Fatalf("/tmp emptyDir source was lost: %#v", ed)
	}
	for kind, o := range k {
		if get(o, "status") != nil || get(o, "metadata", "creationTimestamp") != nil {
			t.Errorf("%s carries server-owned fields", kind)
		}
	}
}

// TestRenderEnvChannels pins the four channels onto the Secrets whose names
// are contracts with other components.
func TestRenderEnvChannels(t *testing.T) {
	spec := backend(v1alpha1.NetworkPrivate, 8080)
	spec.Env = []v1alpha1.EnvVar{
		{Name: "PLAIN", Value: "v"},
		{Name: "EMPTY"},
		{Name: "SREF", SecretRef: &v1alpha1.SecretKeyRef{Name: "s", Key: "k"}},
		{Name: "MANAGED", ManagedSecret: "STRIPE_KEY"},
		{Name: "DB", DatabaseRef: &v1alpha1.DatabaseRef{Name: "orders"}},
		{Name: "DBPASS", DatabaseRef: &v1alpha1.DatabaseRef{Name: "orders", Key: v1alpha1.DatabaseKeyPassword}},
	}
	objs, err := RenderSimpleBackend("api", spec, ctx)
	if err != nil {
		t.Fatal(err)
	}
	env := get(byKind(t, objs)["Deployment"], "spec", "template", "spec", "containers", 0, "env").([]any)
	ref := func(i int) (any, any) {
		return get(env[i], "valueFrom", "secretKeyRef", "name"), get(env[i], "valueFrom", "secretKeyRef", "key")
	}
	if get(env[0], "value") != "v" {
		t.Error("inline value lost")
	}
	if env[1].(map[string]any)["name"] != "EMPTY" || get(env[1], "valueFrom") != nil {
		t.Error("an empty var must render as a bare name")
	}
	if n, k := ref(2); n != "s" || k != "k" {
		t.Errorf("secretRef = %v/%v", n, k)
	}
	if n, k := ref(3); n != v1alpha1.ManagedSecretsSecretName || k != "STRIPE_KEY" {
		t.Errorf("managedSecret = %v/%v, want %s/STRIPE_KEY", n, k, v1alpha1.ManagedSecretsSecretName)
	}
	// CNPG publishes "<cluster>-app" with the whole URI under "uri": the key
	// that needs no $(VAR) composition is the default.
	if n, k := ref(4); n != "orders-app" || k != "uri" {
		t.Errorf("databaseRef = %v/%v, want orders-app/uri", n, k)
	}
	if n, k := ref(5); n != "orders-app" || k != "password" {
		t.Errorf("databaseRef password = %v/%v", n, k)
	}
}

// TestRenderProbes: one HealthCheck drives both probes; an empty path is a TCP
// connect; the timing defaults apply.
func TestRenderProbes(t *testing.T) {
	spec := backend(v1alpha1.NetworkPrivate, 5432)
	spec.HealthCheck = &v1alpha1.HealthCheck{Port: 5432}
	objs, err := RenderSimpleBackend("api", spec, ctx)
	if err != nil {
		t.Fatal(err)
	}
	c := get(byKind(t, objs)["Deployment"], "spec", "template", "spec", "containers", 0)
	for _, probe := range []string{"livenessProbe", "readinessProbe"} {
		if get(c, probe, "tcpSocket", "port") != float64(5432) || get(c, probe, "httpGet") != nil {
			t.Errorf("%s: an empty path must be a TCP connect, got %v", probe, get(c, probe))
		}
		if get(c, probe, "timeoutSeconds") != float64(3) || get(c, probe, "initialDelaySeconds") != float64(5) {
			t.Errorf("%s: forge's timing defaults not applied: %v", probe, get(c, probe))
		}
	}
}

// TestRenderHardening pins the destination-agnostic hardening that now lives in
// Render: non-root, seccomp, drop ALL, read-only root, no token, no Role.
func TestRenderHardening(t *testing.T) {
	spec := backend(v1alpha1.NetworkPublic, 8080)
	spec.StorageGiB = 5
	objs, err := RenderSimpleBackend("api", spec, ctx)
	if err != nil {
		t.Fatal(err)
	}
	k := byKind(t, objs)
	pod := get(k["Deployment"], "spec", "template", "spec")
	c := get(pod, "containers", 0)
	checks := map[string]any{
		"pod runAsNonRoot":        get(pod, "securityContext", "runAsNonRoot"),
		"pod seccomp":             get(pod, "securityContext", "seccompProfile", "type"),
		"pod fsGroup":             get(pod, "securityContext", "fsGroup"),
		"pod automount token":     get(pod, "automountServiceAccountToken"),
		"sa automount token":      get(k["ServiceAccount"], "automountServiceAccountToken"),
		"container readOnlyRoot":  get(c, "securityContext", "readOnlyRootFilesystem"),
		"container privEsc":       get(c, "securityContext", "allowPrivilegeEscalation"),
		"container drop":          get(c, "securityContext", "capabilities", "drop", 0),
		"deployment replicas":     get(k["Deployment"], "spec", "replicas"),
		"pvc access mode":         get(k["PersistentVolumeClaim"], "spec", "accessModes", 0),
		"pvc size":                get(k["PersistentVolumeClaim"], "spec", "resources", "requests", "storage"),
		"pvc mounted at /data":    get(c, "volumeMounts", 1, "mountPath"),
		"service selects backend": get(k["Service"], "spec", "selector", LabelName),
	}
	want := map[string]any{
		"pod runAsNonRoot": true, "pod seccomp": "RuntimeDefault", "pod fsGroup": float64(RunAsUser),
		"pod automount token": false, "sa automount token": false,
		"container readOnlyRoot": true, "container privEsc": false, "container drop": "ALL",
		"deployment replicas": float64(1), "pvc access mode": "ReadWriteOnce", "pvc size": "5Gi",
		"pvc mounted at /data": DataMountPath, "service selects backend": "api",
	}
	for name, w := range want {
		if checks[name] != w {
			t.Errorf("%s = %v, want %v", name, checks[name], w)
		}
	}
	for kind := range k {
		if kind == "Role" || kind == "RoleBinding" {
			t.Errorf("rendered %s: the tier must grant no API access (delta 1)", kind)
		}
	}
	if _, ok := k["Service"]; !ok {
		t.Error("public backend rendered no Service")
	}
	none, _ := RenderSimpleBackend("w", backend(v1alpha1.NetworkNone), ctx)
	if _, ok := byKind(t, none)["Service"]; ok {
		t.Error("network none must render NO Service object")
	}
}

// TestRenderIsDeterministic: same input, byte-identical output, every time.
// Two executors (forge and the operator) call Render against the same objects,
// and a non-deterministic render is a perpetual writer fight. The live
// revision-1855 flap is what that looks like.
func TestRenderIsDeterministic(t *testing.T) {
	spec := backend(v1alpha1.NetworkPublic, 8080, 9090)
	spec.StorageGiB = 3
	spec.Env = []v1alpha1.EnvVar{{Name: "A", Value: "1"}, {Name: "B", ManagedSecret: "B"}}
	first := ""
	for i := 0; i < 50; i++ {
		objs, err := RenderSimpleBackend("api", spec, ctx)
		if err != nil {
			t.Fatal(err)
		}
		db, _ := RenderManagedDatabase("orders", v1alpha1.ManagedDatabaseSpec{}, ctx)
		b, _ := json.Marshal(append(objs, db...))
		if i == 0 {
			first = string(b)
		} else if string(b) != first {
			t.Fatal("Render produced different bytes for the same input")
		}
	}
}

// TestRenderRefusesInvalid: Render validates before emitting anything, so an
// executor that forgot to call Validate still cannot apply a bad spec.
func TestRenderRefusesInvalid(t *testing.T) {
	if _, err := RenderSimpleBackend("api", backend(v1alpha1.NetworkPrivate, 8080), Context{}); err == nil || !strings.Contains(err.Error(), "namespace") {
		t.Errorf("empty namespace: %v", err)
	}
	if _, err := RenderSimpleBackend("Bad_Name", backend(v1alpha1.NetworkPrivate, 8080), ctx); err == nil {
		t.Error("an invalid object name must be refused")
	}
	bad := backend(v1alpha1.NetworkPrivate, 8080)
	bad.Image = "api:latest"
	if _, err := RenderSimpleBackend("api", bad, ctx); err == nil || !strings.Contains(err.Error(), "registry host") {
		t.Errorf("unqualified image: %v", err)
	}
	if _, err := RenderManagedDatabase("orders", v1alpha1.ManagedDatabaseSpec{Instances: 9}, ctx); err == nil {
		t.Error("out-of-range instances must be refused, not clamped")
	}
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
	c := byKind(t, objs)["Cluster"]
	want := map[string]any{
		"spec": map[string]any{
			"instances":             float64(2),
			"enableSuperuserAccess": false,
			"bootstrap":             map[string]any{"initdb": map[string]any{"database": "order_db", "owner": "order_db_app"}},
			"storage":               map[string]any{"size": "50Gi"},
		},
	}
	if !reflect.DeepEqual(c["spec"], want["spec"]) {
		t.Errorf("Cluster spec = %#v\nwant %#v (control-plane's closed shape: no backup stanza, no passthrough)", c["spec"], want["spec"])
	}
	if get(c, "metadata", "namespace") != "acme-prod" || get(c, "metadata", "name") != "order-db" {
		t.Errorf("Cluster placed at %v/%v", get(c, "metadata", "namespace"), get(c, "metadata", "name"))
	}
	// Retain is the default and must be visible to whatever prunes.
	if get(c, "metadata", "annotations", AnnotationDeletionPolicy) != "retain" {
		t.Errorf("deletion policy annotation = %v, want retain", get(c, "metadata", "annotations", AnnotationDeletionPolicy))
	}
	if DatabaseCredentialSecretName("order-db") != "order-db-app" {
		t.Error("CNPG publishes <cluster>-app; DatabaseRef reads it")
	}
}

// TestRenderDispatch: the generic entry point routes each kind, and StaticSite
// renders no Kubernetes objects (its executor is the release planner).
func TestRenderDispatch(t *testing.T) {
	sb := &v1alpha1.SimpleBackend{Spec: backend(v1alpha1.NetworkPrivate, 8080)}
	sb.Name = "api"
	if objs, err := Render(sb, ctx); err != nil || len(objs) == 0 {
		t.Fatalf("SimpleBackend: %v %d", err, len(objs))
	}
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
	if _, err := Render(&v1alpha1.SimpleBackendList{}, ctx); err == nil {
		t.Error("a non-tier object must be refused")
	}
}
