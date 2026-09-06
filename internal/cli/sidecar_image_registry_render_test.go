package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/reliant-labs/forge/internal/kclplugin"
	"github.com/reliant-labs/forge/internal/kclrender"
)

// TestSidecarImageKeepsItsOwnRegistry pins the rule that an image naming its
// OWN registry host is never prefixed with the environment's registry.
//
// THE BUG THIS CATCHES: a third-party sidecar — cloud-sql-proxy, an OTel
// collector — is pulled from its vendor's registry, not from the env's. The
// image resolver prefixed the env registry onto any image that carried a tag,
// without first asking whether the image already named a registry, producing:
//
//	us-central1-docker.pkg.dev/proj/repo/gcr.io/cloud-sql-connectors/cloud-sql-proxy:2.14.1
//
// That reference cannot be pulled. It renders and deploys cleanly, then fails
// at runtime as ImagePullBackOff — the expensive place to find out.
//
// The primary container is asserted alongside it: a BARE image name still
// takes the env registry, which is the behavior the prefixing exists for and
// the thing a naive fix would break.
func TestSidecarImageKeepsItsOwnRegistry(t *testing.T) {
	moduleRoot := forgeModuleRoot(t)
	dir := t.TempDir()

	kclMod := "[package]\nname = \"sidecarimg\"\nedition = \"v0.11.0\"\nversion = \"0.0.1\"\n\n[dependencies]\nforge = { path = \"" + moduleRoot + "\" }\n"
	if err := os.WriteFile(filepath.Join(dir, "kcl.mod"), []byte(kclMod), 0o644); err != nil {
		t.Fatal(err)
	}

	// One service with a bare primary image and three sidecars covering the
	// registry-host forms that must be passed through verbatim.
	main := `import forge

_svc = forge.Service {
    name = "api"
    image = "api"
    ports = [8080]
    sidecars = [
        forge.Container {
            name = "cloud-sql-proxy"
            image = "gcr.io/cloud-sql-connectors/cloud-sql-proxy:2.14.1"
            args = ["--address=127.0.0.1", "--port=5432"]
        }
        forge.Container {
            name = "port-registry"
            image = "localhost:5050/vendor/thing:1.0"
        }
        forge.Container {
            name = "bare-sidecar"
            image = "redis:7"
        }
    ]
}

_bundle = forge.Bundle {
    workloads = [_svc]
    cluster_target = forge.ClusterTarget {
        cluster = "test-cluster"
        namespace = "testns"
        registry = "reg.example.com/proj"
    }
}

manifests = forge.render_manifests(_bundle, "v9")
`
	if err := os.WriteFile(filepath.Join(dir, "main.k"), []byte(main), 0o644); err != nil {
		t.Fatal(err)
	}

	kclplugin.Register()

	out, err := kclrender.Run(dir, dir, nil)
	if err != nil {
		t.Fatalf("render service with sidecars: %v", err)
	}

	var m struct {
		Manifests []map[string]any `json:"manifests"`
	}
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("unmarshal render: %v\n%s", err, out)
	}

	images := map[string]string{}
	rendered := map[string]map[string]any{}
	for _, obj := range m.Manifests {
		if kind, _ := obj["kind"].(string); kind != "Deployment" {
			continue
		}
		spec, _ := obj["spec"].(map[string]any)
		tmpl, _ := spec["template"].(map[string]any)
		podSpec, _ := tmpl["spec"].(map[string]any)
		containers, _ := podSpec["containers"].([]any)
		for _, c := range containers {
			cm, _ := c.(map[string]any)
			name, _ := cm["name"].(string)
			img, _ := cm["image"].(string)
			images[name] = img
			rendered[name] = cm
		}
	}

	// A sidecar's args must render as k8s `args`, NEVER folded into `command`.
	// Folding overrides the image's entrypoint, so the flags themselves become
	// the argv and the container dies at startup with
	//   exec: "--address=127.0.0.1": executable file not found in $PATH
	// — which only shows up once the pod is scheduled in a real cluster.
	proxy := rendered["cloud-sql-proxy"]
	if proxy == nil {
		t.Fatalf("cloud-sql-proxy container missing from render")
	}
	if _, hasCommand := proxy["command"]; hasCommand {
		t.Errorf("sidecar rendered a `command` (%v) — that REPLACES the image entrypoint; args belong in `args`", proxy["command"])
	}
	gotArgs, _ := proxy["args"].([]any)
	wantArgs := []string{"--address=127.0.0.1", "--port=5432"}
	if len(gotArgs) != len(wantArgs) {
		t.Fatalf("sidecar args = %v, want %v", proxy["args"], wantArgs)
	}
	for i, w := range wantArgs {
		if s, _ := gotArgs[i].(string); s != w {
			t.Errorf("sidecar args[%d] = %q, want %q", i, s, w)
		}
	}
	if len(images) == 0 {
		t.Fatalf("no Deployment containers in render:\n%s", out)
	}

	for _, tc := range []struct {
		container string
		want      string
		why       string
	}{
		{
			container: "api",
			want:      "reg.example.com/proj/api:v9",
			why:       "a bare image still takes the env registry and tag",
		},
		{
			container: "cloud-sql-proxy",
			want:      "gcr.io/cloud-sql-connectors/cloud-sql-proxy:2.14.1",
			why:       "a dotted registry host means the image is third-party",
		},
		{
			container: "port-registry",
			want:      "localhost:5050/vendor/thing:1.0",
			why:       "a host:port registry is a registry host too",
		},
		{
			container: "bare-sidecar",
			want:      "reg.example.com/proj/redis:7",
			why:       "a sidecar with NO registry host is still an env image",
		},
	} {
		got, ok := images[tc.container]
		if !ok {
			t.Errorf("container %q missing from render (got %v)", tc.container, images)
			continue
		}
		if got != tc.want {
			t.Errorf("container %q image =\n  %s\nwant\n  %s\n(%s)", tc.container, got, tc.want, tc.why)
		}
	}
}
