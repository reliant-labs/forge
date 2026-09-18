package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/reliant-labs/forge/internal/kclplugin"
	"github.com/reliant-labs/forge/internal/kclrender"
)

// TestSidecarRendersResources pins the rule that EVERY container forge renders
// carries cpu/memory requests and limits — sidecars and init containers
// included, whether or not the author declared any.
//
// THE BUG THIS CATCHES: `forge.Container` had no `resources` field at all, and
// `_render_extra_container` emitted no resources block, so a sidecar was always
// BestEffort. Two consequences, neither of which names the sidecar when it
// bites:
//
//  1. BestEffort is the first thing the kubelet evicts under node pressure, and
//     it carries no CPU guarantee. When the sidecar IS the database path — a
//     cloud-sql-proxy — the symptom is connection errors inside the app, so the
//     investigation starts in the application code and not in the proxy.
//  2. k8s computes pod QoS across ALL containers, so ONE request-less sidecar
//     demoted the entire pod even though the primary container was sized
//     correctly. Sizing the primary looked like it worked and did nothing.
//
// Both halves are asserted, because they fail independently: a declared
// Resources must round-trip to the rendered quantities, and an UNDECLARED one
// must still fall back to forge's defaults rather than rendering nothing. The
// fallback is the half that regresses silently — nothing about a missing
// resources block looks wrong in a diff.
func TestSidecarRendersResources(t *testing.T) {
	moduleRoot := forgeModuleRoot(t)
	dir := t.TempDir()

	kclMod := "[package]\nname = \"sidecarres\"\nedition = \"v0.11.0\"\nversion = \"0.0.1\"\n\n[dependencies]\nforge = { path = \"" + moduleRoot + "\" }\n"
	if err := os.WriteFile(filepath.Join(dir, "kcl.mod"), []byte(kclMod), 0o644); err != nil {
		t.Fatal(err)
	}

	// `sized` declares Resources explicitly; `unsized` declares none and must
	// still render the defaults. The init container covers the same path,
	// since initContainers run through _render_extra_container too.
	main := `import forge

_svc = forge.Service {
    name = "api"
    image = "api"
    ports = [8080]
    sidecars = [
        forge.Container {
            name = "sized"
            image = "gcr.io/cloud-sql-connectors/cloud-sql-proxy:2.14.1"
            resources = forge.Resources {
                cpu_request_millicores = forge.mcpu(200)
                cpu_limit_millicores = forge.mcpu(200)
                memory_request_bytes = forge.mib(256)
                memory_limit_bytes = forge.mib(256)
            }
        }
        forge.Container {
            name = "unsized"
            image = "redis:7"
        }
    ]
    init = [
        forge.Container {
            name = "migrate"
            image = "migrate:1"
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

	rendered := map[string]map[string]any{}
	for _, obj := range m.Manifests {
		if kind, _ := obj["kind"].(string); kind != "Deployment" {
			continue
		}
		spec, _ := obj["spec"].(map[string]any)
		tmpl, _ := spec["template"].(map[string]any)
		podSpec, _ := tmpl["spec"].(map[string]any)
		for _, key := range []string{"containers", "initContainers"} {
			list, _ := podSpec[key].([]any)
			for _, c := range list {
				cm, _ := c.(map[string]any)
				name, _ := cm["name"].(string)
				rendered[name] = cm
			}
		}
	}
	if len(rendered) == 0 {
		t.Fatalf("no Deployment containers in render:\n%s", out)
	}

	// forge's Resources defaults: 100m/500m CPU, 128Mi/512Mi memory.
	for _, tc := range []struct {
		container                      string
		cpuReq, cpuLim, memReq, memLim string
		why                            string
	}{
		{
			container: "sized",
			cpuReq:    "200m", cpuLim: "200m", memReq: "256Mi", memLim: "256Mi",
			why: "a declared Resources round-trips to the rendered quantities",
		},
		{
			container: "unsized",
			cpuReq:    "100m", cpuLim: "500m", memReq: "128Mi", memLim: "512Mi",
			why: "an UNDECLARED sidecar falls back to defaults, never to no block",
		},
		{
			container: "migrate",
			cpuReq:    "100m", cpuLim: "500m", memReq: "128Mi", memLim: "512Mi",
			why: "init containers render through the same path as sidecars",
		},
	} {
		ctr := rendered[tc.container]
		if ctr == nil {
			t.Errorf("container %q missing from render", tc.container)
			continue
		}
		res, ok := ctr["resources"].(map[string]any)
		if !ok {
			t.Errorf("container %q rendered NO resources block — that is BestEffort, and it drags the whole pod's QoS down (%s)", tc.container, tc.why)
			continue
		}
		requests, _ := res["requests"].(map[string]any)
		limits, _ := res["limits"].(map[string]any)
		for _, got := range []struct {
			field string
			have  any
			want  string
		}{
			{"requests.cpu", requests["cpu"], tc.cpuReq},
			{"requests.memory", requests["memory"], tc.memReq},
			{"limits.cpu", limits["cpu"], tc.cpuLim},
			{"limits.memory", limits["memory"], tc.memLim},
		} {
			if s, _ := got.have.(string); s != got.want {
				t.Errorf("container %q %s = %v, want %q (%s)", tc.container, got.field, got.have, got.want, tc.why)
			}
		}
	}
}
