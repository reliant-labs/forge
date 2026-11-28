package debug

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-delve/delve/service/api"
)

// The test binary doubles as a fake `dlv`: launchTestDlv copies it to
// <dir>/dlv(.exe) and puts <dir> first on PATH. Invoked under that name,
// TestMain parses the server flags forge passes, logs what it was given to
// stdout (which forge redirects to the session log), and serves a scripted
// Delve rpc2 server on --listen until killed. That drives Launch, Attach and
// spawn end to end with no real debugger.

const fakeDlvPID = 4242

func TestMain(m *testing.M) {
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(os.Args[0])), ".exe")
	if base == "dlv" {
		os.Exit(runFakeDlvServer(os.Args[1:]))
	}
	os.Exit(m.Run())
}

func runFakeDlvServer(args []string) int {
	var listen string
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, "--listen="); ok {
			listen = v
		}
	}
	os.Stdout.WriteString("fake-dlv args: " + strings.Join(args, " ") + "\n")
	os.Stdout.WriteString("fake-dlv env FAKE_DLV_MARK=" + os.Getenv("FAKE_DLV_MARK") + "\n")
	if p := os.Getenv("FAKE_DLV_MARK_FILE"); p != "" {
		_ = os.WriteFile(p, []byte(os.Getenv("FAKE_DLV_MARK")), 0o600)
	}
	if os.Getenv("FAKE_DLV_NEVER_LISTEN") != "" {
		time.Sleep(time.Hour)
		return 0
	}
	l, err := net.Listen("tcp", listen)
	if err != nil {
		os.Stderr.WriteString("fake-dlv: " + err.Error() + "\n")
		return 2
	}
	f := &scriptDlv{pid: fakeDlvPID, hang: make(chan struct{})}
	listenScript(f, l)
	return 0
}

func launchTestDlv(t *testing.T) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Skipf("cannot locate the test binary: %v", err)
	}
	name := "dlv"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	dir := t.TempDir()
	src, err := os.Open(self)
	if err != nil {
		t.Skipf("cannot read the test binary: %v", err)
	}
	defer src.Close()
	dst, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_WRONLY, 0o755) // #nosec G302 -- an executable test fixture
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(dst, src); err != nil {
		t.Fatal(err)
	}
	if err := dst.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// reap kills the spawned fake dlv so no test leaves a server behind.
func reap(t *testing.T, d *DelveDebugger) {
	t.Helper()
	t.Cleanup(func() {
		if pid := d.DlvPID(); pid > 0 {
			if p, err := os.FindProcess(pid); err == nil {
				_ = p.Kill()
			}
		}
	})
}

func TestLaunch_SpawnsDlvAndRecordsTheSession(t *testing.T) {
	launchTestDlv(t)
	out := filepath.Join(t.TempDir(), "logs", "dlv.log")

	d := NewDelveDebugger()
	reap(t, d)
	err := d.Launch(context.Background(), LaunchOptions{
		Binary:      "/bin/app",
		Args:        []string{"serve", "--port=8080"},
		Env:         append(os.Environ(), "FAKE_DLV_MARK=launched"),
		OutputPath:  out,
		StopOnStart: true,
	})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	if !strings.HasPrefix(d.Addr(), "127.0.0.1:") {
		t.Errorf("Addr = %q, want a loopback address (dlv's API runs code in the target)", d.Addr())
	}
	if d.DlvPID() <= 0 {
		t.Errorf("DlvPID = %d, want the spawned server's pid", d.DlvPID())
	}
	if d.PID() != fakeDlvPID {
		t.Errorf("PID = %d, want the target pid dlv reported (%d)", d.PID(), fakeDlvPID)
	}

	log := waitLog(t, out, "FAKE_DLV_MARK=launched")
	for _, want := range []string{
		"exec --headless --listen=" + d.Addr() + " --api-version=2 --accept-multiclient /bin/app -- serve --port=8080",
		"FAKE_DLV_MARK=launched",
	} {
		if !strings.Contains(log, want) {
			t.Errorf("dlv was started wrongly; log lacks %q:\n%s", want, log)
		}
	}
}

// waitLog polls the log until it holds want: the fake dlv writes its banner
// right as it starts, which can trail the listen by a moment.
func waitLog(t *testing.T, path, want string) string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, _ := os.ReadFile(path)
		if strings.Contains(string(b), want) || time.Now().After(deadline) {
			return string(b)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestStartWithEnv_LayersExtraEnvOntoTheEnvironment(t *testing.T) {
	launchTestDlv(t)
	marker := filepath.Join(t.TempDir(), "env-mark")
	t.Setenv("FAKE_DLV_MARK", "inherited")
	t.Setenv("FAKE_DLV_MARK_FILE", marker)

	d := NewDelveDebugger()
	reap(t, d)
	if err := d.StartWithEnv(context.Background(), "/bin/app", nil, []string{"FAKE_DLV_MARK=extra"}, 0); err != nil {
		t.Fatalf("StartWithEnv: %v", err)
	}
	// os.Environ() + extra: the later value wins, and the rest is inherited.
	if got := waitLog(t, marker, "extra"); strings.TrimSpace(got) != "extra" {
		t.Errorf("dlv saw FAKE_DLV_MARK=%q, want the extra value to win over the inherited one", strings.TrimSpace(got))
	}

	d2 := NewDelveDebugger()
	reap(t, d2)
	if err := d2.StartWithEnv(context.Background(), "/bin/app", nil, nil, 0); err != nil {
		t.Fatalf("StartWithEnv without extra env: %v", err)
	}
	if got := waitLog(t, marker, "inherited"); strings.TrimSpace(got) != "inherited" {
		t.Errorf("with no extra env dlv must inherit forge's environment, saw %q", strings.TrimSpace(got))
	}
}

func TestStart_UsesTheRequestedListenPort(t *testing.T) {
	launchTestDlv(t)
	port, err := freePort(context.Background())
	if err != nil {
		t.Skip("no loopback")
	}
	d := NewDelveDebugger()
	reap(t, d)
	if err := d.Start(context.Background(), "/bin/app", nil, port); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if want := "127.0.0.1:" + strconv.Itoa(port); d.Addr() != want {
		t.Errorf("Addr = %q, want %q", d.Addr(), want)
	}
}

func TestAttach_PassesThePidAndKnowsTheTarget(t *testing.T) {
	launchTestDlv(t)
	out := filepath.Join(t.TempDir(), "dlv-attach.log")
	d := NewDelveDebugger()
	reap(t, d)
	if err := d.StartAttach(context.Background(), 31337); err != nil {
		t.Fatalf("StartAttach: %v", err)
	}
	if d.PID() != 31337 {
		t.Errorf("PID = %d, want the attached pid 31337 (not asked of dlv)", d.PID())
	}

	d2 := NewDelveDebugger()
	reap(t, d2)
	if err := d2.Attach(context.Background(), AttachOptions{PID: 555, OutputPath: out, StopOnStart: true}); err != nil {
		t.Fatalf("Attach: %v", err)
	}
	log := waitLog(t, out, "attach --headless")
	if want := "attach --headless --listen=" + d2.Addr() + " --api-version=2 --accept-multiclient 555"; !strings.Contains(log, want) {
		t.Errorf("log lacks %q:\n%s", want, log)
	}
}

func TestSpawn_ReportsAnUnstartableDlv(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	err := NewDelveDebugger().Launch(context.Background(), LaunchOptions{Binary: "/bin/app", StopOnStart: true})
	if err == nil || !strings.Contains(err.Error(), "starting dlv exec") {
		t.Fatalf("Launch without dlv on PATH = %v, want a start error naming the verb", err)
	}
}

func TestSpawn_LogLocationErrors(t *testing.T) {
	launchTestDlv(t)
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := NewDelveDebugger().Launch(context.Background(), LaunchOptions{
		Binary: "/bin/app", OutputPath: filepath.Join(blocker, "dir", "dlv.log"), StopOnStart: true,
	})
	if err == nil || !strings.Contains(err.Error(), "creating log dir") {
		t.Fatalf("Launch = %v, want a log-dir error", err)
	}

	dir := t.TempDir() // a directory where the log file should be
	err = NewDelveDebugger().Launch(context.Background(), LaunchOptions{Binary: "/bin/app", OutputPath: dir, StopOnStart: true})
	if err == nil || !strings.Contains(err.Error(), "opening log") {
		t.Fatalf("Launch = %v, want an open-log error", err)
	}
}

func TestSpawn_NeverListeningTimesOutAndIsKilled(t *testing.T) {
	launchTestDlv(t)
	t.Setenv("FAKE_DLV_NEVER_LISTEN", "1")
	old := dlvReadyTimeout
	dlvReadyTimeout = 400 * time.Millisecond
	t.Cleanup(func() { dlvReadyTimeout = old })

	out := filepath.Join(t.TempDir(), "dlv.log")
	d := NewDelveDebugger()
	err := d.Launch(context.Background(), LaunchOptions{Binary: "/bin/app", OutputPath: out, StopOnStart: true})
	if err == nil {
		t.Fatal("Launch succeeded though dlv never listened")
	}
	for _, want := range []string{"waiting for dlv", "timeout waiting for dlv at", out, "fake-dlv args:"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error lacks %q:\n%v", want, err)
		}
	}
	if d.DlvPID() != 0 || d.Addr() != "" {
		t.Errorf("a failed launch must record nothing: addr %q dlvPID %d", d.Addr(), d.DlvPID())
	}
}

func TestAfterSpawn_ConnectFailure(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback")
	}
	addr := l.Addr().String()
	_ = l.Close()
	d := &DelveDebugger{addr: addr}
	if err := d.afterSpawn(true, 0); err == nil || !strings.Contains(err.Error(), "connecting to dlv") {
		t.Errorf("afterSpawn = %v, want a connect error", err)
	}
}

func TestAfterSpawn_ResumesUnlessStopOnStart(t *testing.T) {
	for _, stop := range []bool{true, false} {
		f := &scriptDlv{pid: 61}
		d := serveScript(t, f)
		d.client = nil // afterSpawn dials for itself
		if err := d.afterSpawn(stop, 0); err != nil {
			t.Fatal(err)
		}
		if d.pid != 61 {
			t.Errorf("stopOnStart=%v: pid = %d, want 61 from dlv", stop, d.pid)
		}
		if stop {
			if got := f.sentCommands(); len(got) != 0 {
				t.Errorf("stopOnStart left a halted target running: %v", got)
			}
			continue
		}
		if got := f.waitForCommands(t, 1); !slices.Equal(got, []string{api.Continue}) {
			t.Errorf("commands = %v, want the resume", got)
		}
	}
}

func TestListenPortOrFree(t *testing.T) {
	if got, err := listenPortOrFree(context.Background(), 4321); err != nil || got != 4321 {
		t.Errorf("an explicit port must pass through, got (%d, %v)", got, err)
	}
	got, err := listenPortOrFree(context.Background(), 0)
	if err != nil || got <= 0 {
		t.Errorf("a free port was not chosen: (%d, %v)", got, err)
	}
}

func TestDlvOutputTail(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if got := dlvOutputTail(filepath.Join(dir, "missing")); got != "" {
		t.Errorf("a missing log must add nothing, got %q", got)
	}
	if got := dlvOutputTail(write("empty", " \n\n")); got != "" {
		t.Errorf("a blank log must add nothing, got %q", got)
	}
	if got, want := dlvOutputTail(write("two", "first\nsecond\n")), ":\n  first\n  second"; got != want {
		t.Errorf("dlvOutputTail = %q, want %q", got, want)
	}
	long := strings.Repeat("x", dlvOutputTailBytes) + "TAIL"
	got := dlvOutputTail(write("long", "HEAD"+long))
	if !strings.HasSuffix(got, "TAIL") || strings.Contains(got, "HEAD") || len(got) > dlvOutputTailBytes+8 {
		t.Errorf("a long log must be cut to its last %d bytes, got %d bytes starting %q", dlvOutputTailBytes, len(got), got[:20])
	}
}

func TestWaitForServer(t *testing.T) {
	t.Run("returns once something listens", func(t *testing.T) {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Skip("no loopback")
		}
		defer l.Close()
		if err := waitForServer(context.Background(), l.Addr().String(), 5*time.Second, make(chan error)); err != nil {
			t.Errorf("waitForServer: %v", err)
		}
	})

	t.Run("an exited dlv ends the wait at once and carries its error", func(t *testing.T) {
		l, _ := net.Listen("tcp", "127.0.0.1:0")
		addr := l.Addr().String()
		_ = l.Close()
		exited := make(chan error, 1)
		exited <- io.ErrUnexpectedEOF
		start := time.Now()
		err := waitForServer(context.Background(), addr, time.Minute, exited)
		if !errors.Is(err, errDlvExited) || !strings.Contains(err.Error(), io.ErrUnexpectedEOF.Error()) {
			t.Errorf("err = %v, want errDlvExited carrying the exit error", err)
		}
		if time.Since(start) > 10*time.Second {
			t.Error("the wait did not stop at the exit")
		}
		exited <- nil
		if err := waitForServer(context.Background(), addr, time.Minute, exited); !errors.Is(err, errDlvExited) || err.Error() != errDlvExited.Error() {
			t.Errorf("a clean exit = %v, want the bare errDlvExited", err)
		}
	})

	t.Run("nothing listening times out naming the address", func(t *testing.T) {
		l, _ := net.Listen("tcp", "127.0.0.1:0")
		addr := l.Addr().String()
		_ = l.Close()
		err := waitForServer(context.Background(), addr, 250*time.Millisecond, make(chan error))
		if err == nil || !strings.Contains(err.Error(), "timeout waiting for dlv at "+addr) {
			t.Errorf("err = %v", err)
		}
	})
}
