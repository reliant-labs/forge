// Package doctor runs health checks against a forge project's
// development stack: docker-compose services, app endpoints, the
// telemetry backends bundled in the lgtm container, and the Delve
// debugger when --debug is active.
//
// The package wraps two surfaces:
//
//   - Free Check* functions (CheckDocker, CheckAppHealth, CheckPprof,
//     CheckPrometheus, CheckTempo, CheckLoki, CheckPyroscope, CheckDelve)
//     are individual probes. They are exposed at package level so callers
//     can build their own check sets, and they are the primary unit of
//     test coverage.
//
//   - [Service] is the behavioural seam: it builds a [Doctor], registers
//     the standard checks, runs them in the documented order (sequential
//     stage discovers ports for the parallel stage), and pretty-prints
//     the report. CLI-side callers depend on the Service so tests can
//     swap in a fake report.
//
// Data carriers (CheckResult, Report, Environment, Status) remain plain
// types — they describe the outcome, they do not behave.
package doctor

import (
	"context"
	"fmt"
	"io"
)

// Service is the behavioural surface of the doctor package.
type Service interface {
	// RunStandard runs the canonical check set for projectName/projectDir
	// and returns the aggregate report. Sequential checks (Docker port
	// discovery) precede the parallel checks.
	RunStandard(ctx context.Context, projectName, projectDir string) Report

	// RunFiltered runs a subset of checks selected by signal.
	// signal is one of "" (all), "metrics", "traces", "logs", "profiles".
	// Returns an error for unknown signals.
	RunFiltered(ctx context.Context, projectName, projectDir, signal string) (Report, error)

	// PrintReport writes a human-readable report to w.
	PrintReport(w io.Writer, report Report, verbose bool)

	// PrintJSON writes the report as JSON to w.
	PrintJSON(w io.Writer, report Report) error
}

// Deps is the dependency set for the doctor Service. Empty today —
// every check reaches out to its backend (docker, http, dlv) directly.
type Deps struct{}

// New constructs a doctor.Service.
func New(_ Deps) Service { return &svc{} }

type svc struct{}

// standardChecks is the canonical full check set, used by both
// RunStandard and the RunFiltered "all" arm. Having one table is what
// keeps the two surfaces from drifting on display names (they previously
// disagreed: "docker" vs "Docker Compose", "app" vs "App Health", …).
// dockerCheckName names the entry that must run sequentially first
// because it discovers ports the parallel checks consume.
const dockerCheckName = "Docker Compose"

func standardChecks() []namedCheck {
	return []namedCheck{
		{dockerCheckName, CheckDocker},
		{"App Health", CheckAppHealth},
		{"pprof", CheckPprof},
		{"Prometheus", CheckPrometheus},
		{"Traces (Tempo)", CheckTempo},
		{"Logs (Loki)", CheckLoki},
		{"Profiles (Pyro)", CheckPyroscope},
		{"Delve", CheckDelve},
		{"covdata", CheckCovdata},
		{"Disowned Files", CheckDisownedFiles},
	}
}

// RunStandard wires the standard check set and runs it.
func (s *svc) RunStandard(ctx context.Context, projectName, projectDir string) Report {
	d := newDoctor(projectName, projectDir)
	for _, c := range standardChecks() {
		d.register(c.name, c.fn)
	}
	// Docker discovers ports, so it runs sequentially first.
	return d.run(ctx, []string{dockerCheckName})
}

// RunFiltered runs the named subset of checks. Empty signal means "all".
func (s *svc) RunFiltered(ctx context.Context, projectName, projectDir, signal string) (Report, error) {
	d := newDoctor(projectName, projectDir)
	switch signal {
	case "":
		for _, c := range standardChecks() {
			d.register(c.name, c.fn)
		}
	case "metrics":
		d.register(dockerCheckName, CheckDocker)
		d.register("Prometheus", CheckPrometheus)
	case "traces":
		d.register(dockerCheckName, CheckDocker)
		d.register("Traces (Tempo)", CheckTempo)
	case "logs":
		d.register(dockerCheckName, CheckDocker)
		d.register("Logs (Loki)", CheckLoki)
	case "profiles":
		d.register(dockerCheckName, CheckDocker)
		d.register("pprof", CheckPprof)
		d.register("Profiles (Pyro)", CheckPyroscope)
	default:
		return Report{}, fmt.Errorf("unknown signal %q (use: metrics, traces, logs, profiles)", signal)
	}
	return d.run(ctx, []string{dockerCheckName}), nil
}

// PrintReport delegates to the package-level pretty printer.
func (s *svc) PrintReport(w io.Writer, report Report, verbose bool) {
	printReport(w, report, verbose)
}

// PrintJSON delegates to the package-level JSON encoder.
func (s *svc) PrintJSON(w io.Writer, report Report) error {
	return printJSON(w, report)
}
