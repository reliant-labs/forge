package cli

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/buildlog"
)

// A writing deploy runs its build steps quietly; --verbose and the previews
// stream as they always did.
func TestBeginDeployBuildLog(t *testing.T) {
	t.Chdir(t.TempDir())
	for _, f := range []deployCmdFlags{{verbose: true}, {dryRun: true}, {explain: true}, {promote: promoteCmdFlags{plan: true}}, {promote: promoteCmdFlags{planOnly: true}}} {
		if s := beginDeployBuildLog("prod", f); s != nil {
			_ = s.End()
			t.Errorf("%+v started a quiet session; it must stream", f)
		}
	}
	s := beginDeployBuildLog("prod", deployCmdFlags{})
	if s == nil {
		t.Fatal("a writing deploy started no quiet session")
	}
	defer func() { _ = s.End() }()
	if buildlog.Active() != s {
		t.Error("the session is not the active one: build steps would still stream")
	}
	if !strings.HasSuffix(s.Path(), filepath.Join(".forge", "logs", "prod", "deploy.log")) {
		t.Errorf("log path = %s, want .forge/logs/prod/deploy.log", s.Path())
	}
}
