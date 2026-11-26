//go:build e2e

package cli

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/siteaudit"
)

// TestE2ESiteAuditJudgesAFreshSiteBuild is `forge site audit` end to end
// (Site insights, card F2): a site `forge project new --kind site` just wrote
// is built with its own `npm run build`, and the bare `forge site audit` in
// the project finds that build through the env's public_dir, serves it
// in-process and prints the v1 report document. The scaffold's own quality
// (no error findings, SEO ≥ 90) is card F5's to assert; this test pins that
// the command resolves, crawls and judges a real build: the root answers 200
// and carries an SEO score.
func TestE2ESiteAuditJudgesAFreshSiteBuild(t *testing.T) {
	t.Parallel() // its own project; binary shared via sync.Once
	requireTool(t, "node", "npm")
	forgeBin := buildforgeBinary(t)

	root := t.TempDir()
	runCmdTimeout(t, root, 5*time.Minute, forgeBin, "project", "new", "auditsite", "--kind", "site")
	project := filepath.Join(root, "auditsite")
	runCmdTimeout(t, project, 10*time.Minute, "npm", "install", "--no-audit", "--no-fund", "--prefer-offline")
	runCmdTimeout(t, project, 10*time.Minute, "npm", "run", "build")

	cmd := exec.Command(forgeBin, "site", "audit", "--json")
	cmd.Dir = project
	cmd.Env = os.Environ()
	out, err := cmd.Output()
	var exit *exec.ExitError
	if err != nil && (!errors.As(err, &exit) || exit.ExitCode() != exitWrong) {
		t.Fatalf("forge site audit: %v (want exit 0 or 1)\n%s", err, out)
	}
	var rep siteaudit.Report
	if err := json.Unmarshal(out, &rep); err != nil {
		t.Fatalf("forge site audit --json is not the report document: %v\n%s", err, out)
	}
	if rep.RulesVersion != siteaudit.RulesVersion {
		t.Errorf("rules_version = %q, want %q", rep.RulesVersion, siteaudit.RulesVersion)
	}
	var home *siteaudit.RouteReport
	for i := range rep.Routes {
		if rep.Routes[i].Route == "/" {
			home = &rep.Routes[i]
		}
	}
	if home == nil || home.Status != 200 || home.Score.SEO == nil {
		t.Fatalf("the site's root was not judged: %+v\n%s", home, out)
	}
	errorsFound := 0
	for _, f := range rep.Findings {
		if f.Severity == siteaudit.SeverityError {
			errorsFound++
		}
	}
	if (errorsFound > 0) != (err != nil) {
		t.Errorf("exit status (err %v) disagrees with %d error findings", err, errorsFound)
	}
}
