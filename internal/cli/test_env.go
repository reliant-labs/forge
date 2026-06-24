// Package cli — `forge test --env=<env>`: the env-scoped half of the
// two-command e2e loop.
//
// The whole-project `forge test` (unit/integration/e2e subcommands) lives in
// test.go. This file adds ONE orthogonal mode: when `--env=<env>` is passed,
// forge reads the project's per-env `test:` block from forge.yaml, port-
// forwards the env's declared in-cluster services to local ports (each against
// its own kube-context, so a multi-cluster env forwards each service to the
// right cluster), exports the forward URLs + the declared env vars, runs the
// declared test command streaming its output, and tears the forwards down on
// exit — success, failure, or signal.
//
// This is deliberately un-magic: no codegen, no schema derivation, no in-
// process harness. The `test:` block is read verbatim and executed. The goal
// is for a project's Taskfile `e2e` target to shrink to:
//
//	forge up --env=e2e && forge test --env=e2e
package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/reliant-labs/forge/internal/config"
)

// forwarder starts one background port-forward and returns a handle that can
// stop it. It is an interface so tests can substitute a fake that binds a
// local listener without kubectl/a cluster.
type forwarder interface {
	// start launches the forward for f and returns a stop func. The forward
	// process is expected to keep running until stop is called.
	start(ctx context.Context, f config.TestForward, stderr io.Writer) (stop func(), err error)
}

// commandRunner runs the declared test command with the given environment and
// streams its output. Injectable so tests can assert on argv/env without
// spawning a real `go test`.
type commandRunner interface {
	run(ctx context.Context, argv []string, env []string, stdout, stderr io.Writer) error
}

// testEnvDeps are the injectable collaborators for the env-scoped test flow.
// Production wiring uses kubectlForwarder + execCommandRunner; tests inject
// fakes. waitForPort is injectable so the readiness probe is hermetic in tests.
type testEnvDeps struct {
	forwarder   forwarder
	runner      commandRunner
	waitForPort func(ctx context.Context, localPort int) error
	stdout      io.Writer
	stderr      io.Writer
}

// defaultTestEnvDeps returns the production collaborators.
func defaultTestEnvDeps() testEnvDeps {
	return testEnvDeps{
		forwarder:   kubectlForwarder{},
		runner:      execCommandRunner{},
		waitForPort: waitForLocalPort,
		stdout:      os.Stdout,
		stderr:      os.Stderr,
	}
}

// runTestEnv is the entry point for `forge test --env=<env>`. It resolves the
// env's recipe from forge.yaml, brings up the forwards, runs the command, and
// guarantees teardown.
func runTestEnv(ctx context.Context, env string, deps testEnvDeps) error {
	cfg, err := loadProjectConfig()
	if err != nil {
		return err
	}
	return runTestEnvWithConfig(ctx, env, cfg.Test, deps)
}

// runTestEnvWithConfig is the config-injected core, split out so tests drive it
// with an in-memory TestConfig instead of an on-disk forge.yaml.
func runTestEnvWithConfig(ctx context.Context, env string, tc config.TestConfig, deps testEnvDeps) error {
	if len(tc) == 0 {
		return fmt.Errorf("no `test:` block in forge.yaml; declare a per-env test recipe (command + forwards) to use `forge test --env=%s`", env)
	}
	recipe, ok := tc[env]
	if !ok {
		return fmt.Errorf("no test recipe for env %q under `test:` in forge.yaml (have: %s)", env, strings.Join(testEnvNames(tc), ", "))
	}
	if len(recipe.Command) == 0 {
		return fmt.Errorf("test recipe for env %q has no `command`", env)
	}
	if err := validateForwards(env, recipe.Forwards); err != nil {
		return err
	}

	// Install a signal handler so an interactive Ctrl-C (or a SIGTERM from a
	// parent task) cancels the run and triggers teardown via ctx cancellation
	// rather than killing the forwards out from under us and leaking them.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)
	go func() {
		select {
		case <-sigCh:
			fmt.Fprintln(deps.stderr, "[test] signal received — tearing down port-forwards")
			cancel()
		case <-ctx.Done():
		}
	}()

	// Bring up every forward. Teardown is LIFO and runs on every exit path
	// (return, panic-free error, signal). We collect stops as we go so a
	// failure midway still tears down the forwards already started.
	var stops []func()
	teardown := func() {
		for i := len(stops) - 1; i >= 0; i-- {
			stops[i]()
		}
	}
	defer teardown()

	for _, fwd := range recipe.Forwards {
		fmt.Fprintf(deps.stdout, "[test] port-forward %s svc/%s %d:%d (ns=%s)\n",
			fwd.Context, fwd.Service, fwd.LocalPort, fwd.RemotePort, fwd.Namespace)
		stop, err := deps.forwarder.start(ctx, fwd, deps.stderr)
		if err != nil {
			return fmt.Errorf("start port-forward for svc/%s: %w", fwd.Service, err)
		}
		stops = append(stops, stop)
	}

	// Wait for each forward's local port to accept connections before running
	// the suite, so the tests don't race the forwards binding.
	for _, fwd := range recipe.Forwards {
		if err := deps.waitForPort(ctx, fwd.LocalPort); err != nil {
			return fmt.Errorf("port-forward for svc/%s never became ready on :%d: %w", fwd.Service, fwd.LocalPort, err)
		}
	}

	// Build the test command environment: the process env, plus the per-
	// forward URL env vars, plus the declared literal env map. Later entries
	// win, so a recipe `env` can override a forward URL if it ever needs to.
	cmdEnv := testCommandEnv(recipe)

	fmt.Fprintf(deps.stdout, "[test] running: %s\n", strings.Join(recipe.Command, " "))
	if err := deps.runner.run(ctx, recipe.Command, cmdEnv, deps.stdout, deps.stderr); err != nil {
		return fmt.Errorf("test command failed: %w", err)
	}
	fmt.Fprintln(deps.stdout, "[test] passed")
	return nil
}

// testCommandEnv assembles the environment for the test command: the inherited
// process env, then the per-forward URLEnv → http://127.0.0.1:<LocalPort>
// entries, then the recipe's literal env map (which wins on conflict).
func testCommandEnv(recipe config.TestEnvConfig) []string {
	env := os.Environ()
	for _, fwd := range recipe.Forwards {
		if fwd.URLEnv == "" {
			continue
		}
		env = append(env, fmt.Sprintf("%s=http://127.0.0.1:%d", fwd.URLEnv, fwd.LocalPort))
	}
	for k, v := range recipe.Env {
		env = append(env, k+"="+v)
	}
	return env
}

// validateForwards rejects a recipe whose forwards are structurally incomplete
// before we shell out to kubectl, so the error names the problem rather than
// surfacing an opaque kubectl usage error.
func validateForwards(env string, forwards []config.TestForward) error {
	seenLocal := map[int]string{}
	for i, f := range forwards {
		where := fmt.Sprintf("test.%s.forwards[%d]", env, i)
		if f.Service == "" {
			return fmt.Errorf("%s: `service` is required", where)
		}
		if f.Namespace == "" {
			return fmt.Errorf("%s (svc/%s): `namespace` is required", where, f.Service)
		}
		if f.RemotePort <= 0 {
			return fmt.Errorf("%s (svc/%s): `remote_port` must be > 0", where, f.Service)
		}
		if f.LocalPort <= 0 {
			return fmt.Errorf("%s (svc/%s): `local_port` must be > 0", where, f.Service)
		}
		if prev, dup := seenLocal[f.LocalPort]; dup {
			return fmt.Errorf("%s (svc/%s): local_port %d already used by svc/%s", where, f.Service, f.LocalPort, prev)
		}
		seenLocal[f.LocalPort] = f.Service
	}
	return nil
}

func testEnvNames(tc config.TestConfig) []string {
	names := make([]string, 0, len(tc))
	for k := range tc {
		names = append(names, k)
	}
	// Stable order for the error message.
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j] < names[j-1]; j-- {
			names[j], names[j-1] = names[j-1], names[j]
		}
	}
	return names
}

// kubectlForwarder is the production forwarder: it shells out to
// `kubectl --context=<ctx> -n <ns> port-forward svc/<svc> <local>:<remote>`
// in its own process group so the whole forward subtree can be torn down.
type kubectlForwarder struct{}

func (kubectlForwarder) start(ctx context.Context, f config.TestForward, stderr io.Writer) (func(), error) {
	args := []string{}
	if f.Context != "" {
		args = append(args, "--context="+f.Context)
	}
	args = append(args,
		"-n", f.Namespace,
		"port-forward",
		"svc/"+f.Service,
		strconv.Itoa(f.LocalPort)+":"+strconv.Itoa(f.RemotePort),
	)
	// Not bound to ctx via CommandContext: we want explicit, group-wide
	// teardown (kubectl forks helpers) rather than a SIGKILL to the leader
	// only. The stop func below signals the whole group.
	cmd := exec.Command("kubectl", args...)
	cmd.Stdout = stderr
	cmd.Stderr = stderr
	startInOwnProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	stop := func() {
		if cmd.Process != nil {
			killProcessTree(cmd.Process.Pid, syscall.SIGTERM)
		}
		_ = cmd.Wait()
	}
	return stop, nil
}

// execCommandRunner runs the test command as a subprocess in its own process
// group, streaming stdout/stderr, and propagates a non-zero exit as an error.
type execCommandRunner struct{}

func (execCommandRunner) run(ctx context.Context, argv []string, env []string, stdout, stderr io.Writer) error {
	if len(argv) == 0 {
		return fmt.Errorf("empty test command")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = env
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Stdin = os.Stdin
	startInOwnProcessGroup(cmd)
	return cmd.Run()
}

// waitForLocalPort polls 127.0.0.1:<localPort> until a TCP connect succeeds or
// the context is cancelled / a 30s deadline elapses. The forwards are local
// kubectl processes that bind within a second or two; 30s is generous slack for
// a cold cluster.
func waitForLocalPort(ctx context.Context, localPort int) error {
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(localPort))
	deadline := time.Now().Add(30 * time.Second)
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		conn, err := net.DialTimeout("tcp", addr, time.Second)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after 30s waiting for %s", addr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
