package cli

import (
	"errors"
	"strings"
	"testing"
)

// The scaffolded dev env runs its API under air for hot reload. On a machine
// without air, `forge env up dev` must stop BEFORE it builds or starts
// anything, naming the workload, the install command and the go-run way out
// — not reach the host phase and fail on `exec: "air": executable file not
// found`. A runner that needs nothing beyond `go`, a job, and a workload the
// run does not target are not judged.
func TestPreflightHostRunners(t *testing.T) {
	host := func(name, kind, runner string) WorkloadEntity {
		return WorkloadEntity{Name: name, Kind: kind, Runtime: RuntimeEntity{Type: RuntimeHost, Host: &HostRuntime{Runner: runner}}}
	}
	e := &KCLEntities{Workloads: []WorkloadEntity{
		host("api", "service", "air"),
		host("mailer", "worker", "go-run"),
		host("migrate", "job", "air"),
	}}

	prev := hostRunnerLookPath
	t.Cleanup(func() { hostRunnerLookPath = prev })
	installed := map[string]bool{}
	hostRunnerLookPath = func(bin string) (string, error) {
		if installed[bin] {
			return "/usr/local/bin/" + bin, nil
		}
		return "", errors.New("executable file not found in $PATH")
	}

	err := preflightHostRunners(e, nil, "dev")
	if err == nil {
		t.Fatal("air is not installed and the API runs under it: want a refusal")
	}
	for _, want := range []string{"api runs under air (hot reload)", "go install github.com/air-verse/air@latest", `runner = "go-run"`, "deploy/kcl/dev/main.k"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal does not say %q:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "migrate") || strings.Contains(err.Error(), "mailer") {
		t.Errorf("a job, or a go-run workload, is not judged:\n%v", err)
	}
	if err := preflightHostRunners(e, []string{"mailer"}, "dev"); err != nil {
		t.Errorf("--target mailer does not start the API, so air is not needed: %v", err)
	}

	installed["air"] = true
	if err := preflightHostRunners(e, nil, "dev"); err != nil {
		t.Errorf("air installed: %v", err)
	}
}
