package cli

import (
	"errors"
	"fmt"
	"net"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/config"
)

// newRunCmd is `forge run`: the single-command dev runner for the current
// working directory. It is a thin alias over `forge env up <env>` (the SAME
// runUp, no duplicated launch logic), so behaviour — KCL render,
// port-conflict guard, non-TTY detach, per-service logs — is identical to
// that path.
//
// The one thing `run` adds is dev-server passthrough: tokens after `--` are
// forwarded to each frontend's dev server (`npm run dev -- <flags>`). This is
// what the reliant one-shot workflow relies on — `reliant forge run --
// --host 0.0.0.0` starts the scaffolded Vite frontend bound to 0.0.0.0 so the
// workspace proxy can reach it and hand the user a preview URL.
//
// It runs the WHOLE loop, and that is the point. `run` used to imply
// --host-only, which silently rewrote every cluster-declared service into a
// local `go run` — forge overruling the environment's own declaration from a
// flag. A service that should run as a host process during dev says so in its
// KCL (`host = forge.HostOverrides {...}`, with `-D host_runner=go-run`
// selecting the runner); nothing here second-guesses that.
//
// No positional target: it brings up everything the env declares (the
// scaffold's single service + frontend), so the workflow needs no target to
// name. Env defaults to dev (the env `forge project new` scaffolds and the
// one-shot builds against).
func newRunCmd() *cobra.Command {
	var (
		env    string
		noSeed bool
	)
	cmd := &cobra.Command{
		Use:   "run [-- <dev-server flags>]",
		Short: "Run the project's dev loop against the current dir, forwarding flags after `--` to the frontend dev servers",
		Long: `Run the project's dev loop against the current working directory.

Brings up everything ` + "`deploy/kcl/<env>/`" + ` declares (default env: dev) —
the inner loop for iterating on a scaffolded project. This is an alias for
` + "`forge env up <env>`" + ` plus dev-server passthrough; see that command for
the full lifecycle (non-TTY runs start everything and return, leaving the
processes running; stop them with ` + "`forge env down <env>`" + `).

To iterate on ONE service, name it: ` + "`forge env up dev --target <svc>`" + `
scopes the whole run — build, deploy, host and frontend phases alike. A
service that should run as a local process during dev declares a
` + "`host = forge.HostOverrides {...}`" + ` block in its KCL, and
` + "`-D host_runner=go-run`" + ` selects the runner.

Tokens after ` + "`--`" + ` are forwarded to each frontend's dev server
(` + "`npm run dev -- <flags>`" + `), so a Vite/Next dev server can be told
to bind a specific host/port.

On first boot against a dev environment the app boots alive: the fresh
database is auto-seeded with deterministic, FK-coherent demo data derived
from the applied schema — only when the DB is reachable and every seedable
table is empty. Pass ` + "`--no-seed`" + ` to skip it, or inspect with
` + "`forge db seed status`" + `.

Examples:
  forge run                        # the whole dev loop, env=dev
  forge run --env=staging          # against the staging env's KCL
  forge run -- --host 0.0.0.0      # forward --host 0.0.0.0 to the dev server`,
		// Runtime failures (a port already bound, a child dying) are not
		// usage errors — dumping the flag table after them buries the
		// actionable message. Mirrors the removed run command's shape.
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			frontendArgs, err := runPassthroughArgs(args, cmd.ArgsLenAtDash())
			if err != nil {
				return err
			}
			return runUp(cmd.Context(), upOptions{
				env:          env,
				noSeed:       noSeed,
				frontendArgs: frontendArgs,
			})
		},
	}
	cmd.Flags().StringVar(&env, "env", "dev", "Deploy environment whose deploy/kcl/<env>/ to run (default: dev)")
	cmd.Flags().BoolVar(&noSeed, "no-seed", false, "Skip the first-boot dev auto-seed (by default the fresh dev DB is seeded once when reachable and empty)")
	return cmd
}

// runPassthroughArgs splits `forge run`'s positional args at the cobra
// `--` terminator: everything AFTER `--` is dev-server passthrough
// (forwarded to each frontend), and there must be nothing BEFORE it —
// `forge run` takes no positional target (it brings up everything
// host-mode). dashPos is cmd.ArgsLenAtDash(): the count of args before the
// `--`, or -1 when no `--` was given. Extracted from the RunE so the
// split/validation is unit-testable without a real project.
func runPassthroughArgs(args []string, dashPos int) ([]string, error) {
	const noPositional = "forge run takes no positional arguments; pass dev-server flags after `--` (e.g. forge run -- --host 0.0.0.0)"
	if dashPos < 0 {
		// No `--` terminator. Any bare positional is a usage mistake.
		if len(args) > 0 {
			return nil, errors.New(noPositional)
		}
		return nil, nil
	}
	if dashPos > 0 {
		// Positional args appeared before the `--`.
		return nil, errors.New(noPositional)
	}
	return args[dashPos:], nil
}

// This file holds the env-composition helpers shared by the host-mode
// phase of `forge env up` (up.go) and the dev/prod parity check
// (doctor_parity.go). The standalone `forge run` command — both the
// docker-compose orchestrator and the single host-mode service runner —
// was removed: the compose orchestrator is now a KCL deploy target
// consumed by `forge env up`/`forge env deploy`, and the single-service runner
// is `forge env up <env> --target <service>`. These helpers stayed
// because non-run code still depends on them.

// managedProcess tracks a running child process started by the `forge env up`
// orchestrator (up.go). name/cmd identify the child; pid is the PID
// captured at Start time, which survives cmd.Process.Release() on the
// `--background` detach path (Release resets cmd.Process.Pid to -1, so
// reading it afterwards — for the persisted state file `forge env down`
// reads — would record -1). Zero when unset; the foreground path reads
// cmd.Process.Pid directly.
type managedProcess struct {
	name string
	cmd  *exec.Cmd
	pid  int
}

// loadProjectConfigEnv resolves the per-env app config from
// deploy/kcl/<env>/config.k — via config_gen.appConfigEnvMap, the SAME
// projection cluster mode renders into each workload's env — and returns it as
// env-var strings. Returns an empty map (not nil) on any error so callers can
// pass the result straight to [hostlaunch.LayerHostEnv] without guarding. A
// missing/unrenderable config is non-fatal — host-mode services run against
// whatever defaults the binary's flag/env loader provides.
//
// Only the inline `value` channel applies on the host; `from_secret` entries
// belong to a cluster Secret and have no host equivalent (set them in
// `.env.<env>` or the developer shell). Reading the one KCL projection keeps
// host-mode services from drifting off their cluster-mode counterparts.
func loadProjectConfigEnv(_ *config.ProjectConfig, env string) map[string]string {
	out := map[string]string{}
	if env == "" {
		return out
	}
	projectPath, perr := findProjectConfigFile()
	if perr != nil {
		return out
	}
	srcs, err := loadProjectConfigEnvMap(filepath.Dir(projectPath), env)
	if err != nil {
		return out
	}
	for name, src := range srcs {
		if src.Value != nil {
			out[name] = *src.Value
		}
	}
	return out
}

// hostEnvVarsToMap projects the HostDeploy.EnvVars slice to a flat
// NAME→VALUE map for layering onto the subprocess env.
//
// Only the inline `value` channel applies on the host — KCLEnvVar's
// other channels (secret_ref, config_map_ref) are cluster-mode
// projections (Deployment.env.valueFrom.secretKeyRef etc.) with no
// meaningful host equivalent. Those projection channels stay in KCL
// for K8sCluster services; on the host, secrets come from the
// gitignored secrets_file.
//
// Returns an empty map (not nil) on a nil host, so callers can pass
// the result straight to [hostlaunch.LayerHostEnv] without guarding.
func hostEnvVarsToMap(host *HostDeploy) map[string]string {
	if host == nil || len(host.EnvVars) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(host.EnvVars))
	for _, ev := range host.EnvVars {
		if ev.Name == "" || ev.Value == "" {
			continue
		}
		out[ev.Name] = ev.Value
	}
	return out
}

// declaredServiceNames returns the names of every declared component,
// used by error paths that point users at the right spelling when they
// typo a service name. The inventory is enumerated from the REAL sources
// (proto descriptor + owned worker/operator files + cmd/ binaries):
// callers pass codegen.IntrospectComponents.
func declaredServiceNames(comps []config.ComponentConfig) []string {
	out := make([]string, 0, len(comps))
	for _, s := range comps {
		out = append(out, s.Name)
	}
	return out
}

// mergeConfigFrontends populates entities.Frontends from the project's
// forge.yaml `frontends:` when the rendered KCL carried none.
//
// Frontends are a forge.yaml concept, not a Kubernetes workload: `forge
// build` and `forge env deploy` read them from cfg.Frontends, and the
// env templates project only the k8s workloads
// (services/workers/crons/operators from deploy/kcl/workloads.k) into the
// `output` contract — never frontends. So the KCL render always comes back
// with entities.Frontends empty, and the up/run frontend phase (which
// iterates entities.Frontends) would start ZERO dev servers. This bridges
// cfg.Frontends into the entity set that phase consumes, mirroring the
// build/deploy path's source of truth so `forge run` actually launches the
// scaffolded frontend.
//
// No-op when the KCL already carried frontends (forward-compat, if a
// template ever emits them) or the project declares none. dev_runner
// defaults to npm in buildFrontendCmd, so it is left unset here.
func mergeConfigFrontends(e *KCLEntities, cfg *config.ProjectConfig) {
	if e == nil || cfg == nil || len(e.Frontends) > 0 || len(cfg.Frontends) == 0 {
		return
	}
	for _, fe := range cfg.Frontends {
		e.Frontends = append(e.Frontends, FrontendEntity{
			Name: fe.Name,
			Type: fe.Type,
			Path: fe.Path,
			Port: fe.Port,
		})
	}
}

// freeTCPPort asks the OS for an unused TCP port by binding :0 on loopback,
// reading the assigned port, then releasing it. There is an inherent
// (tiny) TOCTOU window between release and the dev server re-binding it, but
// for a per-project dev loop it is the standard, race-tolerant way to pick a
// port nothing else holds — and, crucially, two projects allocating
// concurrently are handed DIFFERENT ports by the kernel, which is exactly the
// non-collision property we need.
func freeTCPPort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	// The listener exists only to have the kernel hand us a free port;
	// it is closed immediately and never written to.
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// resolveEphemeralFrontendPorts assigns a free OS port to every frontend that
// declares none (Port <= 0 — the ephemeral scaffold default), mutating BOTH
// the entity set the run launches AND the matching cfg.Frontends entry (by
// name). Stamping both keeps every downstream reader consistent off a single
// resolved value: the pre-flight port-conflict guard and post-launch
// readiness/summary read entities.Frontends[].Port; buildFrontendCmd
// force-injects it as PORT into the dev server. The backend does not need
// the resolved value: ENVIRONMENT=development makes it reflect whatever
// Origin arrives, which is the only policy that can hold when the port is
// assigned by the kernel at launch.
//
// This is what makes two freshly-scaffolded dev stacks coexist on one host:
// each gets a distinct, kernel-assigned free frontend port instead of both
// fighting for 3000/3001. A frontend that DID declare a concrete port keeps
// it verbatim (no-op), so existing projects are unaffected. Allocation
// failure leaves the port at 0 — buildFrontendCmd then falls through to the
// dev server's own default, exactly as before.
func resolveEphemeralFrontendPorts(cfg *config.ProjectConfig, e *KCLEntities) {
	if e == nil {
		return
	}
	for i := range e.Frontends {
		if e.Frontends[i].Port > 0 {
			continue
		}
		port, err := freeTCPPort()
		if err != nil {
			fmt.Printf("[up] frontend %s: could not allocate an ephemeral port (%v); falling back to the dev server default\n", e.Frontends[i].Name, err)
			continue
		}
		e.Frontends[i].Port = port
		fmt.Printf("[up] frontend %s: ephemeral dev port %d\n", e.Frontends[i].Name, port)
		if cfg != nil {
			for j := range cfg.Frontends {
				if cfg.Frontends[j].Name == e.Frontends[i].Name {
					cfg.Frontends[j].Port = port
				}
			}
		}
	}
}

// resolveEphemeralHostPorts assigns a free OS port to every host service that
// declares no bind port of its own, so two host-only dev stacks never both
// fall back to the architectural backend default (:8080 — forge strips
// per-service ports from forge.yaml, so a freshly-scaffolded backend has no
// declared port and would otherwise collide with every other one). The
// allocated port is stamped as BOTH the service's ListenPorts[0] AND a PORT
// env literal, so every downstream reader stays consistent off one value:
// the pre-flight port-conflict guard and readiness gate (hostEnvPorts), the
// summary URL (hostEnvPort), and the launched process env (buildHostServiceCmd
// injects PORT via hostEnvVarsToMap, which the app binds). A service that DOES
// declare a port keeps it verbatim, so existing projects are unaffected.
//
// Returns the base URL of the primary (first) resolved host service so the
// caller can wire the frontends at the ephemeral backend; empty when no host
// service exposes a port.
func resolveEphemeralHostPorts(e *KCLEntities) string {
	if e == nil {
		return ""
	}
	backendURL := ""
	for i := range e.Services {
		svc := &e.Services[i]
		if svc.Deploy.Type != "host" || svc.Deploy.Host == nil {
			continue
		}
		host := svc.Deploy.Host
		// An explicitly EMPTY listen_ports means "this service binds nothing".
		// Allocating an ephemeral port for it publishes a port the process will
		// never bind, and the readiness gate then fails a run that in fact
		// succeeded — observed with the packaged desktop app, which launched
		// correctly and was still reported as "nothing is listening".
		if host.ListenPorts != nil && len(*host.ListenPorts) == 0 {
			continue
		}
		if len(hostEnvPorts(svc.Name, host)) > 0 {
			// Already binds a declared port — leave it, but adopt it as the
			// backend URL if we don't have one yet.
			if backendURL == "" {
				if p := hostEnvPort(svc.Name, host); p != "" {
					backendURL = "http://localhost:" + p
				}
			}
			continue
		}
		port, err := freeTCPPort()
		if err != nil {
			fmt.Printf("[up] host %s: could not allocate an ephemeral port (%v); falling back to the default\n", svc.Name, err)
			continue
		}
		host.ListenPorts = &[]int{port}
		host.EnvVars = upsertEnvVarValue(host.EnvVars, "PORT", fmt.Sprintf("%d", port))
		fmt.Printf("[up] host %s: ephemeral dev port %d\n", svc.Name, port)
		if backendURL == "" {
			backendURL = fmt.Sprintf("http://localhost:%d", port)
		}
	}
	return backendURL
}

// upsertEnvVarValue sets an inline name=value env var on a host service's KCL
// env list, replacing any existing entry for name (and clearing its ref
// channels so the inline value is authoritative). Used to stamp the allocated
// ephemeral PORT onto the host service the app binds.
func upsertEnvVarValue(vars []KCLEnvVar, name, value string) []KCLEnvVar {
	for i := range vars {
		if vars[i].Name == name {
			vars[i].Value = value
			vars[i].SecretRef = ""
			vars[i].SecretKey = ""
			vars[i].ConfigMapRef = ""
			vars[i].ConfigMapKey = ""
			return vars
		}
	}
	return append(vars, KCLEnvVar{Name: name, Value: value})
}

// frontendEnvPrefix returns the public-env-var prefix a frontend of the
// given type exposes to its client bundle: Next.js NEXT_PUBLIC_, Vite
// VITE_, React Native/Expo EXPO_PUBLIC_. Defaults to the Next.js prefix
// for an unset/unknown type. The single dispatch every framework-scoped
// var name (API_URL / MOCK_API / OTEL_ENDPOINT / ENVIRONMENT) is built on.
//
// Accepts BOTH the KCL Frontend.type spellings ("vite" / "rn" — what
// render.k projects) and the longer forge.yaml / scaffold-kind spellings
// ("vite-spa" / "react-native"), so the dispatch is correct whether the
// type comes from a rendered KCL entity or a config kind.
func frontendEnvPrefix(frontendType string) string {
	switch strings.ToLower(strings.TrimSpace(frontendType)) {
	case "vite", "vite-spa":
		return "VITE_"
	case "rn", "react-native", "react_native":
		return "EXPO_PUBLIC_"
	default:
		return "NEXT_PUBLIC_"
	}
}

// isNextFrontend reports whether the frontend type is Next.js (the
// default). Used to gate the Next-only NEXT_TELEMETRY_DISABLED knob.
func isNextFrontend(frontendType string) bool {
	return frontendEnvPrefix(frontendType) == "NEXT_PUBLIC_"
}

// frontendAPIURLEnvVar returns the environment variable a frontend of the
// given type reads to override its API base URL. Each scaffold's dev transport
// honors exactly one (see the generated connect.ts / apiurl_gen.ts): Next.js
// reads NEXT_PUBLIC_API_URL, Vite reads VITE_API_URL, React Native/Expo reads
// EXPO_PUBLIC_API_URL. Defaults to the Next.js name for an unset/unknown type.
func frontendAPIURLEnvVar(frontendType string) string {
	return frontendEnvPrefix(frontendType) + "API_URL"
}

// frontendMockEnvVar / frontendOTELEnvVar / frontendEnvironmentEnvVar are
// the mock-mode / browser-OTLP-endpoint / environment-label siblings of
// frontendAPIURLEnvVar — the framework-prefixed variable each scaffold's
// transport / telemetry module reads (see connect.ts / otel.ts). Same
// dispatch (frontendEnvPrefix) so the four move together.
func frontendMockEnvVar(frontendType string) string {
	return frontendEnvPrefix(frontendType) + "MOCK_API"
}

func frontendOTELEnvVar(frontendType string) string {
	return frontendEnvPrefix(frontendType) + "OTEL_ENDPOINT"
}

func frontendEnvironmentEnvVar(frontendType string) string {
	return frontendEnvPrefix(frontendType) + "ENVIRONMENT"
}

// frontendConfigEnv maps a typed FrontendConfig onto the framework-scoped
// env vars (NEXT_PUBLIC_* / VITE_* / EXPO_PUBLIC_*, plus Next.js's
// NEXT_TELEMETRY_DISABLED) the frontend's transport + build read. It
// returns them as inline-value KCLEnvVar entries so they flow through the
// SAME dev-launch + build-time plumbing env_vars use. nil cfg (no `config`
// block) yields nil.
//
// mock is normalized: "off" (the default) contributes NOTHING here — the
// scaffold's transport already defaults to the real backend, and the
// build path force-sets an authoritative empty mock var separately (see
// frontendBuildEnv). "true" / "hybrid" pass through so a KCL-declared mock
// applies at dev launch (still overridable by the developer's shell).
func frontendConfigEnv(frontendType string, cfg *FrontendConfigEntity) []KCLEnvVar {
	if cfg == nil {
		return nil
	}
	var out []KCLEnvVar
	if cfg.APIURL != "" {
		out = append(out, KCLEnvVar{Name: frontendAPIURLEnvVar(frontendType), Value: cfg.APIURL})
	}
	if mv := frontendMockValue(cfg.Mock); mv != "" {
		out = append(out, KCLEnvVar{Name: frontendMockEnvVar(frontendType), Value: mv})
	}
	if cfg.OTELEndpoint != "" {
		out = append(out, KCLEnvVar{Name: frontendOTELEnvVar(frontendType), Value: cfg.OTELEndpoint})
	}
	if cfg.Environment != "" {
		out = append(out, KCLEnvVar{Name: frontendEnvironmentEnvVar(frontendType), Value: cfg.Environment})
	}
	if cfg.TelemetryDisabled && isNextFrontend(frontendType) {
		out = append(out, KCLEnvVar{Name: "NEXT_TELEMETRY_DISABLED", Value: "1"})
	}
	return out
}

// frontendMockValue normalizes a FrontendConfig.mock to the value its
// *_MOCK_API variable carries: "off" (or empty/unset) becomes "" — the
// real-backend default — while "true" / "hybrid" pass through verbatim
// (connect.ts treats anything other than those two as the real backend).
func frontendMockValue(mock string) string {
	m := strings.ToLower(strings.TrimSpace(mock))
	if m == "" || m == "off" {
		return ""
	}
	return m
}

// frontendConfigMockValue is frontendMockValue over an optional config
// block: "" (real backend) for a nil config or mock=off, else the mode.
func frontendConfigMockValue(cfg *FrontendConfigEntity) string {
	if cfg == nil {
		return ""
	}
	return frontendMockValue(cfg.Mock)
}

// collapseJobsToHost rewrites each one-shot job's argv from the IN-IMAGE
// form to something runnable on this machine.
//
// This is NOT the deleted deploy-type collapse. That one rewrote a
// cluster-declared SERVICE into a host process, overruling the environment's
// own placement decision from a CLI flag. This rewrites only argv[0], for
// jobs forge is about to exec on the host through runHostJobs — which runs on
// every `forge env up`, so the translation is unconditional rather than
// gated on a flag.
//
// A job's `command` is written for the CONTAINER: `/app/<project> db
// migrate up` is where the Dockerfile's production stage puts the binary.
// That path does not exist on the developer's laptop, so exec'ing it
// verbatim fails with "no such file or directory" — naming a path the
// reader never wrote and cannot find, for a job whose declaration looks
// completely correct.
//
// The translation is the one the host runner already makes for services:
// the argv's first element is the project binary, and on the host the
// project binary is `go run ./cmd/<project>`. Everything after argv[0] is
// the subcommand selection (`db migrate up`) and passes through
// untouched, because that half means the same thing in both worlds.
//
// A job whose argv is NOT the in-image project binary is left ALONE. It
// is something else the author wired deliberately — a shell script, a
// vendored tool, an absolute path they meant — and rewriting it would be
// forge overruling an explicit choice.
func collapseJobsToHost(e *KCLEntities, projectName string) {
	if e == nil || projectName == "" {
		return
	}
	inImage := "/app/" + projectName
	for i := range e.Jobs {
		cmd := e.Jobs[i].Command
		if len(cmd) == 0 || cmd[0] != inImage {
			continue
		}
		host := make([]string, 0, 3+len(cmd)-1)
		host = append(host, "go", "run", "./cmd/"+projectName)
		host = append(host, cmd[1:]...)
		e.Jobs[i].Command = host
	}
}
