package scaffold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/templates"
)

// writeScaffoldedEnvs writes dev, staging and prod exactly as `forge project
// new` renders them, binding only migrate.
func writeScaffoldedEnvs(t *testing.T, root string) {
	t.Helper()
	for _, env := range []string{"dev", "staging", "prod"} {
		bindings := "    _hosted(wl.migrate)"
		if env == "dev" {
			bindings = "    _on_host_job(wl.migrate)"
		}
		out, err := templates.DeployTemplates().Render(scaffoldedEnvTemplate(env), templates.EnvTemplateData{
			ProjectName: "acme", EnvName: env, IngressEnabled: true, PrimaryWorkload: "acme", Bindings: bindings,
		})
		if err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(root, "deploy", "kcl", env)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "main.k"), out, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func readEnvMain(t *testing.T, root, env string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, "deploy", "kcl", env, "main.k"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A kind the hosted platform refuses must never be bound where it cannot run,
// and must never yield an env that renders green and deploys nothing.
// `forge scaffold operator` binds it to a cluster you operate in every
// deployed env, and — because a scaffolded env declares no cluster yet — says
// which envs now refuse to render and why, at the moment it happens.
func TestBindWorkloadInEnvs_OperatorBindsToClusterAndWarns(t *testing.T) {
	root := t.TempDir()
	writeScaffoldedEnvs(t, root)

	out, _ := captureStdout(t, func() error {
		bindWorkloadInEnvs(root, config.ComponentConfig{Name: "reaper", Kind: config.ComponentKindOperator})
		return nil
	})

	if got := readEnvMain(t, root, "dev"); !strings.Contains(got, "    _on_k3d(wl.reaper)\n]") {
		t.Errorf("dev must run the operator in the local cluster:\n%s", got)
	}
	for _, env := range []string{"staging", "prod"} {
		if got := readEnvMain(t, root, env); !strings.Contains(got, "    _on_cluster(wl.reaper)\n]") {
			t.Errorf("%s must bind the operator to a cluster you operate:\n%s", env, got)
		}
	}
	for _, want := range []string{
		"operator 'reaper' cannot run on Reliant hosting",
		"Kubernetes API",
		"deploy/kcl/prod/main.k",
		"deploy/kcl/staging/main.k",
		"declare `_cluster`",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("scaffold output does not say %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "deploy/kcl/dev/main.k —") {
		t.Errorf("dev runs the operator on k3d and needs no cluster; it must not be named:\n%s", out)
	}
}

// forge's cron component is a worker running its own scheduler, so it is
// hosted like any worker: no cluster, no warning.
func TestBindWorkloadInEnvs_CronComponentIsHosted(t *testing.T) {
	root := t.TempDir()
	writeScaffoldedEnvs(t, root)

	out, _ := captureStdout(t, func() error {
		bindWorkloadInEnvs(root, config.ComponentConfig{Name: "cleanup", Kind: config.ComponentKindCron})
		return nil
	})
	for _, env := range []string{"staging", "prod"} {
		if got := readEnvMain(t, root, env); !strings.Contains(got, "    _hosted(wl.cleanup)\n]") {
			t.Errorf("%s must host the cron component (a worker):\n%s", env, got)
		}
	}
	if strings.Contains(out, "cannot run on Reliant hosting") {
		t.Errorf("a hosted-admissible workload drew the refusal warning:\n%s", out)
	}
}
