package buildtarget

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/reliant-labs/forge/pkg/release"
)

const execHelperEnv = "BUILDTARGET_EXEC_HELPER"

// The test binary doubles as the command execRunner runs, so the real
// os/exec path is exercised on every OS without depending on a shell tool.
func TestMain(m *testing.M) {
	switch os.Getenv(execHelperEnv) {
	case "":
		os.Exit(m.Run())
	case "fail":
		os.Exit(7)
	default:
		wd, _ := os.Getwd()
		_ = os.WriteFile(os.Getenv(execHelperEnv+"_OUT"), []byte(wd+"|"+os.Getenv("BUILDTARGET_MARK")), 0o600)
		os.Exit(0)
	}
}

func TestExecRunner_RunInDir(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Skip("no test binary path")
	}
	work := t.TempDir()
	out := filepath.Join(t.TempDir(), "out")
	env := map[string]string{execHelperEnv: "write", execHelperEnv + "_OUT": out, "BUILDTARGET_MARK": "layered"}

	if err := (execRunner{}).RunInDir(context.Background(), work, env, self); err != nil {
		t.Fatalf("RunInDir: %v", err)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	wd, mark, _ := strings.Cut(string(got), "|")
	if resolved, _ := filepath.EvalSymlinks(work); wd != work && wd != resolved {
		t.Errorf("command ran in %q, want %q", wd, work)
	}
	if mark != "layered" {
		t.Errorf("env overlay did not reach the command: %q", mark)
	}

	t.Run("RunWithEnv layers the overlay and inherits the working directory", func(t *testing.T) {
		out2 := filepath.Join(t.TempDir(), "out2")
		env2 := map[string]string{execHelperEnv: "write", execHelperEnv + "_OUT": out2, "BUILDTARGET_MARK": "m2"}
		if err := (execRunner{}).RunWithEnv(context.Background(), env2, self); err != nil {
			t.Fatal(err)
		}
		b, _ := os.ReadFile(out2)
		here, _ := os.Getwd()
		if wd, mark, _ := strings.Cut(string(b), "|"); wd != here || mark != "m2" {
			t.Errorf("got %q, want cwd %q and mark m2", b, here)
		}
	})

	t.Run("Run uses the host environment", func(t *testing.T) {
		t.Setenv(execHelperEnv, "write")
		out3 := filepath.Join(t.TempDir(), "out3")
		t.Setenv(execHelperEnv+"_OUT", out3)
		t.Setenv("BUILDTARGET_MARK", "host")
		if err := (execRunner{}).Run(context.Background(), self); err != nil {
			t.Fatal(err)
		}
		if b, _ := os.ReadFile(out3); !strings.HasSuffix(string(b), "|host") {
			t.Errorf("host env not inherited: %q", b)
		}
	})

	t.Run("a failing command names itself and keeps the exit error", func(t *testing.T) {
		err := (execRunner{}).RunInDir(context.Background(), "", map[string]string{execHelperEnv: "fail"}, self, "-flag", "arg")
		var exitErr *exec.ExitError
		if err == nil || !errors.As(err, &exitErr) || exitErr.ExitCode() != 7 {
			t.Fatalf("err = %v, want an ExitError with code 7", err)
		}
		if !strings.Contains(err.Error(), "-flag arg") {
			t.Errorf("error %q does not echo the command line", err)
		}
	})

	t.Run("a missing binary", func(t *testing.T) {
		if err := (execRunner{}).Run(context.Background(), filepath.Join(t.TempDir(), "no-such-tool")); err == nil {
			t.Error("want an error")
		}
	})
}

func TestNewRunnerExecutesShellBuilds(t *testing.T) {
	dir := t.TempDir()
	res := NewRunner().Build(context.Background(), Spec{
		Service: "api", Tag: "v1", ProjectDir: dir,
		BuildCmd: `echo "$MARK" > mark.txt`, BuildEnv: map[string]string{"MARK": "built"},
	})
	if res.Err != nil {
		t.Fatalf("Build: %v", res.Err)
	}
	if res.Service != "api" || res.Tag != "v1" || res.Duration <= 0 {
		t.Errorf("result = %+v", res)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "mark.txt")); strings.TrimSpace(string(b)) != "built" {
		t.Errorf("the shell build did not run in the project dir: %q", b)
	}
}

// receiptRunner is a fake shell runner that, like a typed builder, publishes
// a receipt at the path forge handed it in the environment.
type receiptRunner struct {
	fakeRunner
	write func(path string) error
	seen  map[string]string
}

func (r *receiptRunner) RunShell(ctx context.Context, dir string, env map[string]string, script string) error {
	r.seen = env
	if r.write != nil {
		if err := r.write(env[ReceiptPathEnv]); err != nil {
			return err
		}
	}
	return r.fakeRunner.RunShell(ctx, dir, env, script)
}

func TestBuild_RequireReceipt(t *testing.T) {
	fixture := receiptFixture(t)
	spec := Spec{Service: "reliant", ProjectDir: t.TempDir(), BuildCmd: "build", RequireReceipt: true, ExpectedInputDigest: fixture.InputDigest}

	t.Run("a valid receipt is returned and its path is private to this build", func(t *testing.T) {
		r := &receiptRunner{write: func(p string) error { return WriteReceipt(p, fixture) }}
		res := (Runner{runner: r}).Build(context.Background(), spec)
		if res.Err != nil {
			t.Fatalf("Build: %v", res.Err)
		}
		if res.Receipt == nil || res.Receipt.InputDigest != fixture.InputDigest {
			t.Errorf("receipt = %+v", res.Receipt)
		}
		if p := r.seen[ReceiptPathEnv]; p == "" || !filepath.IsAbs(p) {
			t.Errorf("%s = %q, want an absolute path", ReceiptPathEnv, p)
		} else if _, err := os.Stat(filepath.Dir(p)); !os.IsNotExist(err) {
			t.Errorf("the receipt directory was not cleaned up (stat err = %v)", err)
		}
	})

	t.Run("a builder that wrote no receipt fails the build", func(t *testing.T) {
		res := (Runner{runner: &receiptRunner{}}).Build(context.Background(), spec)
		if res.Err == nil || !strings.Contains(res.Err.Error(), "read build receipt") {
			t.Errorf("err = %v", res.Err)
		}
		if res.Receipt != nil {
			t.Error("no receipt may be reported for a failed build")
		}
	})

	t.Run("inputs that changed while the build ran fail the build", func(t *testing.T) {
		bad := spec
		bad.ExpectedInputDigest = "sha256:" + strings.Repeat("c", 64)
		r := &receiptRunner{write: func(p string) error { return WriteReceipt(p, fixture) }}
		res := (Runner{runner: r}).Build(context.Background(), bad)
		if res.Err == nil || !strings.Contains(res.Err.Error(), "build inputs changed during execution") {
			t.Errorf("err = %v", res.Err)
		}
	})

	t.Run("a failing build never reads a receipt", func(t *testing.T) {
		r := &receiptRunner{}
		r.err = os.ErrPermission
		res := (Runner{runner: r}).Build(context.Background(), spec)
		if res.Err == nil || strings.Contains(res.Err.Error(), "receipt") {
			t.Errorf("err = %v, want the build's own error", res.Err)
		}
	})

	t.Run("a legacy build blanks an inherited receipt path", func(t *testing.T) {
		r := &receiptRunner{}
		legacy := Spec{Service: "s", ProjectDir: t.TempDir(), BuildCmd: "build"}
		t.Setenv(ReceiptPathEnv, "/outer/receipt")
		if res := (Runner{runner: r}).Build(context.Background(), legacy); res.Err != nil {
			t.Fatal(res.Err)
		}
		if v, ok := r.seen[ReceiptPathEnv]; !ok || v != "" {
			t.Errorf("%s = %q (set=%v); an outer invocation's receipt destination must never reach the child", ReceiptPathEnv, v, ok)
		}
	})
}

func TestStateBelongsTo(t *testing.T) {
	tests := []struct {
		name    string
		st      *State
		env     string
		derived string
		want    bool
	}{
		{"nil state", nil, "dev", "api", false},
		{"recorded env matches", &State{Env: "dev", Service: "api"}, "dev", "api", true},
		{"recorded env differs: a sibling env's file", &State{Env: "dev-k8s", Service: "gateway"}, "dev", "k8s-gateway", false},
		{"an empty env means default", &State{Env: "default"}, "", "x", true},
		{"an empty env does not match another env", &State{Env: "dev"}, "", "x", false},
		{"legacy file: service agrees", &State{Service: "daemon-gateway"}, "dev-k8s", "daemon-gateway", true},
		{"legacy file: filename split at the wrong hyphen", &State{Service: "daemon-gateway"}, "dev", "k8s-daemon-gateway", false},
		{"neither recorded: the filename filter is all there is", &State{}, "dev", "api", true},
		{"legacy file with no derived service", &State{Service: "api"}, "dev", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StateBelongsTo(tt.st, tt.env, tt.derived); got != tt.want {
				t.Errorf("StateBelongsTo = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestListStates(t *testing.T) {
	proj := t.TempDir()
	for _, w := range []struct{ env, service string }{
		{"dev", "api"},
		{"dev", "worker"},
		{"dev-k8s", "gateway"}, // build-dev-k8s-gateway.json also globs for env "dev"
		{"prod", "api"},
	} {
		if err := WriteState(proj, w.env, State{Service: w.service, Image: "r/" + w.service, Tag: "v1"}); err != nil {
			t.Fatal(err)
		}
	}
	// A corrupt file for env dev is skipped, not fatal.
	if err := os.WriteFile(filepath.Join(proj, ".forge", "state", "build-dev-broken.json"), []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ListStates(proj, "dev")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"api", "worker"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ListStates(dev) = %v, want %v (a sibling env that extends the name must not leak in)", got, want)
	}
	got, err = ListStates(proj, "dev-k8s")
	if err != nil || !reflect.DeepEqual(got, []string{"gateway"}) {
		t.Errorf("ListStates(dev-k8s) = (%v, %v)", got, err)
	}
	if got, _ := ListStates(proj, "staging"); len(got) != 0 {
		t.Errorf("ListStates(staging) = %v, want none", got)
	}

	// An empty env is the default env's states.
	if err := WriteState(proj, "", State{Service: "solo"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := ListStates(proj, ""); !reflect.DeepEqual(got, []string{"solo"}) {
		t.Errorf("ListStates(\"\") = %v, want [solo]", got)
	}
}

func TestWriteState_StampsTheEnv(t *testing.T) {
	proj := t.TempDir()
	if err := WriteState(proj, "prod", State{Service: "api"}); err != nil {
		t.Fatal(err)
	}
	st, err := ReadState(proj, "prod", "api")
	if err != nil || st == nil || st.Env != "prod" {
		t.Fatalf("ReadState = (%+v, %v), want Env stamped to prod", st, err)
	}
	if err := WriteState(proj, "prod", State{Service: "web", Env: "explicit"}); err != nil {
		t.Fatal(err)
	}
	if st, _ := ReadState(proj, "prod", "web"); st == nil || st.Env != "explicit" {
		t.Errorf("an explicit Env must be kept, got %+v", st)
	}
	if err := WriteState(proj, "", State{Service: "d"}); err != nil {
		t.Fatal(err)
	}
	if st, _ := ReadState(proj, "", "d"); st == nil || st.Env != "default" {
		t.Errorf("an empty env must be stamped default, got %+v", st)
	}
}

func TestReadInputs(t *testing.T) {
	dir := t.TempDir()
	d := "sha256:" + strings.Repeat("a", 64)
	in := release.BuildInputs{
		Recipe: "reliant", Sources: []release.Source{{Repo: "example/reliant", Commit: strings.Repeat("b", 40)}},
		ContextDigest: d, RecipeDigest: d, ConfigDigest: d, Tools: map[string]string{"go": d},
		Platforms: []string{"linux/amd64"}, Outputs: []string{"reliant"},
	}
	want, err := release.BuildInputDigest(in)
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	good := `{"recipe":"reliant","sources":[{"repo":"example/reliant","commit":"` + strings.Repeat("b", 40) + `"}],"context_digest":"` + d + `","recipe_digest":"` + d +
		`","config_digest":"` + d + `","tools":{"go":"` + d + `"},"platforms":["linux/amd64"],"outputs":["reliant"],"cacheable":false}`

	got, digest, err := ReadInputs(write("good.json", good))
	if err != nil {
		t.Fatalf("ReadInputs: %v", err)
	}
	if digest != want || got.Recipe != "reliant" {
		t.Errorf("digest = %q, want %q (got %+v)", digest, want, got)
	}

	if _, _, err := ReadInputs(filepath.Join(dir, "absent.json")); !os.IsNotExist(err) {
		t.Errorf("a missing file = %v, want not-exist", err)
	}
	if _, _, err := ReadInputs(write("unknown.json", strings.Replace(good, `"recipe"`, `"surprise":1,"recipe"`, 1))); err == nil {
		t.Error("an unknown field was accepted")
	}
	if _, _, err := ReadInputs(write("trailing.json", good+`{}`)); err == nil || !strings.Contains(err.Error(), "trailing data") {
		t.Errorf("trailing data = %v", err)
	}
	if _, _, err := ReadInputs(write("junk.json", "{nope")); err == nil {
		t.Error("malformed JSON was accepted")
	}
	if _, _, err := ReadInputs(write("huge.json", strings.Repeat(" ", maxReceiptBytes+1))); err == nil || !strings.Contains(err.Error(), "size bound") {
		t.Errorf("an oversized file = %v", err)
	}
	if _, _, err := ReadInputs(write("invalid.json", `{"recipe":""}`)); err == nil {
		t.Error("inputs that cannot be digested were accepted")
	}
}

func TestWriteReceipt_Refusals(t *testing.T) {
	fixture := receiptFixture(t)
	if err := WriteReceipt("", fixture); err == nil || !strings.Contains(err.Error(), ReceiptPathEnv+" is not set") {
		t.Errorf("an empty path = %v", err)
	}
	bad := fixture
	bad.Outputs = nil
	if err := WriteReceipt(filepath.Join(t.TempDir(), "r.json"), bad); err == nil {
		t.Error("an incomplete receipt was written")
	}
	if err := WriteReceipt(filepath.Join(t.TempDir(), "no-such-dir", "r.json"), fixture); err == nil {
		t.Error("writing into a directory that does not exist must fail")
	}
}

func TestReadReceipt_Refusals(t *testing.T) {
	dir := t.TempDir()
	fixture := receiptFixture(t)
	if _, err := ReadReceipt(filepath.Join(dir, "absent"), ""); err == nil || !strings.Contains(err.Error(), "read build receipt") {
		t.Errorf("a missing receipt = %v", err)
	}
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if _, err := ReadReceipt(write("junk", "{nope"), ""); err == nil || !strings.Contains(err.Error(), "decode build receipt") {
		t.Errorf("malformed receipt = %v", err)
	}
	if _, err := ReadReceipt(write("huge", strings.Repeat(" ", maxReceiptBytes+1)), ""); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("an oversized receipt = %v", err)
	}

	p := filepath.Join(dir, "ok.json")
	if err := WriteReceipt(p, fixture); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(p)
	if _, err := ReadReceipt(write("two", string(raw)+string(raw)), ""); err == nil || !strings.Contains(err.Error(), "exactly one JSON object") {
		t.Errorf("two objects = %v", err)
	}
	if _, err := ReadReceipt(p, ""); err != nil {
		t.Errorf("an empty expected digest accepts any valid receipt: %v", err)
	}
}

func TestSourceForRelease(t *testing.T) {
	var none *Source
	if got := none.ForRelease(); got != nil {
		t.Errorf("a nil source records nothing, got %+v", got)
	}
	if got := (&Source{Repo: "x/y"}).ForRelease(); got != nil {
		t.Errorf("a source with no commit records nothing, got %+v", got)
	}
	got := (&Source{Dir: "/home/me/secret/path", Repo: "x/y", Commit: strings.Repeat("c", 40), Dirty: true}).ForRelease()
	want := &release.BuildSource{Repo: "x/y", Commit: strings.Repeat("c", 40), Dirty: true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ForRelease = %+v, want %+v (and no path: a release is shared beyond this machine)", got, want)
	}
}
