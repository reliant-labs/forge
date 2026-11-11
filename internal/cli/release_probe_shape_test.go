package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/kclplugin"
	"github.com/reliant-labs/forge/internal/kclrender"
	"github.com/reliant-labs/forge/pkg/release"
)

// renderReleaseProbeEnv renders one env through the seam `forge env shape`
// uses. bundle is the body of a forge.Bundle literal. It returns the decoded
// entities, or the render's error.
func renderReleaseProbeEnv(t *testing.T, bundle string) (*KCLEntities, error) {
	t.Helper()
	e, _, err := renderReleaseProbeRaw(t, bundle)
	return e, err
}

// renderReleaseProbeRaw is renderReleaseProbeEnv plus the render's document.
func renderReleaseProbeRaw(t *testing.T, bundle string) (*KCLEntities, []byte, error) {
	t.Helper()
	if testing.Short() {
		t.Skip("-short: renders KCL; runs in task test")
	}
	dir := t.TempDir()
	files := map[string]string{
		"kcl.mod": "[package]\nname = \"probes\"\nedition = \"v0.11.0\"\nversion = \"0.0.1\"\n",
		"main.k": `import forge
import forge.workloads as fw

output = forge.render(forge.Bundle {
    project = "demo"
` + bundle + `
})
`,
	}
	for f, c := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(c), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	kclplugin.Register()
	out, err := kclrender.Run(dir, dir, nil)
	if err != nil {
		return nil, nil, err
	}
	e, err := parseKCLEntities(out)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return e, out, nil
}

const (
	probeSite = `
    frontends = [forge.Frontend {
        name = "web"
        image = "ghcr.io/acme/web"
        path = "web"
        type = "vite"
        public_dir = "dist"
        runtime = forge.OnHosted {}
    }]`
	probeService = `
    workloads = [fw.Workload {
        name = "api"
        image = "ghcr.io/acme/api"
        ports = [fw.Port {name = "http", port = 8080, expose = True}]
        %s
        runtime = forge.OnHosted {}
    }]`
)

func probeServiceWith(decl string) string { return strings.Replace(probeService, "%s", decl, 1) }

// probeShape renders bundle and projects its release probes into a shape that
// must pass the shape's own validation: what KCL accepts, pkg/release accepts.
func probeShape(t *testing.T, bundle string) []release.ShapeReleaseProbe {
	t.Helper()
	e, err := renderReleaseProbeEnv(t, bundle)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got := shapeReleaseProbesOf(e)
	if err := (release.Shape{Kind: release.EnvPersistent, ReleaseProbes: got}).Canonical().Validate(); err != nil {
		t.Errorf("release probes KCL rendered must satisfy the shape's validation: %v\n%+v", err, got)
	}
	return got
}

// KCL -> render contract -> shape field -> Shape.Validate.
func TestReleaseProbesReachTheShape(t *testing.T) {
	cp := "    control_plane = forge.ControlPlane {}\n"

	t.Run("a hosted static site with no declaration is probed at its root", func(t *testing.T) {
		got := probeShape(t, cp+probeSite)
		want := []release.ShapeReleaseProbe{{
			Target: "web", Kind: release.ReleaseProbeKindFrontend, Paths: []string{"/"}, Method: "GET",
			RatePerMin: 4, TimeoutSeconds: 10, Source: release.ReleaseProbeSourceDefault,
		}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("default probe = %+v, want %+v", got, want)
		}
	})

	t.Run("a hosted service with no declaration is not probed", func(t *testing.T) {
		if got := probeShape(t, cp+probeServiceWith("")); got != nil {
			t.Errorf("an undeclared service was probed: %+v", got)
		}
	})

	t.Run("a hosted service's declaration is recorded as written", func(t *testing.T) {
		got := probeShape(t, cp+probeServiceWith(`release_probe = forge.Probe {paths = ["/ok", "/flow-health"], method = "HEAD", rate_per_min = 2, timeout = "20s", expect_status = ["204"]}`))
		want := []release.ShapeReleaseProbe{{
			Target: "api", Kind: release.ReleaseProbeKindWorkload, Paths: []string{"/ok", "/flow-health"}, Method: "HEAD",
			RatePerMin: 2, TimeoutSeconds: 20, ExpectStatus: []string{"204"}, Source: release.ReleaseProbeSourceUser,
		}}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("declared probe = %+v, want %+v", got, want)
		}
	})

	t.Run("an env the control plane does not probe records nothing", func(t *testing.T) {
		for name, bundle := range map[string]string{
			"local lifecycle":     cp + "    lifecycle = \"local\"\n" + probeSite,
			"ephemeral lifecycle": cp + "    lifecycle = \"ephemeral\"\n" + probeSite,
		} {
			e, err := renderReleaseProbeEnv(t, bundle)
			if err != nil {
				t.Fatalf("%s: render: %v", name, err)
			}
			if got := shapeReleaseProbesOf(e); got != nil {
				t.Errorf("%s: probed %+v", name, got)
			}
			raw, err := (release.Shape{Kind: release.EnvPersistent, ReleaseProbes: shapeReleaseProbesOf(e)}).Canonical().Encode()
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "release_probes") {
				t.Errorf("%s: a shape with no probes encodes the key, so it no longer hashes as it did before the field: %s", name, raw)
			}
		}
	})
}

// An invalid declaration fails the render, naming the rule, before anything
// is built or recorded.
func TestReleaseProbeInvalidDeclarationsFailTheRender(t *testing.T) {
	cp := "    control_plane = forge.ControlPlane {}\n"
	for name, tc := range map[string]struct{ decl, want string }{
		"POST":               {`release_probe = forge.Probe {method = "POST"}`, "POST"},
		"too many paths":     {`release_probe = forge.Probe {paths = ["/a", "/b", "/c", "/d", "/e", "/f"]}`, "name 1 to 5 paths, not 6"},
		"rate above the cap": {`release_probe = forge.Probe {rate_per_min = 13}`, "rate_per_min 13 must be 1 to 12"},
		"rate of zero":       {`release_probe = forge.Probe {rate_per_min = 0}`, "rate_per_min 0 must be 1 to 12"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := renderReleaseProbeEnv(t, cp+probeServiceWith(tc.decl))
			if err == nil {
				t.Fatalf("rendered %s", tc.decl)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("render error does not name %q:\n%v", tc.want, err)
			}
		})
	}
}

// The KCL render emits each probe in pkg/release's own JSON vocabulary: every
// key ShapeReleaseProbe declares, and none it does not. A key renamed on one
// side fails here rather than decoding to a zero field.
func TestReleaseProbeRenderKeysMatchTheShape(t *testing.T) {
	_, out, err := renderReleaseProbeRaw(t, "    control_plane = forge.ControlPlane {}\n"+
		probeServiceWith(`release_probe = forge.Probe {expect_status = ["200"]}`))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Output struct {
			ReleaseProbes []json.RawMessage `json:"release_probes"`
		} `json:"output"`
	}
	if err := json.Unmarshal(out, &doc); err != nil || len(doc.Output.ReleaseProbes) != 1 {
		t.Fatalf("render has no single release probe (%v):\n%s", err, out)
	}
	raw := doc.Output.ReleaseProbes[0]
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&release.ShapeReleaseProbe{}); err != nil {
		t.Errorf("rendered probe carries a key ShapeReleaseProbe does not: %v\n%s", err, raw)
	}
	var keys map[string]any
	if err := json.Unmarshal(raw, &keys); err != nil {
		t.Fatal(err)
	}
	typ := reflect.TypeOf(release.ShapeReleaseProbe{})
	for i := 0; i < typ.NumField(); i++ {
		key, _, _ := strings.Cut(typ.Field(i).Tag.Get("json"), ",")
		if _, ok := keys[key]; !ok {
			t.Errorf("rendered probe has no %q: %s", key, raw)
		}
	}
}
