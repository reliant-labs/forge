package cli

// The daemonless image build.
//
// `docker build` is one of two ways forge makes the project image. The other
// assembles it in-process (internal/imagebuild): the pinned base image from the
// Dockerfile's final stage, plus a layer holding the binaries forge already
// cross-compiled on the host, plus that stage's config. No Docker daemon is
// involved, in the build or the push, so `forge env deploy` works on a machine
// that has none (a CI runner, a cloud sandbox, a container).
//
// WHEN IT RUNS (--image-builder, default auto):
//
//	docker       always `docker build`; fail if there is no daemon.
//	in-process   always assemble in-process; fail if the Dockerfile needs a
//	             daemon.
//	auto         docker when a daemon answers; otherwise in-process when the
//	             Dockerfile's final stage is one it can model; otherwise an
//	             error naming both ways out.
//
// A reachable daemon keeps winning under auto on purpose. Both builders produce
// the same image for a forge Dockerfile, but "the thing the author has always
// run" is the safe default; the in-process builder is what makes the daemon
// optional, not what replaces it.
//
// WHAT IT REFUSES. The interpreter is strict (imagebuild.UnsupportedError): a
// RUN that executes code, an ADD, a COPY --from an image, a bind mount — each
// is an error naming the Dockerfile line, never a skip. A Dockerfile the author
// has customized past forge's scaffold therefore falls back to docker, or fails
// with that line, instead of shipping an image that differs from what `docker
// build` would have made.

import (
	"context"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/reliant-labs/forge/internal/cliutil"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/imagebuild"
)

// Image-builder modes. The empty string is auto.
const (
	imageBuilderAuto      = "auto"
	imageBuilderDocker    = "docker"
	imageBuilderInProcess = "in-process"
)

const imageBuilderFlag = "image-builder"

const imageBuilderUsage = "How the project image is built: auto (default: docker when a daemon is reachable, otherwise assembled in-process from the binaries forge just built), docker (always docker build), in-process (never docker; fails if the Dockerfile's final stage needs one)"

// validateImageBuilder rejects an unknown --image-builder value.
func validateImageBuilder(mode string) error {
	switch mode {
	case "", imageBuilderAuto, imageBuilderDocker, imageBuilderInProcess:
		return nil
	}
	return cliutil.UserErr("--image-builder",
		fmt.Sprintf("unknown image builder %q", mode), "",
		"use one of: auto (docker when a daemon is reachable, else in-process), docker, in-process")
}

// dockerDaemonUp is the cached answer to "does a Docker daemon answer". It is
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

// inProcessPlan is the decision for one image.
type inProcessPlan struct {
	use bool
	// err is non-nil when the build cannot proceed at all (no daemon and the
	// Dockerfile is not one the in-process builder can assemble).
	err error
	// why is the one-line reason printed when the in-process builder is used.
	why string
}

// chooseImageBuilder decides docker vs in-process for the Dockerfile at
// dockerfilePath. check reports whether the in-process builder can assemble it.
func chooseImageBuilder(ctx context.Context, mode, dockerfilePath string, check func() error) inProcessPlan {
	switch mode {
	case imageBuilderDocker:
		return inProcessPlan{}
	case imageBuilderInProcess:
		if err := check(); err != nil {
			return inProcessPlan{err: noDaemonlessBuildError(dockerfilePath, err, "--image-builder in-process was requested")}
		}
		return inProcessPlan{use: true, why: "--image-builder in-process"}
	}
	daemonErr := dockerDaemonReachable(ctx)
	if daemonErr == nil {
		return inProcessPlan{}
	}
	if err := check(); err != nil {
		return inProcessPlan{err: noDaemonlessBuildError(dockerfilePath, err, firstLine(daemonErr.Error()))}
	}
	return inProcessPlan{use: true, why: "no Docker daemon (" + firstLine(daemonErr.Error()) + ")"}
}

// noDaemonlessBuildError is the error for an image that needs a daemon nobody
// has. It names every way out, because which one is right depends on who is
// asking: a laptop user starts Docker, a sandbox user cannot.
func noDaemonlessBuildError(dockerfilePath string, why error, context string) error {
	return cliutil.UserErr("forge build",
		fmt.Sprintf("%s needs a Docker daemon to build, and none is available (%s)", dockerfilePath, context),
		dockerfilePath,
		fmt.Sprintf("%v\n"+
			"  forge assembles an image without Docker only for the Dockerfile it scaffolds: a final stage that COPYs the binaries forge already built and sets ENTRYPOINT/USER/ENV/EXPOSE/HEALTHCHECK.\n"+
			"  to build this one, either:\n"+
			"    - run it where a Docker daemon is reachable (start Docker, or set DOCKER_HOST), or\n"+
			"    - build it remotely: a workload with build = forge.RemoteBuild (declared in the schema; this forge cannot submit one yet), or\n"+
			"    - bring the final stage back to the scaffold's shape (move RUN steps into the builder stage or a base image); --image-builder in-process requires that path", why))
}

// requireDaemonForDockerOnlyImage is the guard in front of every image forge still builds
// ONLY with `docker build` (a DockerBuild workload, a frontend image): without
// a daemon it fails once, naming both ways out, instead of docker's own
// "failed to connect to the docker API".
func requireDaemonForDockerOnlyImage(ctx context.Context, what, dockerfile string) error {
	err := dockerDaemonReachable(ctx)
	if err == nil {
		return nil
	}
	return cliutil.UserErr("forge build",
		fmt.Sprintf("%s is built with `docker build -f %s`, which needs a Docker daemon, and none is available (%s)", what, dockerfile, firstLine(err.Error())),
		dockerfile,
		"forge builds without Docker only the project image from the Dockerfile it scaffolds. For this one, either:\n"+
			"    - run it where a Docker daemon is reachable (start Docker, or set DOCKER_HOST), or\n"+
			"    - build it remotely: build = forge.RemoteBuild (declared in the schema; this forge cannot submit one yet)")
}

// projectImageBinaries maps the name a binary has inside the image to the file
// the host go-build wrote for it. The scaffolded builder stage compiles each
// cmd/<dir> to /app/bin/<dir>, while forge's host build names its output after
// the workload (GoBuild.output_name), so both spellings are registered.
func projectImageBinaries(goTargets []goBuildTarget) map[string]string {
	m := map[string]string{}
	for _, t := range goTargets {
		m[t.outputName] = t.outputName
	}
	for _, t := range goTargets {
		if _, taken := m[path.Base(t.cmd)]; !taken {
			m[path.Base(t.cmd)] = t.outputName
		}
	}
	return m
}

// projectImageArtifacts maps the builder stage's binary paths to the files the
// host go-build wrote, so `COPY --from=builder /app/bin/<bin> ./<bin>` reads
// the cross-compiled binary instead of running a compiler in the image.
// binaries is projectImageBinaries' result.
func projectImageArtifacts(df []byte, outputDir string, binaries map[string]string) map[string]map[string]string {
	out := map[string]map[string]string{}
	for _, stage := range builderStageNames(df) {
		m := map[string]string{}
		for inImage, built := range binaries {
			p := filepath.Join(outputDir, built)
			if _, err := os.Stat(p); err == nil {
				m[path.Join("/app/bin", inImage)] = p
			}
		}
		out[stage] = m
	}
	return out
}

// builderStageNames is the named stages of a Dockerfile other than its last: the
// stages whose output the final stage copies from.
func builderStageNames(df []byte) []string {
	var names []string
	last := ""
	for _, line := range strings.Split(string(df), "\n") {
		f := strings.Fields(strings.TrimSpace(line))
		if len(f) == 0 || !strings.EqualFold(f[0], "FROM") {
			continue
		}
		if last != "" {
			names = append(names, last)
		}
		last = ""
		if len(f) == 4 && strings.EqualFold(f[2], "AS") {
			last = f[3]
		}
	}
	return names
}

// imageBuildEpoch is the build's SOURCE_DATE_EPOCH: the same value the docker
// path passes (last commit touching the build's inputs), so a rebuild of
// unchanged inputs keeps its digest. Outside git, the epoch.
func imageBuildEpoch(ctx context.Context, inputs ...string) time.Time {
	if v := resolveSourceDateEpoch(ctx, ".", inputs...); v != "" {
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

// projectImageInProcess builds and pushes the project image without a daemon.
// It returns ok=false when the docker builder should run instead. The returned
// buildResult carries the same fields the docker path fills, so the build
// state, the release ledger and the deploy pin read it unchanged.
func projectImageInProcess(ctx context.Context, cfg *config.ProjectConfig, tags dockerImageTags, crossArch string, resolvedVersion versionInfo, guard projectImageGuard) (buildResult, bool) {
	mode := guard.imageBuilder
	start := time.Now()
	fail := func(err error) (buildResult, bool) {
		return buildResult{name: cfg.Name + " (docker)", kind: "docker", duration: time.Since(start), err: err}, true
	}
	src, err := os.ReadFile("Dockerfile")
	if err != nil {
		return buildResult{}, false
	}
	platform := imagePlatform(crossArch)
	artifacts := projectImageArtifacts(src, guard.outputDir, guard.binaries)
	buildArgs := map[string]string{
		"FORGE_VERSION": resolvedVersion.version,
		"FORGE_COMMIT":  resolvedVersion.commit,
		"FORGE_DATE":    resolvedVersion.date,
	}
	target := "production"
	plan := chooseImageBuilder(ctx, mode, "Dockerfile", func() error {
		return imagebuild.Check(src, target, buildArgs, artifacts)
	})
	if plan.err != nil {
		return fail(plan.err)
	}
	if !plan.use {
		return buildResult{}, false
	}

	if err := assertProjectImageBinaries(guard.outputDir, crossArch, guard.envLabel, guard.binaryNames); err != nil {
		return fail(err)
	}
	if err := checkBuildStorageFn(projectDirForKCL()); err != nil {
		return fail(err)
	}
	// An in-process image exists only in a registry: there is no local daemon
	// to load it into. Building one with nowhere to push would record a build
	// state that names an image nothing holds.
	if len(tags.push) == 0 {
		return fail(cliutil.UserErr("forge build",
			fmt.Sprintf("the project image would be assembled without Docker (%s), but nothing is being pushed", plan.why),
			"",
			"an image built without a Docker daemon lives only in a registry. Push it: forge env build <env> --push (or forge env deploy <env>), or run where a Docker daemon is reachable"))
	}
	fmt.Printf("[build] %s: assembling image in-process for %s (%s; %d tags)\n",
		cfg.Name, platformLabel(platform), plan.why, len(tags.push))

	res, err := imagebuild.Build(ctx, imagebuild.Options{
		Dockerfile: src, Target: target, BuildArgs: buildArgs,
		Platform: platform, ContextDir: ".", Artifacts: artifacts,
		Created: imageBuildEpoch(ctx, ".", "Dockerfile"),
	})
	if err != nil {
		return fail(err)
	}
	defer res.Cleanup()
	for _, n := range res.Notes {
		fmt.Printf("[build]   %s\n", n)
	}
	if res.Base != "" {
		fmt.Printf("[build] %s: base %s\n", cfg.Name, res.Base)
	}

	refs := dedupeRefs(tags.push)
	for _, t := range refs {
		fmt.Printf("[build] %s: push %s\n", cfg.Name, t)
	}
	if err := pushInProcessImage(ctx, res.Image, refs); err != nil {
		return fail(err)
	}
	digest, platforms := res.Digest.String(), []string{platformLabel(platform)}
	fmt.Printf("[build] %s: pushed digest %s\n", cfg.Name, digest)
	return buildResult{
		name:       cfg.Name + " (docker)",
		kind:       "docker",
		duration:   time.Since(start),
		image:      cfg.Name,
		repository: tags.repository,
		tag:        tags.tag,
		digest:     digest,
		platforms:  platforms,
	}, true
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

func platformLabel(p v1.Platform) string { return p.OS + "/" + p.Architecture }

// pushInProcessImage pushes img to refs with the docker credential store, and
// appends the realm-mismatch hint when the registry refuses the push, exactly
// as dockerPush does.
func pushInProcessImage(ctx context.Context, img v1.Image, refs []string) error {
	if len(refs) == 0 {
		return nil
	}
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
