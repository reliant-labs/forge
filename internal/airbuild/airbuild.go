// Package airbuild is `forge dev air-build`: the build step of the scaffolded
// .air.toml, which keeps air from restarting the running server when a build
// fails.
//
// WHY. air (v1.67.4, and its master) builds first and then, on unix, stops the
// running binary and starts the one on disk — whether or not the build
// succeeded. With stop_on_error = false (the scaffold's) a failed build
// therefore costs a full restart (SIGINT, kill_delay's drain, a new process)
// onto the OLD binary: every stream the server held is cut, and nothing says
// it is still running stale code. stop_on_error = true is worse: air stops the
// server and returns, so a typo takes the API down until the fix compiles.
// air has no setting that leaves the process alone.
//
// (Both settings act only once the build command returns, so under a hold
// stop_on_error is never read; the scaffold keeps false for the cases below
// that do not hold.)
//
// HOW. air runs a build in a goroutine (buildRun) and does nothing with the
// server until the build command returns. So a failed build here does not
// return: it prints one line and waits while air lives. The server keeps
// serving the last good build. The next save starts a new build, which, when
// it succeeds, restarts the server once with the fresh code, as any build
// does.
//
// The failed build never returns while air runs, even after it is superseded,
// and that is load-bearing. air keeps each build's stop token in a one-slot
// channel: on a new change it takes the running build's token out and closes
// it, and each build takes a token out again when it returns. A superseded
// build's return is therefore one take too many: it removes the NEXT build's
// token, after which air can cancel nothing, and every later superseded build
// stops and starts the server concurrently with its successor (two servers,
// one orphaned holding the port — observed with air v1.67.4). A build that
// never returns never takes, and it also absorbs the surplus take air causes
// on its own when a file changes during an ordinary build. Each held build is
// one idle process until air exits: a /bin/sh polling once a second
// (execWaiter), not a forge, so a long session of compile errors costs a few
// megabytes.
//
// It does not hold, and exits with the build's status at once (air then does
// what it always did), when holding would keep nothing running:
//
//   - on Windows, where air stops the binary before building (a running .exe
//     cannot be replaced), so there is no server left to keep;
//   - when no build has succeeded under THIS air process — the first build of
//     an `env up`, or a binary left behind by an air that was killed. The
//     readiness gate of `forge env up` reads air's "failed to build" line, so
//     a failed first build must reach air at once;
//   - when the air process cannot be told apart (a platform with no process
//     table forge reads, or a run that is not air's child).
//
// It stops waiting, and exits with the build's status, when the air that ran
// it is gone (its process group's leader was reparented), when air removed its
// tmp_dir on exit (clean_on_exit), or on SIGINT/SIGTERM/SIGHUP — so no build
// outlives `forge env down`.
package airbuild

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/reliant-labs/forge/internal/atomicfile"
)

// DefaultPoll is how often a held build checks whether air went away.
const DefaultPoll = 200 * time.Millisecond

// Options configure one build.
type Options struct {
	// Out is the binary air runs (the config's entrypoint). The build's
	// output is installed here, and the bookkeeping file lives beside it.
	Out string
	// Argv is the build command, run as given: no shell.
	Argv []string
	// PrecheckStatus is the storage check's exit code. Non-zero skips the
	// command and handles the precheck failure like a failed build.
	PrecheckStatus int
	// Env is the build's environment; nil inherits this process's.
	Env []string
	// Stdout and Stderr receive the build's output and this command's lines.
	Stdout, Stderr io.Writer
	// GOOS is the platform whose air semantics apply; runtime.GOOS when "".
	GOOS string
	// Poll is the held build's check interval; DefaultPoll when 0.
	Poll time.Duration
	// Runner names the air process this build runs under: an ID stable for
	// its lifetime, and a func reporting whether it is still there. ok is
	// false when it cannot be told. nil uses the process table
	// (CurrentRunner); a test passes a fake.
	Runner func() (id string, alive func() bool, ok bool)
	// Signals, when set, replaces the SIGINT/SIGTERM/SIGHUP subscription: a
	// value on it is forwarded to a running build, and ends a hold.
	Signals <-chan os.Signal
	// ExecWaiter hands a hold to a small /bin/sh wait loop (execWaiter) in
	// place of this process, on linux and darwin: `forge dev air-build`
	// sets it. Unset, or when the exec fails, the hold runs in-process.
	ExecWaiter bool
}

// Result is what one build did.
type Result struct {
	// ExitCode is the status the command exits with.
	ExitCode int
	// Held reports that the build failed and waited until air went away.
	Held bool
	// Err is a failure of this command itself (the build could not start,
	// or its output could not be installed); ExitCode is non-zero with it.
	Err error
}

// Run builds once, as described in the package comment.
func Run(ctx context.Context, o Options) Result {
	o = o.withDefaults()
	if len(o.Argv) == 0 {
		return Result{ExitCode: 2, Err: errors.New("no build command given")}
	}
	sigs := o.Signals
	if sigs == nil {
		ch := make(chan os.Signal, 4)
		signal.Notify(ch, os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
		defer signal.Stop(ch)
		sigs = ch
	}

	code := o.PrecheckStatus
	var err error
	if code == 0 {
		staged := statOrNil(o.stagedPath())
		code, err = execute(ctx, o.Argv, o.Env, o.Stdout, o.Stderr, sigs)
		if code == 0 {
			err = install(o.stagedPath(), o.Out, staged)
			if err != nil {
				code = 1
			}
		}
	}
	runner, alive, haveRunner := o.Runner()
	if code == 0 {
		if haveRunner {
			_ = atomicfile.WriteFile(o.okPath(), []byte(runner), 0o644)
		} else {
			_ = os.Remove(o.okPath())
		}
		return Result{}
	}
	if o.GOOS == "windows" || !haveRunner || !o.builtUnder(runner) {
		return Result{ExitCode: code, Err: err}
	}
	if err != nil {
		_, _ = fmt.Fprintf(o.Stderr, "forge dev air-build: %v\n", err)
	}
	_, _ = fmt.Fprintf(o.Stderr, "forge: failed to build; the running %s keeps serving the last good build. Fix the error and save to rebuild.\n", filepath.Base(o.Out))
	// The wait lasts the rest of the air session, and one is left per failed
	// build: hand it to a shell, else give back what this process can.
	if o.ExecWaiter {
		_ = execWaiter(o.Out, code)
	}
	debug.FreeOSMemory()
	o.hold(ctx, alive, sigs)
	return Result{ExitCode: code, Held: true}
}

func (o Options) withDefaults() Options {
	if o.GOOS == "" {
		o.GOOS = runtime.GOOS
	}
	if o.Poll <= 0 {
		o.Poll = DefaultPoll
	}
	if o.Runner == nil {
		o.Runner = CurrentRunner
	}
	if o.Stdout == nil {
		o.Stdout = io.Discard
	}
	if o.Stderr == nil {
		o.Stderr = io.Discard
	}
	return o
}

// stagedPath is where the build writes: Out with .tmp appended, as the
// scaffolded command's `go build -o` names it.
func (o Options) stagedPath() string { return o.Out + ".tmp" }

// okPath holds the runner ID the last successful build of Out ran under.
func (o Options) okPath() string {
	return filepath.Join(filepath.Dir(o.Out), "."+filepath.Base(o.Out)+".air-build-ok")
}

// builtUnder reports whether Out is a binary this runner started: it exists,
// and the last successful build ran under the same air process. Only then is
// a server from it running for a hold to keep.
func (o Options) builtUnder(runner string) bool {
	if st, err := os.Stat(o.Out); err != nil || !st.Mode().IsRegular() {
		return false
	}
	ok, err := os.ReadFile(o.okPath())
	return err == nil && string(ok) == runner
}

// hold waits until air is gone, its tmp_dir is removed (clean_on_exit), or a
// signal arrives. It never ends because a newer build started: see the
// package comment.
func (o Options) hold(ctx context.Context, alive func() bool, sigs <-chan os.Signal) {
	t := time.NewTicker(o.Poll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sigs:
			return
		case <-t.C:
		}
		if _, err := os.Stat(filepath.Dir(o.Out)); err != nil {
			return
		}
		if alive != nil && !alive() {
			return
		}
	}
}

// install moves the build's staged output onto out. A build that wrote no
// staged file (or left one it did not touch) wrote out itself, or nothing.
func install(staged, out string, before fs.FileInfo) error {
	after := statOrNil(staged)
	if after == nil {
		return nil
	}
	if before != nil && os.SameFile(before, after) && before.ModTime().Equal(after.ModTime()) && before.Size() == after.Size() {
		return nil
	}
	if err := atomicfile.Replace(staged, out); err != nil {
		return fmt.Errorf("install the build's output: %w", err)
	}
	return nil
}

func statOrNil(path string) fs.FileInfo {
	st, err := os.Stat(path)
	if err != nil {
		return nil
	}
	return st
}

// execute runs argv and returns its exit status: the command's own, 128+n
// when signal n killed it, 127 when it was not found and 126 when it could
// not start. A signal on sigs is forwarded to it.
func execute(ctx context.Context, argv, env []string, stdout, stderr io.Writer, sigs <-chan os.Signal) (int, error) {
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec // the build command the air config names
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = stdout, stderr
	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, fs.ErrNotExist) {
			return 127, err
		}
		return 126, err
	}
	done := make(chan struct{})
	forwarded := make(chan struct{})
	go func() {
		defer close(forwarded)
		for {
			select {
			case s := <-sigs:
				_ = cmd.Process.Signal(s)
			case <-ctx.Done():
				_ = cmd.Process.Signal(os.Interrupt)
				return
			case <-done:
				return
			}
		}
	}()
	_ = cmd.Wait()
	close(done)
	<-forwarded
	return exitStatus(cmd.ProcessState), nil
}

func exitStatus(ps *os.ProcessState) int {
	if ps == nil {
		return 1
	}
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	if code := ps.ExitCode(); code >= 0 {
		return code
	}
	return 1
}
