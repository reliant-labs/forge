package buildtarget

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeRunner captures the most recent RunWithEnv invocation so tests
// can assert what `sh -c <expanded>` would have run without spawning
// a real shell. Mirrors the deploytarget tests' fake — same minimum
// shape, no shared interface (the packages are independent).
type fakeRunner struct {
	mu    sync.Mutex
	calls []fakeCall
	err   error // optional canned error returned by RunWithEnv
}

type fakeCall struct {
	dir  string
	env  map[string]string
	name string
	args []string
}

func (f *fakeRunner) Run(_ context.Context, name string, args ...string) error {
	return f.RunWithEnv(nil, nil, name, args...)
}

func (f *fakeRunner) RunWithEnv(ctx context.Context, env map[string]string, name string, args ...string) error {
	return f.RunInDir(ctx, "", env, name, args...)
}

func (f *fakeRunner) RunInDir(_ context.Context, dir string, env map[string]string, name string, args ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fakeCall{dir: dir, env: env, name: name, args: append([]string(nil), args...)})
	return f.err
}

func (f *fakeRunner) last() (fakeCall, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return fakeCall{}, false
	}
	return f.calls[len(f.calls)-1], true
}

// TestVars_BuiltinsWin confirms a Spec's built-in tokens
// (IMAGE/TAG/SERVICE/TARGETARCH/REGISTRY/PROJECT_DIR/BUILD_CWD) win
// against any conflicting BuildEnv key — same precedence External
// uses, so users carry one mental model across the two escape
// hatches.
func TestVars_BuiltinsWin(t *testing.T) {
	spec := Spec{
		Service:    "daemon-gateway",
		Image:      "reliant-daemon-gateway",
		Tag:        "v1.2.3",
		TargetArch: "amd64",
		Registry:   "localhost:5051",
		ProjectDir: "/home/dev/cp-forge",
		BuildCwd:   "../reliant",
		BuildEnv: map[string]string{
			// User-declared overlay; built-ins must win on conflict.
			"IMAGE":  "user-shadow-attempt",
			"TAG":    "user-shadow-attempt",
			"CUSTOM": "user-custom",
		},
	}
	got := Vars(spec)
	if got["IMAGE"] != "reliant-daemon-gateway" {
		t.Errorf("IMAGE: want reliant-daemon-gateway, got %q", got["IMAGE"])
	}
	if got["TAG"] != "v1.2.3" {
		t.Errorf("TAG: want v1.2.3, got %q", got["TAG"])
	}
	if got["SERVICE"] != "daemon-gateway" {
		t.Errorf("SERVICE: want daemon-gateway, got %q", got["SERVICE"])
	}
	if got["TARGETARCH"] != "amd64" {
		t.Errorf("TARGETARCH: want amd64, got %q", got["TARGETARCH"])
	}
	if got["REGISTRY"] != "localhost:5051" {
		t.Errorf("REGISTRY: want localhost:5051, got %q", got["REGISTRY"])
	}
	if got["PROJECT_DIR"] != "/home/dev/cp-forge" {
		t.Errorf("PROJECT_DIR: want /home/dev/cp-forge, got %q", got["PROJECT_DIR"])
	}
	if got["BUILD_CWD"] != "../reliant" {
		t.Errorf("BUILD_CWD: want ../reliant, got %q", got["BUILD_CWD"])
	}
	if got["CUSTOM"] != "user-custom" {
		t.Errorf("CUSTOM (user-declared): want user-custom, got %q", got["CUSTOM"])
	}
}

// TestVars_CodeVersionAndEnv pins the two External-mirror tokens added
// for the External.build_cmd feature: ${CODE_VERSION} (== ${TAG}, the
// version to stamp into the image) and ${ENV} (the deploy env name).
// These let an External build_cmd carry the same provenance the
// deploy-side deploy_cmd does.
func TestVars_CodeVersionAndEnv(t *testing.T) {
	spec := Spec{
		Tag: "forge-test",
		Env: "prod",
	}
	got := Vars(spec)
	if got["CODE_VERSION"] != "forge-test" {
		t.Errorf("CODE_VERSION: want forge-test (==TAG), got %q", got["CODE_VERSION"])
	}
	if got["ENV"] != "prod" {
		t.Errorf("ENV: want prod, got %q", got["ENV"])
	}
}

// TestExpand_ExternalBuildShape validates the substitution against the
// exact build_cmd the kalshi-trader e2e declares on its External target
// — the build-side mirror of deploy_cmd. Pins that ${IMAGE} ${TAG}
// ${PROJECT_DIR} ${TARGETARCH} all resolve in one shell string.
func TestExpand_ExternalBuildShape(t *testing.T) {
	spec := Spec{
		Image:      "ghcr.io/kalshi-trader",
		Tag:        "forge-test",
		TargetArch: "amd64",
		ProjectDir: "/Users/x/src/kalshi-trader",
		Env:        "prod",
	}
	template := `docker build --platform linux/${TARGETARCH} -t ${IMAGE}:${TAG} -f ${PROJECT_DIR}/Dockerfile ${PROJECT_DIR}`
	want := `docker build --platform linux/amd64 -t ghcr.io/kalshi-trader:forge-test -f /Users/x/src/kalshi-trader/Dockerfile /Users/x/src/kalshi-trader`
	if got := Expand(template, spec); got != want {
		t.Errorf("Expand:\n got %q\nwant %q", got, want)
	}
}

// TestExpand_CPForgeShape validates the substitution against the
// exact shape cp-forge's scripts/cloud-dev.sh:build_daemon_gateway_image
// helper builds. This is the canonical real-world use case the feature
// is designed to subsume; pinning the expansion here means any future
// change to the var set or precedence is caught at unit time before
// downstream projects pick it up.
func TestExpand_CPForgeShape(t *testing.T) {
	spec := Spec{
		Service:    "daemon-gateway",
		Image:      "reliant-daemon-gateway",
		Tag:        "abc1234-dirty",
		TargetArch: "arm64",
		Registry:   "localhost:5051",
		ProjectDir: "/home/dev/cp-forge",
		BuildCwd:   "../reliant",
	}
	template := `cd ${BUILD_CWD} && ` +
		`GOOS=linux GOARCH=${TARGETARCH} CGO_ENABLED=0 go build -o /tmp/bin ./cmd/reliant && ` +
		`docker build --platform=linux/${TARGETARCH} -t ${REGISTRY}/${IMAGE}:${TAG} ` +
		`-f ${PROJECT_DIR}/docker/Dockerfile.daemon-gateway.dev /tmp && ` +
		`docker push ${REGISTRY}/${IMAGE}:${TAG}`

	got := Expand(template, spec)
	want := `cd ../reliant && ` +
		`GOOS=linux GOARCH=arm64 CGO_ENABLED=0 go build -o /tmp/bin ./cmd/reliant && ` +
		`docker build --platform=linux/arm64 -t localhost:5051/reliant-daemon-gateway:abc1234-dirty ` +
		`-f /home/dev/cp-forge/docker/Dockerfile.daemon-gateway.dev /tmp && ` +
		`docker push localhost:5051/reliant-daemon-gateway:abc1234-dirty`
	if got != want {
		t.Errorf("Expand:\n want: %s\n  got: %s", want, got)
	}
}

// TestExpand_UnknownTokensEmpty pins the "unknown ${X} substitutes to
// empty string" behaviour. A typo in the user's build_cmd surfaces as
// a missing flag rather than a leaked literal landing on the shell —
// the safer of the two failure modes.
func TestExpand_UnknownTokensEmpty(t *testing.T) {
	spec := Spec{Service: "x", Image: "x", Tag: "v1"}
	got := Expand("echo ${TYPO} done", spec)
	if got != "echo  done" {
		t.Errorf("unknown ${X}: want %q, got %q", "echo  done", got)
	}
}

// TestExpand_UserEnvAvailable confirms BuildEnv keys land in the
// substitution map alongside the built-ins — same shape External's
// `env` block carries. Lets users pass deploy-target-specific knobs
// (region, profile, tenant) into their build_cmd without env-var
// gymnastics.
func TestExpand_UserEnvAvailable(t *testing.T) {
	spec := Spec{
		Service: "edge",
		Image:   "edge",
		Tag:     "v1",
		BuildEnv: map[string]string{
			"REGION": "us-east-1",
			"TENANT": "acme",
		},
	}
	got := Expand("build --region ${REGION} --tenant ${TENANT}", spec)
	want := "build --region us-east-1 --tenant acme"
	if got != want {
		t.Errorf("user env: want %q, got %q", want, got)
	}
}

// TestMergeEnv_ExtraWins pins the env-overlay precedence the runner
// uses: extra (BuildEnv) wins on key conflict over the inherited
// os.Environ. Mirrors deploytarget.mergeEnv, kept as a duplicate
// because the two packages run in different dispatcher tables and
// importing one from the other would couple build/deploy concerns
// the codebase deliberately keeps separate.
func TestMergeEnv_ExtraWins(t *testing.T) {
	base := []string{"PATH=/usr/bin", "FOO=base-foo", "BAR=base-bar"}
	extra := map[string]string{
		"FOO":     "extra-foo",
		"NEW_KEY": "extra-new",
	}
	merged := mergeEnv(base, extra)
	got := map[string]string{}
	for _, kv := range merged {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		got[kv[:eq]] = kv[eq+1:]
	}
	if got["FOO"] != "extra-foo" {
		t.Errorf("FOO: want extra-foo (overlay wins), got %q", got["FOO"])
	}
	if got["BAR"] != "base-bar" {
		t.Errorf("BAR: want base-bar (unchanged), got %q", got["BAR"])
	}
	if got["NEW_KEY"] != "extra-new" {
		t.Errorf("NEW_KEY: want extra-new, got %q", got["NEW_KEY"])
	}
	if got["PATH"] != "/usr/bin" {
		t.Errorf("PATH: want /usr/bin (unchanged), got %q", got["PATH"])
	}
}

// TestMergeEnv_EmptyExtra confirms the no-overlay path returns a
// fresh slice (not the base aliased) so callers can mutate the result
// without surprising other code holding the base slice.
func TestMergeEnv_EmptyExtra(t *testing.T) {
	base := []string{"PATH=/usr/bin", "FOO=bar"}
	merged := mergeEnv(base, nil)
	if len(merged) != 2 {
		t.Fatalf("len: want 2, got %d", len(merged))
	}
	// Mutating the result must not touch the base.
	merged[0] = "PATH=/tmp"
	if base[0] != "PATH=/usr/bin" {
		t.Errorf("base was aliased: %s", base[0])
	}
}

// TestBuild_ExpandsAndExecs pins the happy path: tokens substitute,
// BuildEnv flows through to the runner env overlay, the final command
// is wrapped with `cd <abs> && <expanded>` when BuildCwd is set, and
// the result reports success.
func TestBuild_ExpandsAndExecs(t *testing.T) {
	projDir := t.TempDir()
	// Create the build_cwd so the runner doesn't trigger skip-with-warn.
	cwd := filepath.Join(projDir, "sibling")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatalf("mkdir cwd: %v", err)
	}

	fake := &fakeRunner{}
	r := Runner{runner: fake}
	spec := Spec{
		Service:    "daemon-gateway",
		Image:      "reliant-daemon-gateway",
		Tag:        "v1.2.3",
		TargetArch: "arm64",
		Registry:   "localhost:5051",
		ProjectDir: projDir,
		BuildCwd:   "sibling",
		BuildCmd:   "docker build -t ${REGISTRY}/${IMAGE}:${TAG} .",
		BuildEnv: map[string]string{
			"REGION": "us-east-1",
		},
	}
	res := r.Build(context.Background(), spec)
	if res.Err != nil {
		t.Fatalf("Build: unexpected err: %v", res.Err)
	}
	if res.Skipped {
		t.Fatal("Build: unexpected skip")
	}
	if res.Service != "daemon-gateway" {
		t.Errorf("Service: got %q, want daemon-gateway", res.Service)
	}
	if res.Tag != "v1.2.3" {
		t.Errorf("Tag: got %q, want v1.2.3", res.Tag)
	}

	call, ok := fake.last()
	if !ok {
		t.Fatal("runner was not invoked")
	}
	if call.name != "sh" {
		t.Errorf("runner name: got %q, want sh", call.name)
	}
	if len(call.args) != 2 || call.args[0] != "-c" {
		t.Fatalf("runner args: got %v, want [-c <cmd>]", call.args)
	}
	// The working directory is carried as cmd.Dir (call.dir), NOT a
	// shell `cd <cwd> && …` prefix — so a path with spaces or shell
	// metacharacters can never break the script.
	wantCmd := "docker build -t localhost:5051/reliant-daemon-gateway:v1.2.3 ."
	if call.args[1] != wantCmd {
		t.Errorf("expanded cmd: got %q, want %q", call.args[1], wantCmd)
	}
	if call.dir != cwd {
		t.Errorf("runner dir: got %q, want %q", call.dir, cwd)
	}
	if strings.Contains(call.args[1], "cd ") {
		t.Errorf("expanded cmd should not carry a `cd …` shell prefix; got %q", call.args[1])
	}
	if call.env["REGION"] != "us-east-1" {
		t.Errorf("env REGION: got %q, want us-east-1", call.env["REGION"])
	}
}

// TestBuild_SkipsWhenCwdMissing locks in the local-dev contract: a
// missing build_cwd is a warn-skip (Skipped=true, no Err) rather than
// a hard failure. CI without the sibling repo, fresh checkouts of an
// optional sibling — both must surface as a clean skip.
func TestBuild_SkipsWhenCwdMissing(t *testing.T) {
	projDir := t.TempDir()
	fake := &fakeRunner{}
	r := Runner{runner: fake}
	spec := Spec{
		Service:    "edge",
		Image:      "edge",
		Tag:        "v1",
		ProjectDir: projDir,
		BuildCwd:   "does-not-exist",
		BuildCmd:   "docker build .",
	}
	res := r.Build(context.Background(), spec)
	if res.Err != nil {
		t.Fatalf("Build: unexpected err on missing cwd: %v", res.Err)
	}
	if !res.Skipped {
		t.Fatal("Build: want Skipped=true when build_cwd missing")
	}
	if !strings.Contains(res.SkipMsg, "does not exist") {
		t.Errorf("SkipMsg: got %q, want a 'does not exist' message", res.SkipMsg)
	}
	if _, ok := fake.last(); ok {
		t.Error("runner should not have been invoked for a skipped build")
	}
}

// TestBuild_NoCwd confirms an empty BuildCwd runs the command without
// the `cd <abs> && ` prefix. The execRunner inherits the current
// process cwd (set to the project root by forge build), so omitting
// the prefix is the right shape.
func TestBuild_NoCwd(t *testing.T) {
	fake := &fakeRunner{}
	r := Runner{runner: fake}
	spec := Spec{
		Service:    "x",
		Image:      "x",
		Tag:        "v1",
		ProjectDir: "/proj",
		BuildCmd:   "echo ${IMAGE}",
	}
	res := r.Build(context.Background(), spec)
	if res.Err != nil {
		t.Fatalf("Build: unexpected err: %v", res.Err)
	}
	call, _ := fake.last()
	if call.dir != "" {
		t.Errorf("no BuildCwd should leave runner dir empty; got %q", call.dir)
	}
	if strings.HasPrefix(call.args[1], "cd ") {
		t.Errorf("no BuildCwd should not produce a `cd …` prefix; got %q", call.args[1])
	}
	if call.args[1] != "echo x" {
		t.Errorf("expanded: got %q, want %q", call.args[1], "echo x")
	}
}

// TestBuild_EmptyBuildCmdIsDispatcherBug pins the contract: callers
// should NOT construct a Spec without BuildCmd set — the dispatcher
// filters those out before reaching Runner.Build. If one slips through,
// surface a loud error rather than silently no-op.
func TestBuild_EmptyBuildCmdIsDispatcherBug(t *testing.T) {
	r := Runner{runner: &fakeRunner{}}
	spec := Spec{Service: "x", Image: "x", Tag: "v1"} // BuildCmd left empty
	res := r.Build(context.Background(), spec)
	if res.Err == nil {
		t.Fatal("Build: expected error for empty BuildCmd")
	}
	if !strings.Contains(res.Err.Error(), "dispatcher bug") {
		t.Errorf("Err: got %v, want a 'dispatcher bug' message", res.Err)
	}
}

// TestWriteAndReadState round-trips a State through disk to confirm
// the per-service file layout and JSON shape. Pins the path so a
// future refactor of statePath catches the consumer (forge deploy
// reads the file by path and would silently miss a relocation).
func TestWriteAndReadState(t *testing.T) {
	projDir := t.TempDir()
	want := State{
		Service:  "daemon-gateway",
		Image:    "reliant-daemon-gateway",
		Tag:      "v1.2.3",
		Registry: "localhost:5051",
		PushedAt: "2026-06-05T16:00:00Z",
	}
	if err := WriteState(projDir, "dev", want); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
	// Confirm the path layout — one file per (env, service).
	wantPath := filepath.Join(projDir, ".forge", "state", "build-dev-daemon-gateway.json")
	if _, err := os.Stat(wantPath); err != nil {
		t.Fatalf("state file at %s: %v", wantPath, err)
	}
	// And that the on-disk shape is JSON (round-trip via the public API).
	got, err := ReadState(projDir, "dev", "daemon-gateway")
	if err != nil {
		t.Fatalf("ReadState: %v", err)
	}
	if got == nil {
		t.Fatal("ReadState: got nil for an existing file")
	}
	if got.Service != want.Service || got.Image != want.Image || got.Tag != want.Tag ||
		got.Registry != want.Registry || got.PushedAt != want.PushedAt {
		t.Errorf("round-trip mismatch:\n got: %+v\nwant: %+v", *got, want)
	}
	// Verify ReadState returns (nil, nil) for a missing file —
	// the "deploy without build" path the dispatcher relies on.
	none, nerr := ReadState(projDir, "dev", "missing-service")
	if nerr != nil {
		t.Errorf("ReadState missing file: unexpected err %v", nerr)
	}
	if none != nil {
		t.Errorf("ReadState missing file: want nil, got %+v", *none)
	}
}

// TestStatePath_EmptyEnvCollapsesToDefault pins the path-stability
// contract: an empty env still produces a usable, stable filename so
// projects that haven't migrated to per-env state can still write one
// canonical file rather than landing on path/build--service.json.
func TestStatePath_EmptyEnvCollapsesToDefault(t *testing.T) {
	got := StatePath("/proj", "", "svc")
	want := filepath.Join("/proj", ".forge", "state", "build-default-svc.json")
	if got != want {
		t.Errorf("StatePath empty env: got %q, want %q", got, want)
	}
}

// TestWriteState_JSONShape pins the on-disk JSON shape — fields are
// snake_case so a user can eyeball the file by hand without a
// formatter. A future serializer change (camel-case, type rename)
// trips this test before it ships and breaks downstream consumers.
func TestWriteState_JSONShape(t *testing.T) {
	projDir := t.TempDir()
	state := State{
		Service:  "edge",
		Image:    "edge",
		Tag:      "v1",
		Registry: "localhost:5051",
		PushedAt: "2026-06-05T16:00:00Z",
	}
	if err := WriteState(projDir, "dev", state); err != nil {
		t.Fatalf("WriteState: %v", err)
	}
	data, err := os.ReadFile(StatePath(projDir, "dev", "edge"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var raw map[string]string
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, k := range []string{"service", "image", "tag", "registry", "pushed_at"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("missing key %q in: %s", k, string(data))
		}
	}
}
