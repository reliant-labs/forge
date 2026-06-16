package deploytarget

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ComposeProvider deploys each service in a group via docker-compose
// on the local host (or a remote docker context — that's a CLI-side
// concern, not the provider's). Unlike ExternalProvider there's no
// shell-override option: the docker-compose CLI is the contract, and
// users who want to escape it should reach for External.
//
// Deploy is a vanilla `docker compose pull` + `docker compose up -d`
// against the compose file declared in KCL. Compose handles the
// container swap itself.
//
// Rollback for docker-compose is a sharper edge: compose itself has
// no native "go back to the previous revision" affordance. Our
// strategy:
//
//  1. Track the last-good image+tag in a per-(env, service) state
//     file (same shape External uses).
//  2. On rollback, generate a temporary override file alongside the
//     state file that pins `image: <name>:<old-tag>`, then run
//     `docker compose -f <main> -f <override> up -d --force-recreate
//     <svc>`. This avoids mutating the user's compose file or
//     requiring them to keep multiple tagged copies around.
//  3. When no state file exists, error loudly — there's nothing to
//     roll back to and guessing would risk shipping a regression.
//
// The override-file approach assumes the docker daemon already has
// the old image locally (it was pulled by the previous deploy). If
// it doesn't — e.g. the registry GC'd the tag, or the user wiped
// /var/lib/docker — the up command surfaces the pull failure and
// rollback reports it.
type ComposeProvider struct {
	// ProjectDir is the project root used for state-file paths. Empty
	// means "current working directory".
	ProjectDir string

	// Runner is the os/exec indirection used to invoke docker compose.
	// Nil falls back to the package default.
	Runner commandRunner
}

// Name returns the provider identifier.
func (ComposeProvider) Name() string { return "compose" }

// Deploy ships every service in the group via docker compose.
func (p ComposeProvider) Deploy(ctx context.Context, group ServiceGroup) error {
	runner := p.runner()
	for _, svc := range group.Services {
		if svc.Compose == nil {
			return fmt.Errorf("compose %s: Compose spec is nil (group misrouted?)", svc.Name)
		}
		if err := p.deployOne(ctx, runner, group, svc); err != nil {
			return err
		}
	}
	return nil
}

// Rollback restarts every service against its previously recorded
// good tag via a generated override file. Best-effort: per-service
// failures are joined into the returned error rather than aborting
// the loop.
func (p ComposeProvider) Rollback(ctx context.Context, group ServiceGroup, lastGoodTag string) error {
	runner := p.runner()
	var failures []string
	for _, svc := range group.Services {
		if svc.Compose == nil {
			failures = append(failures, fmt.Sprintf("%s: Compose spec is nil", svc.Name))
			continue
		}
		if err := p.rollbackOne(ctx, runner, group, svc, lastGoodTag); err != nil {
			fmt.Printf("  rollback %s: %v\n", svc.Name, err)
			failures = append(failures, fmt.Sprintf("%s: %v", svc.Name, err))
			continue
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("compose rollback: %d failure(s): %s",
			len(failures), strings.Join(failures, "; "))
	}
	return nil
}

func (p ComposeProvider) runner() commandRunner {
	if p.Runner != nil {
		return p.Runner
	}
	return defaultRunner
}

func (p ComposeProvider) projectDir() string {
	if p.ProjectDir != "" {
		return p.ProjectDir
	}
	return "."
}

// composeServiceName returns the compose-side service name. KCL lets
// the user override via Compose.service; default is the forge service
// name (matches how a K8sCluster deploy names its Deployment).
func composeServiceName(spec *ComposeSpec, svcName string) string {
	if spec.Service != "" {
		return spec.Service
	}
	return svcName
}

// composeFile returns the file path the user declared. Empty falls
// back to the docker-compose default — keeps the provider working
// when KCL leaves the field unset (which is the common case).
func composeFile(spec *ComposeSpec) string {
	if spec.ComposeFile != "" {
		return spec.ComposeFile
	}
	return "docker-compose.yml"
}

// deployOne ships a single compose service via pull + up -d.
func (p ComposeProvider) deployOne(ctx context.Context, runner commandRunner, group ServiceGroup, svc ResolvedService) error {
	spec := svc.Compose
	file := composeFile(spec)
	target := composeServiceName(spec, svc.Name)
	fmt.Printf("  deploying %s via compose (file %s, service %s)...\n", svc.Name, file, target)

	// 1. Pull — `docker compose pull` is a no-op when the local image
	//    is current, so it's safe to always run. Surfacing pull
	//    failures here (rather than at up time) gives a clearer error.
	pullArgs := []string{"compose", "-f", file, "pull", target}

	// 2. Up -d — compose decides whether to recreate based on its own
	//    diff against the running container. We don't force --force-
	//    recreate here because the typical case (image tag changed)
	//    triggers a recreate naturally and forcing it on no-op deploys
	//    is unnecessary container churn.
	upArgs := []string{"compose", "-f", file}
	if spec.EnvFile != "" {
		upArgs = append(upArgs, "--env-file", spec.EnvFile)
	}
	upArgs = append(upArgs, "up", "-d", target)

	if group.DryRun {
		fmt.Printf("  [DRY-RUN] would run: docker %s\n", strings.Join(pullArgs, " "))
		fmt.Printf("  [DRY-RUN] would run: docker %s\n", strings.Join(upArgs, " "))
		return nil
	}

	// Load env_file (if declared) and merge into the docker-compose
	// process env so the compose file's `${VAR}` references resolve
	// even when the user forgets to pre-export. Passing --env-file
	// only forwards values to *containers*; the compose file itself
	// reads from the docker-compose process env. Layering both keeps
	// the two cases in sync.
	envOverlay, ferr := loadExternalEnvFile(spec.EnvFile)
	if ferr != nil {
		return fmt.Errorf("compose %s: env_file: %w", svc.Name, ferr)
	}

	// Merge resolved secrets (from a dotenv secret_provider) as the BASE
	// layer, then let env_file entries override on conflict — the explicit
	// file wins. No-op when svc.Secrets is nil/empty (the common case for
	// external/none providers), preserving the pre-secrets behaviour.
	if len(svc.Secrets) > 0 {
		merged := make(map[string]string, len(svc.Secrets)+len(envOverlay))
		for k, v := range svc.Secrets {
			merged[k] = v
		}
		for k, v := range envOverlay {
			merged[k] = v // env_file wins
		}
		envOverlay = merged
	}

	if err := runner.RunWithEnv(ctx, envOverlay, "docker", pullArgs...); err != nil {
		return fmt.Errorf("compose %s: pull: %w", svc.Name, err)
	}
	if err := runner.RunWithEnv(ctx, envOverlay, "docker", upArgs...); err != nil {
		return fmt.Errorf("compose %s: up: %w", svc.Name, err)
	}

	// 3. Health check — `compose ps --status running` returns a non-
	//    empty line for running services and empty for failed ones.
	psArgs := []string{"compose", "-f", file, "ps", "--status", "running", target}
	out, err := runner.Output(ctx, "docker", psArgs...)
	if err != nil {
		return fmt.Errorf("compose %s: health check: %w", svc.Name, err)
	}
	// The header line `NAME ... STATUS ... PORTS` shows up even when
	// no services match in some docker-compose versions; we count
	// lines past the header.
	if !composeHasRunningLine(out, target) {
		return fmt.Errorf("compose %s: service not in running state", svc.Name)
	}

	// 4. Persist the (image, tag) tuple for rollback. We pull the
	//    image+tag from the group; if the dispatcher didn't set
	//    ImageTag we still record the service+env tuple so a future
	//    rollback at least sees that a deploy happened.
	st := DeployState{Tag: group.ImageTag}
	if _, err := WriteDeployState(p.projectDir(), "compose", group.Env, svc.Name, st); err != nil {
		return fmt.Errorf("compose %s: record state: %w", svc.Name, err)
	}
	return nil
}

// composeHasRunningLine returns true when the `compose ps` output
// contains a non-header line referencing the given service. Docker
// versions differ in whether they print a header — this normalizes
// both shapes.
func composeHasRunningLine(out []byte, service string) bool {
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Skip header rows; the docker compose ps header always starts
		// with `NAME` (or `SERVICE` in older versions). Body rows
		// reference the container name, which embeds the service.
		if strings.HasPrefix(line, "NAME") || strings.HasPrefix(line, "SERVICE") {
			continue
		}
		if strings.Contains(line, service) {
			return true
		}
	}
	return false
}

// rollbackOne writes a temp override file that pins the service's
// image to the previous good tag, then runs `compose up -d --force-
// recreate` with both files. The override is deleted after the
// command returns so a subsequent normal deploy isn't shadowed by
// stale state on disk.
func (p ComposeProvider) rollbackOne(ctx context.Context, runner commandRunner, group ServiceGroup, svc ResolvedService, lastGoodTag string) error {
	spec := svc.Compose
	prev, err := ReadDeployState(p.projectDir(), "compose", group.Env, svc.Name)
	if err != nil {
		return err
	}
	target := lastGoodTag
	imageHint := ""
	if prev != nil {
		if prev.Tag != "" {
			target = prev.Tag
		}
		imageHint = prev.Image
	}
	if target == "" {
		return errors.New("no previous tag recorded; cannot rollback")
	}
	if imageHint == "" {
		// Without an image name we can't write the override — compose
		// pins by image, not by tag alone. Tell the user what they
		// need to do rather than failing silently.
		return fmt.Errorf("no previous image recorded for tag %s; manual `docker compose -f %s up -d` against the older image required",
			target, composeFile(spec))
	}

	file := composeFile(spec)
	composeSvc := composeServiceName(spec, svc.Name)

	if group.DryRun {
		// Don't write the override file on dry-run — the goal is "no
		// side effects on disk." Show the user the shape of the
		// override fragment we would have written so the dry-run is
		// informative.
		fmt.Printf("  [DRY-RUN] would write override pinning %s to %s:%s\n", composeSvc, imageHint, target)
		fmt.Printf("  [DRY-RUN] would run: docker compose -f %s -f <override> up -d --force-recreate %s\n",
			file, composeSvc)
		return nil
	}

	overridePath, err := writeComposeOverride(p.projectDir(), group.Env, svc.Name, composeSvc, imageHint, target)
	if err != nil {
		return fmt.Errorf("write override: %w", err)
	}
	defer os.Remove(overridePath)

	upArgs := []string{"compose", "-f", file, "-f", overridePath}
	if spec.EnvFile != "" {
		upArgs = append(upArgs, "--env-file", spec.EnvFile)
	}
	upArgs = append(upArgs, "up", "-d", "--force-recreate", composeSvc)
	envOverlay, ferr := loadExternalEnvFile(spec.EnvFile)
	if ferr != nil {
		return fmt.Errorf("env_file: %w", ferr)
	}
	if err := runner.RunWithEnv(ctx, envOverlay, "docker", upArgs...); err != nil {
		return fmt.Errorf("up --force-recreate: %w", err)
	}
	fmt.Printf("  rollback %s: ok (tag %s)\n", svc.Name, target)
	return nil
}

// writeComposeOverride writes a minimal `services.<name>.image:` YAML
// fragment to a temp file under .forge/state/. The fragment is
// hand-rolled rather than gen'd via a YAML library — the shape is
// fixed, well-understood, and the output is short enough that string
// formatting is more legible than yaml.Marshal.
func writeComposeOverride(projectDir, env, svc, composeService, image, tag string) (string, error) {
	dir := filepath.Join(projectDir, stateDirRel)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	name := fmt.Sprintf("compose-%s-%s-rollback.override.yml", safeStateSegment(env), safeStateSegment(svc))
	path := filepath.Join(dir, name)
	body := fmt.Sprintf("services:\n  %s:\n    image: %s:%s\n", composeService, image, tag)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		return "", err
	}
	return path, nil
}
