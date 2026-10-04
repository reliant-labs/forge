package cli

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestClientApplyCallSitesAreGuarded fails when a new non-test file in the CLI
// reaches the cluster apply pipeline. Every such file must be on the allowlist
// below; deploy.go and dev_cluster.go must call refuseDirectApply.
func TestClientApplyCallSitesAreGuarded(t *testing.T) {
	t.Parallel()
	allowed := map[string]string{
		"deploy.go":          "runDeploy calls refuseDirectApply before any apply",
		"dev_cluster.go":     "reload calls refuseDirectApply; platform charts are bootstrap",
		"deploy_flux.go":     "the Flux pointer writer",
		"deploy_dispatch.go": "dispatches groups runDeploy has already guarded",
		"platform_charts.go": "cluster bootstrap charts",

		"deploy_promote_follow.go": "bootstrap chart install; its other apply runs through runDeploy, which refuses",
	}
	call := regexp.MustCompile(`\bcluster\.Apply\(|cluster\.ApplyOpts\{|ApplyOptsBuilder:|flux\.Apply|installPlatformCharts\(`)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		body, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if call.Match(body) {
			if _, ok := allowed[f]; !ok {
				t.Errorf("%s reaches the client-side apply pipeline but is not on the allowlist: "+
					"non-local envs must deploy bundle -> record -> Flux (see refuseDirectApply)", f)
			}
		}
	}
	for _, f := range []string{"deploy.go", "dev_cluster.go"} {
		body, _ := os.ReadFile(f)
		if !strings.Contains(string(body), "refuseDirectApply(") {
			t.Errorf("%s applies to clusters but does not call refuseDirectApply", f)
		}
	}
}
