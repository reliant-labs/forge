package cli

// The project image, assembled from forge's own build model.
//
// A project that declares `image:` in forge.yaml gets its project image built
// by forge itself (internal/imagebuild), the way ko builds one: the declared
// base pulled by digest for the target platform, a layer of the binaries forge
// just cross-compiled (each at <workdir>/<output_name>), a layer of the
// declared runtime files, and the declared config. It is pushed straight to the
// workload's registry. No Dockerfile is read and no Docker daemon is needed to
// build or push it, so `forge env deploy` works on a CI runner, in a cloud
// sandbox or inside a container exactly as on a laptop.
//
// A project without the block builds the root Dockerfile with `docker build`
// (dockerBuildProject). forge never interprets a Dockerfile: an image that
// needs RUN steps is docker's (or a remote builder's) to build.

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/reliant-labs/forge/internal/cliutil"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/imagebuild"
)

// dockerDaemonOnce/dockerDaemonResult are the cached answer to "does a Docker daemon answer". It is
// asked once per process because a build asks for every image it makes, and a
// probe that waits out a dead Docker Desktop proxy costs seconds each time.
var (
	dockerDaemonOnce   sync.Once
	dockerDaemonResult error
)

// dockerDaemonReachable is a seam so tests state the answer instead of probing
// the host (the suite's tripwire fails any test that reaches the real docker).
var dockerDaemonReachable = func(ctx context.Context) error {
	dockerDaemonOnce.Do(func() { dockerDaemonResult = ensureDockerDaemon(ctx) })
	return dockerDaemonResult
}

// dockerLoad is the seam the model path loads an unpushed image through:
// `docker load` reading the tarball on stdin.
var dockerLoad = func(ctx context.Context, write func(io.Writer) error) error {
	cmd := exec.CommandContext(ctx, "docker", "load")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	werr := write(stdin)
	_ = stdin.Close()
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("docker load: %w", err)
	}
	return werr
}

// requireDaemonForDockerImage is the guard in front of every image forge
// builds with `docker build` (the root Dockerfile, a DockerBuild workload, a
// frontend image): without a daemon it fails once, naming the ways out,
// instead of docker's own "failed to connect to the docker API".
func requireDaemonForDockerImage(ctx context.Context, what, dockerfile, remedy string) error {
	err := dockerDaemonReachable(ctx)
	if err == nil {
		return nil
	}
	hint := "this image is built with `docker build`. Either:\n" +
		"    - run it where a Docker daemon is reachable (start Docker, or set DOCKER_HOST), or\n" +
		"    - build it remotely: build = forge.RemoteBuild (declared in the schema; this forge cannot submit one yet)"
	if remedy != "" {
		hint += ", or\n    - " + remedy
	}
	return cliutil.UserErr("forge build",
		fmt.Sprintf("%s is built with `docker build -f %s`, which needs a Docker daemon, and none is available (%s)", what, dockerfile, firstLine(err.Error())),
		dockerfile, hint)
}

// projectImageRemedy is the way out a project image built from the root
// Dockerfile has that other docker images do not.
const projectImageRemedy = "declare the image in forge.yaml (`image: {base: <ref@sha256:…>}`) and forge assembles it from the binaries it built, without Docker (load skill deploy/project-image)"

// projectImageSpec is the imagebuild.Spec forge.yaml's `image:` block and the
// host go-build describe.
func projectImageSpec(cfg *config.ProjectConfig, goTargets []goBuildTarget, outputDir, arch string, created time.Time) (imagebuild.Spec, error) {
	ic := cfg.Image
	workdir := ic.EffectiveWorkdir()
	bins := map[string]string{}
	var names []string
	for _, t := range goTargets {
		dest := path.Join(workdir, t.outputName)
		if _, dup := bins[dest]; dup {
			continue
		}
		bins[dest] = filepath.Join(outputDir, t.outputName)
		names = append(names, t.outputName)
	}
	entrypoint := ic.Entrypoint
	if len(entrypoint) == 0 {
		primary, err := primaryImageBinary(cfg.Name, names)
		if err != nil {
			return imagebuild.Spec{}, err
		}
		entrypoint = []string{path.Join(workdir, primary)}
	}
	files := map[string]string{}
	for src, dest := range ic.Files {
		files[dest] = filepath.FromSlash(src)
	}
	return imagebuild.Spec{
		Base:       ic.Base,
		Platform:   imagePlatform(arch),
		Workdir:    workdir,
		User:       ic.EffectiveUser(),
		Entrypoint: entrypoint,
		Cmd:        ic.Cmd,
		Env:        ic.Env,
		Binaries:   bins,
		Files:      files,
		Created:    created,
	}, nil
}

// primaryImageBinary is the binary the default entrypoint runs: the one named
// after the project, else the only one.
func primaryImageBinary(project string, names []string) (string, error) {
	for _, n := range names {
		if n == project {
			return n, nil
		}
	}
	if len(names) == 1 {
		return names[0], nil
	}
	sort.Strings(names)
	return "", cliutil.UserErr("forge build",
		fmt.Sprintf("the project image has %d binaries (%s) and none is named %q, so forge cannot choose its entrypoint", len(names), strings.Join(names, ", "), project),
		"forge.yaml",
		"set image.entrypoint in forge.yaml, e.g. entrypoint: [/app/<binary>]")
}

// imageBuildEpoch is the build's SOURCE_DATE_EPOCH: the same value the docker
// path passes (the last commit to the repository), so a rebuild of unchanged
// inputs keeps its digest. Outside git, the Unix epoch.
func imageBuildEpoch(ctx context.Context) time.Time {
	if v := resolveSourceDateEpoch(ctx, "."); v != "" {
		if sec, err := strconv.ParseInt(v, 10, 64); err == nil {
			return time.Unix(sec, 0).UTC()
		}
	}
	return time.Unix(0, 0).UTC()
}

// imagePlatform is the platform of an image built for GOARCH arch. forge builds
// container images for linux only.
func imagePlatform(arch string) v1.Platform {
	if arch == "" {
		arch = hostArch
	}
	return v1.Platform{OS: "linux", Architecture: arch}
}

// projectImageFromModel builds the project image from forge.yaml's `image:`
// block and pushes it (or, with nothing to push, loads it into the local
// daemon). The buildResult carries the same fields the docker path fills, so
// the build state, the release ledger and the deploy pin read it unchanged.
func projectImageFromModel(ctx context.Context, cfg *config.ProjectConfig, tags dockerImageTags, crossArch string, guard projectImageGuard) buildResult {
	start := time.Now()
	fail := func(err error) buildResult {
		return buildResult{name: cfg.Name + " (docker)", kind: "docker", duration: time.Since(start), err: err}
	}
	if err := assertProjectImageBinaries(guard.outputDir, crossArch, guard.envLabel, guard.binaryNames); err != nil {
		return fail(err)
	}
	refs := dedupeRefs(tags.push)
	if len(refs) == 0 {
		// Nothing to push: the image is for the local daemon.
		if err := dockerDaemonReachable(ctx); err != nil {
			return fail(cliutil.UserErr("forge build",
				fmt.Sprintf("the project image has nowhere to go: nothing is being pushed and no Docker daemon is available to load it into (%s)", firstLine(err.Error())),
				"",
				"push it: forge env build <env> --push (or forge env deploy <env>), or run where a Docker daemon is reachable"))
		}
	}
	spec, err := projectImageSpec(cfg, guard.goTargets, guard.outputDir, crossArch, imageBuildEpoch(ctx))
	if err != nil {
		return fail(err)
	}
	fmt.Printf("[build] %s: assembling image for %s from forge.yaml image (base %s)\n",
		cfg.Name, imagebuild.PlatformString(spec.Platform), spec.Base)
	res, err := imagebuild.Build(ctx, spec)
	if err != nil {
		return fail(err)
	}
	if res.Base != spec.Base {
		fmt.Printf("[build]   base %s\n", res.Base)
	}

	if len(refs) == 0 {
		fmt.Printf("[build] %s: docker load (%d tags)\n", cfg.Name, len(tags.local))
		if err := dockerLoad(ctx, func(w io.Writer) error { return imagebuild.WriteTarball(w, res.Image, dedupeRefs(tags.local)) }); err != nil {
			return fail(err)
		}
		return buildResult{
			name: cfg.Name + " (docker)", kind: "docker", duration: time.Since(start),
			image: cfg.Name, repository: tags.repository, tag: tags.tag,
		}
	}
	for _, t := range refs {
		fmt.Printf("[build] %s: push %s\n", cfg.Name, t)
	}
	if err := pushImage(ctx, res.Image, refs); err != nil {
		return fail(err)
	}
	fmt.Printf("[build] %s: pushed digest %s\n", cfg.Name, res.Digest)
	return buildResult{
		name:       cfg.Name + " (docker)",
		kind:       "docker",
		duration:   time.Since(start),
		image:      cfg.Name,
		repository: tags.repository,
		tag:        tags.tag,
		digest:     res.Digest.String(),
		platforms:  []string{imagebuild.PlatformString(spec.Platform)},
	}
}

// dedupeRefs drops repeated references, keeping order. imageTagSet lists
// repo:latest twice when the resolved tag is itself "latest", and a
// registry write is not free.
func dedupeRefs(refs []string) []string {
	seen := make(map[string]bool, len(refs))
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		if !seen[r] {
			seen[r] = true
			out = append(out, r)
		}
	}
	return out
}

// pushImage pushes img to refs with the docker credential store, and appends
// the realm-mismatch hint when the registry refuses the push, exactly as
// dockerPush does.
func pushImage(ctx context.Context, img v1.Image, refs []string) error {
	if err := imagebuild.Push(ctx, img, refs, nil); err != nil {
		var hint string
		for _, r := range refs {
			if h := deniedPushHint(err, "", r); h != "" {
				hint = h
				break
			}
		}
		return fmt.Errorf("%w%s", err, hint)
	}
	return nil
}
