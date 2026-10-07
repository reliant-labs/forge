package codegen

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/reliant-labs/forge/internal/templates"
	"github.com/reliant-labs/forge/pkg/observe"
)

// The success-log sampling wire has TWO ends and they have to agree.
//
//   - proto/config/v1/config.proto (config.proto.tmpl) declares
//     log_success_sample_window. It is what types the value, validates it at
//     startup, and generates the KCL schema + env projection a deployment
//     sets it through (deploy/kcl/<env>/config.k).
//   - forge/pkg/observe reads the variable that projection writes,
//     observe.SuccessSampleWindowEnv, when it builds a logging layer — the
//     only channel that reaches an app's owned serve.go and observe_chain.go
//     seams without editing them.
//
// A drift between the two names is silent: the field validates, the
// manifest carries the variable, and nothing reads it. So they are pinned
// together here, along with the two properties the declaration promises:
// the default logs every success, and there is no flag (observe never
// sees one, so a flag would be a setting that does nothing).
func TestScaffoldLogSuccessSampleWindowMatchesObserve(t *testing.T) {
	out, err := templates.ProjectTemplates().Render("config.proto.tmpl", struct{ Module string }{Module: "example.com/proj"})
	if err != nil {
		t.Fatalf("render config.proto.tmpl: %v", err)
	}
	proto := string(out)
	idx := strings.Index(proto, "google.protobuf.Duration log_success_sample_window = ")
	if idx < 0 {
		t.Fatal("the scaffolded config.proto declares no log_success_sample_window Duration field — " +
			"a deployment then has no typed, per-environment way to turn success sampling on")
	}
	rest := proto[idx:]
	end := strings.Index(rest, "}]")
	if end < 0 {
		t.Fatal("log_success_sample_window has an unterminated option block")
	}
	block := rest[:end]

	if want := `env_var: "` + observe.SuccessSampleWindowEnv + `"`; !strings.Contains(block, want) {
		t.Fatalf("log_success_sample_window is not bound to %s, the variable forge/pkg/observe reads:\n%s",
			observe.SuccessSampleWindowEnv, block)
	}
	if strings.Contains(block, "flag:") {
		t.Errorf("log_success_sample_window declares a flag, but observe reads only the environment — "+
			"`--flag` would load into the config and change nothing:\n%s", block)
	}

	m := regexp.MustCompile(`default_value:\s*"([^"]*)"`).FindStringSubmatch(block)
	if m == nil {
		t.Fatalf("log_success_sample_window has no default_value:\n%s", block)
	}
	window, perr := time.ParseDuration(m[1])
	if perr != nil {
		t.Fatalf("default_value %q is not a Go duration: %v", m[1], perr)
	}
	if window > 0 {
		t.Errorf("default_value %q samples successes; the default must log every success "+
			"(sampling is a per-environment opt-in)", m[1])
	}
}
