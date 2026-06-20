// Package buildtarget owns the per-service build dispatch for services
// whose source lives outside the project's Go module — sibling repos,
// third-party binaries, language runtimes forge doesn't natively build.
// Mirrors internal/deploytarget for the BUILD side: a service declares
// `build_cmd` on its KCL Service, and forge runs that command via
// `sh -c` instead of the built-in Go-build pipeline.
//
// Design notes:
//
//   - Mirrors External (deploy provider) in shape — same `sh -c`
//     execution, same ${X} substitution, same skip-with-warn when the
//     source path is missing on a developer's machine. The build side
//     and deploy side are ORTHOGONAL: a service can have `build_cmd`
//     (build externally) AND `deploy = K8sCluster { ... }` (deploy to
//     in-cluster) — the typical cp-forge pattern.
//
//   - The user's command owns BOTH the build AND the push. Forge does
//     NOT run `docker push` afterwards. Matches External's "user owns
//     the command end-to-end" contract.
//
//   - Skip-with-warn (not error) when `build_cwd` doesn't exist on
//     disk — local dev pattern where the sibling repo is optional, and
//     CI without the sibling shouldn't fail.
//
// The runner is split from the dispatcher so unit tests can inject a
// fake commandRunner without spawning a real shell — the External
// provider's testing pattern.
//
// Phase 1 landed the schema + token helper. Phase 2 (this commit)
// wires Runner.Build into internal/cli/build.go and persists per-
// service state. Phase 3 adds audit + doctor surfaces.
package buildtarget

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/reliant-labs/forge/internal/deploytarget"
	"github.com/reliant-labs/forge/internal/envutil"
	"github.com/reliant-labs/forge/internal/statefile"
)

// Spec is the per-service build-target shape consumed by the runner.
// Mirrors deploytarget.ExternalSpec for the build side — the fields
// the user declares on KCL Service translate to this shape.
//
// Image is the raw Service.image string (matching External's IMAGE
// semantics — registry is composed separately via ${REGISTRY}). Tag
// is the build-resolved tag (`git describe` or --tag override) shared
// across all services in a build invocation.
type Spec struct {
	// Service is the KCL Service.name, surfaced in log lines and used
	// as the ${SERVICE} substitution token.
	Service string

	// Image is the raw Service.image string. Used as the ${IMAGE}
	// substitution token. The user composes the registry into their
	// command via ${REGISTRY}/${IMAGE}:${TAG} (matching External's
	// ${IMAGE} semantics).
	Image string

	// Tag is the build-resolved image tag (git describe or --tag
	// override). Used as the ${TAG} substitution token.
	Tag string

	// TargetArch is the resolved deploy-target GOARCH (amd64/arm64).
	// Used as the ${TARGETARCH} substitution token so user commands
	// can cross-compile and pass --platform=linux/<arch> to docker
	// buildx without re-deriving the arch.
	TargetArch string

	// Registry is the configured docker registry (from forge.yaml
	// docker.registry or the resolved push target). Used as the
	// ${REGISTRY} substitution token.
	Registry string

	// ProjectDir is the project root containing forge.yaml. Used as
	// the ${PROJECT_DIR} substitution token AND as the cwd fallback
	// when BuildCwd is empty.
	ProjectDir string

	// Env is the deploy-env name (dev/staging/prod). Used as the
	// ${ENV} substitution token so a build_cmd can branch on env (e.g.
	// a different Dockerfile or build-arg per env). Mirrors the
	// deploy-side External provider's ${ENV} token.
	Env string

	// BuildCmd is the shell command to exec via `sh -c`. Required —
	// callers should NOT construct a Spec without a build_cmd set.
	BuildCmd string

	// BuildCwd is the working directory the command runs from.
	// Relative paths are resolved against ProjectDir. Empty means
	// "use ProjectDir directly." Missing-on-disk is a warn-and-skip
	// (see Runner.Build).
	BuildCwd string

	// BuildEnv carries extra env vars merged into the command's
	// environment AND added to the substitution map (built-in tokens
	// win on conflict — same precedence External uses).
	BuildEnv map[string]string
}

// commandRunner is the same indirection the deploytarget providers
// use — Run streams stdout/stderr, RunWithEnv layers an env overlay.
// Tests swap in a fake; production uses execRunner.
//
// Kept package-private because the surface is identical to
// deploytarget's: there's no caller outside forge that would benefit
// from a public commandRunner type, and exposing it would invite
// drift from deploytarget's shape.
type commandRunner interface {
	Run(ctx context.Context, name string, args ...string) error
	RunWithEnv(ctx context.Context, env map[string]string, name string, args ...string) error
	// RunInDir runs the command with its working directory set to dir
	// (and an optional env overlay). Unlike a shell `cd <dir> && …`
	// prefix this never quotes the path through a shell, so a dir with
	// spaces or shell metacharacters is handled correctly.
	RunInDir(ctx context.Context, dir string, env map[string]string, name string, args ...string) error
}

// execRunner is the production commandRunner. Run pipes through to
// the parent stdout/stderr so users see build progress in real time.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) error {
	return execRunner{}.RunWithEnv(ctx, nil, name, args...)
}

func (execRunner) RunWithEnv(ctx context.Context, env map[string]string, name string, args ...string) error {
	return execRunner{}.RunInDir(ctx, "", env, name, args...)
}

func (execRunner) RunInDir(ctx context.Context, dir string, env map[string]string, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// Empty dir leaves cmd.Dir unset, so the command inherits the host
	// cwd — which equals ProjectDir for forge build invocations.
	cmd.Dir = dir
	if len(env) > 0 {
		cmd.Env = envutil.MergeExtraWins(os.Environ(), env)
	}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

// Vars returns the substitution map for a Spec's ${X} tokens. The
// built-in keys (IMAGE/TAG/CODE_VERSION/SERVICE/TARGETARCH/REGISTRY/
// PROJECT_DIR/ENV/BUILD_CWD) win on conflict with BuildEnv keys — same precedence
// the deploy-side External provider uses (so users carry one mental
// model across both escape hatches).
//
// Exposed so callers that want to render a preview of the
// substituted command (forge build --dry-run, forge audit) can reuse
// the same token map the runner consumes — no risk of the preview
// drifting from the actual exec.
func Vars(spec Spec) map[string]string {
	vars := map[string]string{}
	// User-declared env first so the built-ins win on conflict.
	for k, v := range spec.BuildEnv {
		vars[k] = v
	}
	vars["IMAGE"] = spec.Image
	vars["TAG"] = spec.Tag
	// CODE_VERSION mirrors TAG — the canonical version to stamp into
	// the image so the running container's reported code_version always
	// matches its tag. Same semantics as the deploy-side External token.
	vars["CODE_VERSION"] = spec.Tag
	vars["SERVICE"] = spec.Service
	vars["TARGETARCH"] = spec.TargetArch
	vars["REGISTRY"] = spec.Registry
	vars["PROJECT_DIR"] = spec.ProjectDir
	vars["ENV"] = spec.Env
	vars["BUILD_CWD"] = spec.BuildCwd
	return vars
}

// Expand substitutes the documented ${X} tokens in template against
// the Spec. Thin wrapper around deploytarget.ExpandVars + Vars(spec)
// so callers don't have to thread the var-map through themselves.
//
// Phase-1 surface: callers that just want to know "what would forge
// run" can call this without instantiating a Runner.
func Expand(template string, spec Spec) string {
	return deploytarget.ExpandVars(template, Vars(spec))
}

// Runner executes a Spec's BuildCmd via `sh -c` after substituting
// the documented ${X} tokens. Mirrors deploytarget.ExternalProvider's
// shape — same `sh -c` invocation, same env-overlay precedence, same
// "user owns the command" contract.
//
// Tests inject a fake Runner via the unexported runner field; the
// production zero-value runs commands through execRunner.
type Runner struct {
	// runner is the os/exec indirection used to invoke `sh -c`. Nil
	// falls back to execRunner. Package-private so tests in the same
	// package can swap it.
	runner commandRunner
}

// NewRunner returns a production Runner whose commandRunner is the
// real os/exec wrapper. Exposed so the forge CLI can construct one
// without poking package internals.
func NewRunner() Runner {
	return Runner{runner: execRunner{}}
}

// BuildResult is the outcome of a single Runner.Build call. Skipped
// is true when the BuildCwd was missing on disk — the dispatcher
// surfaces this differently than a real failure (warn-and-continue
// rather than abort).
//
// Tag carries the resolved tag the build actually produced so the
// caller can persist it to the per-service state file. Duration is
// the wall-clock time the user's command took so the build-summary
// line can show it alongside the docker timings.
type BuildResult struct {
	Service  string
	Tag      string
	Skipped  bool
	SkipMsg  string
	Duration time.Duration
	Err      error
}

// Build runs spec.BuildCmd through `sh -c` after substituting tokens
// and merging BuildEnv onto os.Environ(). Skip-with-warn semantics:
//
//   - spec.BuildCwd resolves against spec.ProjectDir when relative.
//   - If the resolved cwd is non-empty AND doesn't exist on disk,
//     return Skipped=true with a SkipMsg the caller logs. No error.
//     Local-dev pattern where the sibling repo is optional.
//   - Any other failure (cwd Stat error other than NotExist, exec
//     error) returns Err set; Skipped stays false.
//
// The user's BuildCmd owns BOTH the build AND the push — forge does
// not run docker push afterwards. Matches External's contract.
func (r Runner) Build(ctx context.Context, spec Spec) BuildResult {
	start := time.Now()
	result := BuildResult{
		Service: spec.Service,
		Tag:     spec.Tag,
	}
	if spec.BuildCmd == "" {
		result.Err = fmt.Errorf("build_cmd is empty (dispatcher bug — Spec without BuildCmd shouldn't reach Runner.Build)")
		result.Duration = time.Since(start)
		return result
	}

	// Resolve BuildCwd: relative paths against ProjectDir, absolute
	// passes through. Empty BuildCwd means "use ProjectDir directly"
	// — execRunner picks up the host cwd, which equals ProjectDir for
	// forge build invocations (cobra runs with cwd=project root).
	cwd := spec.BuildCwd
	if cwd != "" && !filepath.IsAbs(cwd) {
		cwd = filepath.Join(spec.ProjectDir, cwd)
	}

	// Skip-with-warn when the resolved cwd is missing on disk. CI
	// without the sibling repo, fresh checkouts of a project that
	// references an optional sibling — both should surface a clear
	// "skipped because X" message rather than failing the whole build.
	if cwd != "" {
		if _, err := os.Stat(cwd); err != nil {
			if os.IsNotExist(err) {
				result.Skipped = true
				result.SkipMsg = fmt.Sprintf("build_cwd %s does not exist on disk", cwd)
				result.Duration = time.Since(start)
				return result
			}
			result.Err = fmt.Errorf("stat build_cwd %s: %w", cwd, err)
			result.Duration = time.Since(start)
			return result
		}
	}

	expanded := Expand(spec.BuildCmd, spec)

	runner := r.runner
	if runner == nil {
		runner = execRunner{}
	}
	// Set the working directory on the runner (cmd.Dir) rather than via
	// a shell `cd <dir> && …` prefix: the latter breaks on a cwd with
	// spaces or shell metacharacters. RunInDir with an empty dir leaves
	// cmd.Dir unset, inheriting the host cwd (== ProjectDir for forge
	// build) — matching the prior no-cwd behavior.
	err := runner.RunInDir(ctx, cwd, spec.BuildEnv, "sh", "-c", expanded)
	result.Err = err
	result.Duration = time.Since(start)
	return result
}

// State is the per-service build-state record persisted after a
// successful Runner.Build. Mirrors internal/cli's BuildState shape on
// the project-docker side — but per-service so different external-
// build services can carry independent (image, tag) tuples.
//
// PushedAt is RFC3339 wall-clock. The state file is informational
// across forge invocations, so real time is fine.
type State struct {
	Service  string `json:"service"`
	Image    string `json:"image"`
	Tag      string `json:"tag"`
	Registry string `json:"registry,omitempty"`
	PushedAt string `json:"pushed_at"`
}

// statePath returns the absolute path to the per-service build-state
// file. Sits under .forge/state/build-<env>-<service>.json alongside
// the project-docker state file (.forge/state/build-<env>.json). One
// file per (env, service) so concurrent external builds of the same
// service across envs don't clobber each other.
//
// projectDir is the project root (the directory holding forge.yaml).
// Empty env collapses to "default" — same convention build_state.go
// uses on the project-docker side.
func statePath(projectDir, env, service string) string {
	if env == "" {
		env = "default"
	}
	// Sanitize the env/service segments before composing the filename.
	// They're KCL-validated identifiers in practice, so for real inputs
	// SafeSegment is a no-op and existing build-<env>-<service>.json
	// files keep loading — but a separator-bearing value used to be able
	// to escape .forge/state, which is the latent path-traversal smell
	// this hoist closes (deploytarget already sanitized; this side did
	// not).
	name := "build-" + statefile.SafeSegment(env) + "-" + statefile.SafeSegment(service) + ".json"
	return statefile.Path(projectDir, name)
}

// WriteState persists a successful Runner.Build to disk. Called from
// the build dispatcher after every successful per-service build so a
// subsequent `forge deploy <env>` can pin the same tag. Skipped
// builds DO NOT call WriteState — there's no successful tag to record.
//
// The directory is created lazily so projects that never use the
// external-build path never grow .forge/state/build-*-*.json files.
// File mode is 0o644 to match the project-docker state file.
func WriteState(projectDir, env string, state State) error {
	return statefile.Write(statePath(projectDir, env, state.Service), "build state", state)
}

// ReadState loads the per-service build-state file. Returns
// (nil, nil) when the file is missing — that's the deploy-without-
// build path (CI with a separate build job, or a fresh checkout) and
// the caller falls through to whatever default tag-resolution applies.
// Returns (nil, err) for malformed JSON or unreadable files; callers
// should not silently swallow these.
func ReadState(projectDir, env, service string) (*State, error) {
	return statefile.Read[State](statePath(projectDir, env, service), "build state")
}

// StatePath exposes the per-service state path for callers that want
// to print it (forge build's summary, forge audit) without re-deriving
// the layout. Kept exported so the path lives in one place.
func StatePath(projectDir, env, service string) string {
	return statePath(projectDir, env, service)
}
