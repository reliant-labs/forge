package templates_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/buildinfo"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
	"github.com/reliant-labs/forge/internal/kclrender"
	"github.com/reliant-labs/forge/internal/kclvendor"
)

// TestReleaseBuildScaffoldResolvesAndRenders is the regression guard for
// the bug that shipped: a RELEASE build of forge scaffolded a project
// whose KCL dependency could not be resolved at all.
//
// Every test that existed when that shipped asserted the dependency
// STRING — that kcl.mod carried the expected `tag = "kcl-v0.1.0"` line —
// and each of them passed, because the line was written exactly as
// intended. The tag had simply never been published. A string assertion
// cannot tell the difference between a dependency that resolves and one
// that does not, so this test resolves it instead: it renders the
// scaffold through forge's own evaluation seam and requires real
// manifests to come out the other end.
//
// It also pins the release/dev question closed. The scaffold is produced
// with a stamped pkg version — the exact discriminator that used to
// select the broken path — so if a release-only branch is ever
// reintroduced here, this fails.
//
// NOT parallel: buildinfo is process-global, and the test asserts the
// offline claim by breaking git for its own duration.
func TestReleaseBuildScaffoldResolvesAndRenders(t *testing.T) {
	// Build the project the way a released forge binary does.
	buildinfo.SetPkgVersion("v9.9.9")
	defer buildinfo.SetPkgVersion("")

	// Offline, air-gapped, no credentials. A git-tag dependency needs the
	// network and git auth at render time; the vendored copy needs
	// neither, and this is where that claim is actually tested. `none` is
	// not a protocol git will ever allow, so any attempt to fetch a
	// remote dependency fails here instead of silently succeeding on a
	// machine that happens to have network and a cached credential.
	t.Setenv("GIT_ALLOW_PROTOCOL", "none")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")

	tmp := t.TempDir()
	g := generator.NewProjectGenerator("rel-render", tmp, "example.com/rel-render")
	g.Kind = config.ProjectKindService
	g.ApplyKindFeatureDefaults(config.ProjectKindService)
	if err := g.Generate(); err != nil {
		t.Fatalf("Generate: %v", err)
	}

	// A release scaffold must be born resolvable: the module materialized
	// and the dependency pointing at it. Not a string check for its own
	// sake — it localizes the failure when the render below breaks.
	kclModPath := filepath.Join(tmp, "deploy", "kcl", "kcl.mod")
	kclMod, err := os.ReadFile(kclModPath)
	if err != nil {
		t.Fatalf("read deploy/kcl/kcl.mod: %v", err)
	}
	if strings.Contains(string(kclMod), "git = ") {
		t.Fatalf("release scaffold emits a git dependency — it must be born vendored:\n%s", kclMod)
	}
	if !kclvendor.Present(tmp) {
		t.Fatalf("release scaffold did not materialize %s/", kclvendor.VendorDirName)
	}
	if stale, stamped := kclvendor.Stale(tmp); stale {
		t.Errorf("freshly scaffolded vendor copy reports stale (stamp %q)", stamped)
	}

	// The pipeline-generated config trio, which a bare Generate has not
	// produced yet. Same stubs as TestScaffoldedIngressEvaluates.
	configGenStub := `import forge

schema AppConfig:
    port: int = 8080

appConfigEnvMap = lambda c: AppConfig -> {str: forge.EnvSource} {
    {"PORT" = {value = str(c.port)}}
}
`
	if err := os.WriteFile(filepath.Join(tmp, "deploy/kcl", "config_gen.k"), []byte(configGenStub), 0644); err != nil {
		t.Fatalf("write config_gen.k stub: %v", err)
	}
	for _, env := range []string{"dev", "staging", "prod"} {
		configStub := `import config_gen

app_config: config_gen.AppConfig = {
}
`
		if err := os.WriteFile(filepath.Join(tmp, "deploy/kcl", env, "config.k"), []byte(configStub), 0644); err != nil {
			t.Fatalf("write %s config.k stub: %v", env, err)
		}
	}

	// THE ASSERTION THAT WOULD HAVE CAUGHT THIS: resolve the dependency
	// and render. Every env, because a per-env kcl.mod depth mistake only
	// shows in the env that has it.
	for _, env := range []string{"dev", "staging", "prod"} {
		out, err := kclrender.Run(tmp, filepath.Join(tmp, "deploy/kcl", env), []string{"env=" + env})
		if err != nil {
			t.Fatalf("render %s env from a release-build scaffold: %v", env, err)
		}
		var doc map[string]any
		if err := json.Unmarshal(out, &doc); err != nil {
			t.Fatalf("unmarshal %s render: %v\n%s", env, err, out)
		}
		// Rendering "successfully" to nothing would satisfy a weaker
		// check, so require the manifests the deploy path consumes.
		manifests, ok := doc["manifests"].([]any)
		if !ok || len(manifests) == 0 {
			t.Fatalf("%s render produced no manifests:\n%s", env, out)
		}
		if _, ok := doc["output"].(map[string]any); !ok {
			t.Fatalf("%s render has no `output` contract:\n%s", env, out)
		}
	}
}
