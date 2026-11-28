package k3dkmsg

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

func TestEnsure_StatFailureIsReturnedAsIs(t *testing.T) {
	boom := errors.New("stat exploded")
	r := &recorder{}
	h := linuxHost(r, StateMissing)
	h.Stat = func(p string) (State, error) {
		if p == HostPath {
			return StateMissing, nil
		}
		return StateMissing, boom
	}
	_, err := h.addArgs(nil)
	if !errors.Is(err, boom) {
		t.Fatalf("addArgs error = %v, want the stat error wrapped", err)
	}
	if len(r.ran) != 0 {
		t.Errorf("ran %v after a failed stat", r.ran)
	}
}

func TestEnsure_RemoveFailureTellsTheUserToRemoveByHand(t *testing.T) {
	r := &recorder{}
	h := linuxHost(r, StateMissing)
	r.state["/cache/forge/k3d/kmsg"] = StateOther
	h.Remove = func(string) error { return errors.New("permission denied") }
	_, err := h.addArgs(nil)
	if err == nil {
		t.Fatal("want an error when the stale file cannot be removed")
	}
	for _, want := range []string{
		"remove the stale /cache/forge/k3d/kmsg",
		"permission denied",
		"sudo rm -f /cache/forge/k3d/kmsg && sudo mknod /cache/forge/k3d/kmsg c 1 11",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if len(r.ran) != 0 {
		t.Errorf("mknod ran (%v) though the stale file was still in the way", r.ran)
	}
}

func TestEnsure_MkdirFailureTellsTheUserToMakeTheDirectory(t *testing.T) {
	r := &recorder{}
	h := linuxHost(r, StateMissing)
	h.Mkdir = func(string) error { return errors.New("read-only file system") }
	_, err := h.addArgs(nil)
	if err == nil {
		t.Fatal("want an error when the cache directory cannot be created")
	}
	for _, want := range []string{"read-only file system", "mkdir -p /cache/forge/k3d && sudo mknod"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if len(r.ran) != 0 {
		t.Errorf("mknod ran (%v) though the directory could not be created", r.ran)
	}
}

func TestAddArgs_NonLocalDockerHostLeavesArgsAlone(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://10.0.0.5:2375")
	in := []string{"cluster", "create", "x"}
	out, err := AddArgs(slices.Clone(in))
	if err != nil {
		t.Fatalf("AddArgs: %v", err)
	}
	if !slices.Equal(in, out) {
		t.Fatalf("argv changed for a remote daemon: %v", out)
	}
}

func TestAddArgs_NoCacheDirectoryIsAnError(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("os.UserCacheDir only fails from unset XDG_CACHE_HOME/HOME on linux")
	}
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("HOME", "")
	_, err := AddArgs(nil)
	if err == nil {
		t.Fatal("want an error when there is no cache directory")
	}
	if !strings.Contains(err.Error(), "no cache directory for the /dev/kmsg device file") {
		t.Errorf("error %q does not explain the missing cache directory", err)
	}
}

func TestOSHost_ReadsTheRealMachine(t *testing.T) {
	t.Setenv("DOCKER_HOST", "unix:///run/test-docker.sock")
	h, err := osHost()
	if err != nil {
		t.Fatalf("osHost: %v", err)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		t.Skipf("no user cache dir on this host: %v", err)
	}
	if h.GOOS != runtime.GOOS || h.Euid != os.Geteuid() {
		t.Errorf("host = %+v, want GOOS %s euid %d", h, runtime.GOOS, os.Geteuid())
	}
	if h.DockerHost != "unix:///run/test-docker.sock" {
		t.Errorf("DockerHost = %q, want the DOCKER_HOST env value", h.DockerHost)
	}
	if want := filepath.Join(cache, "forge", "k3d"); h.Dir != want {
		t.Errorf("Dir = %q, want %q", h.Dir, want)
	}
	if h.Stat == nil || h.Run == nil || h.Remove == nil || h.Mkdir == nil {
		t.Fatalf("osHost left a seam nil: %+v", h)
	}
	dir := filepath.Join(t.TempDir(), "a", "b")
	if err := h.Mkdir(dir); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("Mkdir did not create nested directories: %v", err)
	}
}

func TestStatDevice(t *testing.T) {
	dir := t.TempDir()

	missing, err := statDevice(filepath.Join(dir, "nope"))
	if err != nil || missing != StateMissing {
		t.Errorf("absent path = (%v, %v), want (StateMissing, nil)", missing, err)
	}

	file := filepath.Join(dir, "regular")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if st, err := statDevice(file); err != nil || st != StateOther {
		t.Errorf("regular file = (%v, %v), want (StateOther, nil)", st, err)
	}

	// A path under a regular file fails with ENOTDIR, not ErrNotExist: that is
	// a real error and must not be reported as "missing".
	if st, err := statDevice(filepath.Join(file, "child")); err == nil && st == StateMissing && runtime.GOOS != "windows" {
		t.Errorf("path under a regular file = (%v, nil), want an error", st)
	}
}

func TestStatDevice_CharDeviceWithOtherNumbersIsNotKmsg(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("char device numbers are only classified on linux")
	}
	// /dev/null is a character device (1:3): same major as kmsg, wrong minor.
	st, err := statDevice(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	if st != StateOther {
		t.Errorf("/dev/null classified %v, want StateOther", st)
	}
}

func TestStatDevice_SymlinkIsOtherNotFollowed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on windows")
	}
	dir := t.TempDir()
	link := filepath.Join(dir, "link")
	if err := os.Symlink(os.DevNull, link); err != nil {
		t.Skipf("cannot symlink: %v", err)
	}
	st, err := statDevice(link)
	if err != nil || st != StateOther {
		t.Errorf("symlink = (%v, %v), want (StateOther, nil): it must be replaced, not followed", st, err)
	}
}

const helperEnv = "K3DKMSG_TEST_HELPER"

func TestMain(m *testing.M) {
	switch os.Getenv(helperEnv) {
	case "":
		os.Exit(m.Run())
	case "fail":
		os.Stderr.WriteString("mknod: operation not permitted\n")
		os.Exit(3)
	default:
		os.Exit(0)
	}
}

func TestRunCombined(t *testing.T) {
	self, err := exec.LookPath(os.Args[0])
	if err != nil {
		self = os.Args[0]
	}

	t.Setenv(helperEnv, "ok")
	if err := runCombined(self); err != nil {
		t.Errorf("runCombined of a succeeding command: %v", err)
	}

	t.Setenv(helperEnv, "fail")
	err = runCombined(self)
	if err == nil {
		t.Fatal("want an error from a failing command")
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		t.Errorf("error %v does not wrap the *exec.ExitError", err)
	}
	if !strings.Contains(err.Error(), "mknod: operation not permitted") {
		t.Errorf("error %q lost the command's output", err)
	}

	if err := runCombined(filepath.Join(t.TempDir(), "no-such-binary")); err == nil {
		t.Error("want an error for a binary that does not exist")
	}
}
