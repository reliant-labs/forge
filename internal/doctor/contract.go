// Package doctor runs two DIFFERENT question sets, and keeping them apart
// is the point of this package's shape:
//
//   - PROJECT HEALTH — "is this project well-formed?" Deployability of the
//     rendered manifests, payload caps, disowned files, toolchain. These
//     read the project's own emitted artefacts, span every declared
//     environment, and need nothing running. `forge doctor` is exactly this
//     set, which is why it takes no `--env`: it never asks about one.
//
//   - ENV RUNTIME — "is the stack for environment X up and reachable?" The
//     compose infra, the app's /healthz, pprof, the telemetry backends,
//     Delve. Every one of these needs a resolved ADDRESS, and doctor has no
//     business inventing one. `forge env status <env>` owns the resolver
//     (it renders the same KCL `forge env up` does and overlays the ports
//     the live stack actually bound), so it is the caller for this set and
//     it supplies the addresses via [RuntimeTarget].
//
// The split exists because doctor used to straddle both and lied about the
// second half: it guessed the app was on :8080, missed a project serving on
// :3091, and rendered the miss as a gray dash indistinguishable from "not
// applicable" — while `forge env status` sat next to it reporting the real
// port, holder pid, ownership and build freshness. There is now ONE resolver
// for "what port does this component serve on", and it is not in here.
//
// The package wraps two surfaces:
//
//   - Free Check* functions (CheckDocker, CheckAppHealth, CheckPprof,
//     CheckPrometheus, CheckTempo, CheckLoki, CheckPyroscope, CheckDelve,
//     plus the deployability set) are individual probes. They are exposed
//     at package level so callers can build their own check sets, and they
//     are the primary unit of test coverage.
//
//   - [Service] is the behavioural seam: it builds a doctor, registers a
//     check set, runs it in the documented order (sequential stage
//     discovers ports for the parallel stage), and pretty-prints the
//     report. CLI-side callers depend on the Service so tests can swap in a
//     fake report.
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
	// RunFiltered runs the PROJECT-HEALTH set for projectName/projectDir.
	// signal is "" (all project checks) or "deploy" (deployability only —
	// the arm CI calls). Returns an error for any other value, including
	// the env-runtime signals that moved to `forge env status`.
	RunFiltered(ctx context.Context, projectName, projectDir, signal string) (Report, error)

	// RunRuntime runs the ENV-RUNTIME set against the addresses the caller
	// resolved. It never discovers a host port for itself — see
	// [RuntimeInput].
	RunRuntime(ctx context.Context, in RuntimeInput) (Report, error)

	// PrintReport writes a human-readable report to w.
	PrintReport(w io.Writer, report Report, verbose bool)

	// PrintJSON writes the report as JSON to w.
	PrintJSON(w io.Writer, report Report) error
}

// Deps is the dependency set for the doctor Service. Every check reaches out
// to its own backend (docker, http, dlv) directly, and the one thing doctor
// cannot derive for itself — the running stack's addresses — is passed per
// call on [RuntimeInput] rather than wired here, because it is a fact about
// one invocation and one environment, not about the service.
type Deps struct{}

// New constructs a doctor.Service.
//
// forge:no-observe
// Pure compute: empty Deps. Doctor shells out to local tooling and
// reports what it found; the CLI's own output is the trace.
func New(_ Deps) Service { return &svc{} }

type svc struct{}

// RuntimeTarget is the resolved address set the env-runtime checks probe.
// It is supplied by the CLI from the SAME render + live-port overlay
// `forge env status` uses for its own table. Empty fields mean "the caller
// could not resolve this", and the dependent check then reports
// [StatusUnknown] — never a pass, and never a skip.
type RuntimeTarget struct {
	// Service is the host service whose HTTP surface the app checks probe.
	// Carried so the result names WHICH service answered: "healthz=ok" is
	// useless in a project that runs four of them.
	Service string
	// HTTP is "host:port" of that service's main listener (/healthz,
	// /readyz).
	HTTP string
	// Pprof is "host:port" of its pprof side-listener (PPROF_ADDR), which
	// serverkit binds separately from the main mux.
	Pprof string
}

// RuntimeInput carries everything RunRuntime needs. Env is reported in
// messages so a report pasted into a bug says which environment it is about.
type RuntimeInput struct {
	ProjectName string
	ProjectDir  string
	Env         string
	Target      RuntimeTarget
	// Signal scopes the run: "" (all), "metrics", "traces", "logs",
	// "profiles", "app". Any other value is an error.
	Signal string
}

// composeCheckName names the compose-infra entry, which must run
// sequentially first because it discovers the container ports the telemetry
// checks consume. It is named for what it inspects: the host-service half of
// "is the dev stack up" is `forge env status`'s own table, which answers it
// with holder pid, forge-ownership and build freshness — facts a compose
// probe cannot reach.
const composeCheckName = "Compose Infra"

// projectChecks is the PROJECT-HEALTH set — the whole of `forge doctor`.
// Every entry answers a question about the project's own artefacts, so all
// of them are answerable in CI on a bare checkout with nothing running.
func projectChecks() []namedCheck {
	base := []namedCheck{
		{"covdata", CheckCovdata},
		{"Disowned Files", CheckDisownedFiles},
		{"Auth Issuer Parity", CheckAuthParity},
	}
	return append(base, deployabilityChecks()...)
}

// deployabilityChecks answer "would an SRE let this reach production". They
// read the project's own emitted artefacts (the rendered deploy/kcl
// manifests and the cmd/ composition root), so they need no running stack —
// which is the point: they must be answerable in CI, before anything is
// deployed. Each SKIPs cleanly on a project that declares no environments
// (--kind cli / library).
func deployabilityChecks() []namedCheck {
	return []namedCheck{
		{"Deploy Manifests", CheckDeployManifests},
		{"Deploy Probes", CheckDeployProbes},
		{"Deploy Resources", CheckDeployResources},
		{"Deploy Secrets", CheckDeploySecrets},
		{"Deploy SA Binding", CheckDeployServiceAccount},
		{"Deploy Migrations", CheckDeployMigrations},
		{"Payload Limits", CheckPayloadLimits},
	}
}

// runtimeSignals maps a RunRuntime signal to the checks it selects. The ""
// (all) arm is assembled from the parts so the whole set and the filtered
// arms cannot drift on a display name.
func runtimeSignals() map[string][]namedCheck {
	app := []namedCheck{{"App Health", CheckAppHealth}}
	profiles := []namedCheck{{"pprof", CheckPprof}, {"Profiles (Pyro)", CheckPyroscope}}
	metrics := []namedCheck{{"Prometheus", CheckPrometheus}}
	traces := []namedCheck{{"Traces (Tempo)", CheckTempo}}
	logs := []namedCheck{{"Logs (Loki)", CheckLoki}}
	delve := []namedCheck{{"Delve", CheckDelve}}

	all := []namedCheck{{composeCheckName, CheckDocker}}
	all = append(all, app...)
	all = append(all, profiles...)
	all = append(all, metrics...)
	all = append(all, traces...)
	all = append(all, logs...)
	all = append(all, delve...)

	// Every filtered arm keeps the compose check: it is what discovers the
	// Grafana/Delve container ports the rest read.
	withCompose := func(checks []namedCheck) []namedCheck {
		return append([]namedCheck{{composeCheckName, CheckDocker}}, checks...)
	}
	return map[string][]namedCheck{
		"":         all,
		"app":      withCompose(app),
		"metrics":  withCompose(metrics),
		"traces":   withCompose(traces),
		"logs":     withCompose(logs),
		"profiles": withCompose(profiles),
	}
}

// RunFiltered runs the named subset of the PROJECT-HEALTH set.
func (s *svc) RunFiltered(ctx context.Context, projectName, projectDir, signal string) (Report, error) {
	d := newDoctor(projectName, projectDir)
	switch signal {
	case "":
		for _, c := range projectChecks() {
			d.register(c.name, c.fn)
		}
	case "deploy":
		// Deployability only: no docker, no running stack. This is the arm
		// CI calls — `forge doctor --signal deploy --json` answers "would
		// this deploy" on a checkout, before an image exists.
		for _, c := range deployabilityChecks() {
			d.register(c.name, c.fn)
		}
	default:
		if _, moved := runtimeSignals()[signal]; moved && signal != "" {
			return Report{}, fmt.Errorf(
				"signal %q is an ENVIRONMENT-runtime question, not a project one — run `forge env status <env> --signal %s` "+
					"(it resolves the ports the stack actually bound; doctor would have to guess)", signal, signal)
		}
		return Report{}, fmt.Errorf("unknown signal %q (doctor accepts: deploy)", signal)
	}
	return d.run(ctx, nil), nil
}

// RunRuntime runs the ENV-RUNTIME set against in.Target. The addresses are
// the caller's — this package resolves none of them.
func (s *svc) RunRuntime(ctx context.Context, in RuntimeInput) (Report, error) {
	checks, ok := runtimeSignals()[in.Signal]
	if !ok {
		return Report{}, fmt.Errorf("unknown signal %q (use: app, metrics, traces, logs, profiles)", in.Signal)
	}
	d := newDoctor(in.ProjectName, in.ProjectDir)
	d.env.Env = in.Env
	d.env.Target = in.Target
	// Seed the resolved addresses BEFORE the compose check runs, under the
	// same keys the app checks read. A project running host-mode has no
	// "app" container for compose to discover; a project running under
	// compose has one, and CheckDocker publishes it. Seeding first means
	// the CALLER'S resolved address wins when both exist — which is the
	// whole point of the split.
	if in.Target.HTTP != "" {
		d.env.SetPort("app", 8080, in.Target.HTTP)
	}
	if in.Target.Pprof != "" {
		d.env.SetPort("app", 6060, in.Target.Pprof)
	}
	for _, c := range checks {
		d.register(c.name, c.fn)
	}
	return d.run(ctx, []string{composeCheckName}), nil
}

// PrintReport delegates to the package-level pretty printer.
func (s *svc) PrintReport(w io.Writer, report Report, verbose bool) {
	printReport(w, report, verbose)
}

// PrintJSON delegates to the package-level JSON encoder.
func (s *svc) PrintJSON(w io.Writer, report Report) error {
	return printJSON(w, report)
}
