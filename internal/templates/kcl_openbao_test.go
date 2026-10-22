package templates

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/deploy/openbao"
)

// TestOpenBaoGrantsWhatTheSecretStoreAsksFor closes the loop between the two
// halves of the platform-secrets render. kcl/lib/external_secret.k renders the
// SecretStore a namespace logs in with (mount, role, audience, server, CA,
// KV path); kcl/lib/openbao.k lowers the component whose converge Job creates
// what that login needs. They are written separately, so this renders ONE
// Bundle containing both, expands the OpenBao record through the real Go
// renderer, and requires the access document to contain exactly the role the
// SecretStore names: on the mount it names, bound to the audience it requests,
// with a policy granting the path its ExternalSecret reads. A mismatch is a
// namespace that silently syncs nothing.
func TestOpenBaoGrantsWhatTheSecretStoreAsksFor(t *testing.T) {
	if testing.Short() {
		t.Skip("renders KCL; runs in task test")
	}
	t.Parallel()
	requireKCLRender(t)

	const main = `import forge

_hub = forge.ClusterTarget {cluster = "gke_acme_us-central1_prod", namespace = "acme-prod", platform = "amd64", connected_cluster = "hub"}

output = forge.render(forge.Bundle {
    project = "acme"
    env = "prod"
    cluster_target = _hub
    openbao = forge.OpenBao {}
    external_secrets_operators = [forge.ExternalSecretsOperator {cluster = _hub}]
    bootstrap_source = forge.GCPSecretManager {project = "acme-prod"}
    required_secrets = [
        forge.ExternalSecret {
            name = "stripe"
            namespace = "payments"
            keys = ["key"]
            source = forge.StoreSource {
                store = forge.OpenBaoStore {}
                data = {"key" = "STRIPE_KEY"}
            }
        }
        forge.ExternalSecret {
            name = "db"
            namespace = "orders"
            keys = ["url"]
            source = forge.StoreSource {
                store = forge.OpenBaoStore {}
                data = {"url" = "DB_URL"}
            }
        }
    ]
})
`
	dir := t.TempDir()
	for name, body := range map[string]string{
		"kcl.mod": "[package]\nname = \"openbao_probe\"\nedition = \"v0.11.0\"\nversion = \"0.0.1\"\n",
		"main.k":  main,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	out, err := runKCL(t, filepath.Join(dir, "main.k"))
	if err != nil {
		t.Fatalf("render: %s", kclDiagnostics(err))
	}
	var doc struct {
		Output struct {
			Manifests []map[string]any `json:"manifests"`
			OpenBao   struct {
				ServerURL   string `json:"server_url"`
				CAConfigMap string `json:"ca_configmap"`
				AuthMount   string `json:"auth_mount"`
				Audience    string `json:"audience"`
			} `json:"openbao"`
		} `json:"output"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		t.Fatal(err)
	}

	var spec openbao.Spec
	stores := map[string]map[string]any{}
	keys := map[string]string{}
	for _, m := range doc.Output.Manifests {
		md, _ := m["metadata"].(map[string]any)
		ns, _ := md["namespace"].(string)
		switch m["kind"] {
		case openbao.Kind:
			raw, _ := json.Marshal(m["spec"])
			dec := json.NewDecoder(strings.NewReader(string(raw)))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&spec); err != nil {
				t.Fatalf("the Go renderer cannot read the record KCL emits: %v", err)
			}
		case "SecretStore":
			stores[ns] = m["spec"].(map[string]any)
		case "ExternalSecret":
			data := m["spec"].(map[string]any)["data"].([]any)[0].(map[string]any)
			keys[ns] = data["remoteRef"].(map[string]any)["key"].(string)
		}
	}
	if spec.Name == "" {
		t.Fatal("no OpenBao record in the stream")
	}
	if len(stores) != 2 {
		t.Fatalf("want a SecretStore in each of payments and orders, got %v", stores)
	}

	access, err := openbao.BuildAccess(spec)
	if err != nil {
		t.Fatal(err)
	}
	roles := map[string]openbao.Role{}
	for _, r := range access.Roles {
		roles[r.Mount+"/"+r.Name] = r
	}
	policies := map[string]openbao.Policy{}
	for _, p := range access.Policies {
		policies[p.Name] = p
	}

	for ns, st := range stores {
		vault := st["provider"].(map[string]any)["vault"].(map[string]any)
		jwt := vault["auth"].(map[string]any)["jwt"].(map[string]any)
		mount, role := jwt["path"].(string), jwt["role"].(string)

		if mount != doc.Output.OpenBao.AuthMount {
			t.Errorf("%s: the SecretStore logs in on %q, the component's resolved mount is %q", ns, mount, doc.Output.OpenBao.AuthMount)
		}
		r, ok := roles[mount+"/"+role]
		if !ok {
			t.Fatalf("%s: the SecretStore logs in as %s/%s, which the access document does not create (it has %v)", ns, mount, role, keysOf(roles))
		}
		aud := jwt["kubernetesServiceAccountToken"].(map[string]any)["audiences"].([]any)[0].(string)
		if got := r.Payload["bound_audiences"].([]string)[0]; got != aud {
			t.Errorf("%s: the SecretStore requests audience %q, the role requires %q", ns, aud, got)
		}
		if aud != doc.Output.OpenBao.Audience {
			t.Errorf("%s: audience %q differs from the resolved block's %q", ns, aud, doc.Output.OpenBao.Audience)
		}
		if want := "system:serviceaccount:" + ns + ":reliant-secrets"; r.Payload["bound_subject"] != want {
			t.Errorf("%s: role subject %v, want %s", ns, r.Payload["bound_subject"], want)
		}
		if vault["server"] != doc.Output.OpenBao.ServerURL {
			t.Errorf("%s: the SecretStore dials %v, the component serves %q", ns, vault["server"], doc.Output.OpenBao.ServerURL)
		}
		if ca := vault["caProvider"].(map[string]any); ca["name"] != doc.Output.OpenBao.CAConfigMap || ca["type"] != "ConfigMap" {
			t.Errorf("%s: the SecretStore trusts %v, the component publishes ConfigMap %q", ns, ca, doc.Output.OpenBao.CAConfigMap)
		}
		read := vault["path"].(string) + "/data/" + keys[ns]
		granted := false
		for _, rule := range policies[role].Rules {
			if rule.Path == read && len(rule.Capabilities) == 1 && rule.Capabilities[0] == "read" {
				granted = true
			}
		}
		if !granted {
			t.Errorf("%s: ESO reads %q, but policy %s grants %v", ns, read, role, policies[role].Rules)
		}
		if !slices.Contains(spec.CANamespaces, ns) || !slices.Contains(spec.IngressNamespaces, ns) {
			t.Errorf("%s: holds a SecretStore but is not given the CA / admitted by the NetworkPolicy (%v / %v)", ns, spec.CANamespaces, spec.IngressNamespaces)
		}
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
