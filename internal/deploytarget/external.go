package deploytarget

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// ExternalProvider deploys each service in a group by exec'ing a
// user-supplied shell command via `sh -c`. It's the generic
// escape-hatch deploy target — Fly.io (`flyctl deploy`), Cloudflare
// Workers (`wrangler deploy`), GCP Cloud Run (`gcloud run deploy`),
// AWS ECS (`aws ecs update-service`), Vercel, Railway, systemd-on-VM,
// NixOS, etc. all flow through this one provider.
//
// The provider's responsibilities are deliberately narrow:
//
//  1. Substitute the documented ${X} tokens into deploy_cmd /
//     rollback_cmd / health_cmd against the merged env map
//     (built-ins + user-declared `env`).
//  2. Run deploy_cmd via `sh -c`. On success, optionally run
//     health_cmd. On both success, persist the (image, tag) tuple to
//     .forge/state/external-<env>-<service>.json so a future rollback
//     has a previous good tag to target.
//  3. Rollback reads the state file, substitutes ${LAST_TAG}, and
//     runs rollback_cmd. When no state file exists or rollback_cmd
//     is unset, return a clear error rather than guess.
//
// The provider does NOT understand the user's CLI. It doesn't know
// whether `flyctl deploy` succeeded beyond the process exit code,
// doesn't parse JSON, doesn't retry. That's all left to the
// user-supplied health_cmd. Keeping the provider narrow is the point:
// every deploy target ever invented can be modelled as "run THIS CLI
// command," so the provider has to be CLI-agnostic.
type ExternalProvider struct {
	// ProjectDir is the project root used for state-file paths and the
	// ${PROJECT_DIR} substitution. Empty means "current working
	// directory" — the forge CLI sets this explicitly; tests pass a
	// t.TempDir().
	ProjectDir string

	// Runner is the os/exec indirection used to invoke `sh -c`. Nil
	// falls back to the package default. Tests inject a fake runner.
	Runner commandRunner
}

// Name returns the provider identifier.
func (ExternalProvider) Name() string { return "external" }

// Deploy ships every service in the group by running the user-
// supplied deploy_cmd. Per-service failures abort the loop — external
// groups are typically one-service-per-group (each group's natural
// batching is "services sharing the same deploy_cmd," which is rare
// across services) and "keep going after a failure" would surprise
// the user.
func (p ExternalProvider) Deploy(ctx context.Context, group ServiceGroup) error {
	runner := p.runner()
	for _, svc := range group.Services {
		if svc.External == nil {
			return fmt.Errorf("external %s: External spec is nil (group misrouted?)", svc.Name)
		}
		if err := p.deployOne(ctx, runner, group, svc); err != nil {
			return err
		}
	}
	return nil
}

// Rollback reverts every service in the group to its previously
// recorded good tag by running the user-supplied rollback_cmd.
// Per-service failures are accumulated rather than aborting the loop —
// rollback is a recovery affordance, not a way to mask the underlying
// failure.
func (p ExternalProvider) Rollback(ctx context.Context, group ServiceGroup, lastGoodTag string) error {
	runner := p.runner()
	var failures []string
	for _, svc := range group.Services {
		if svc.External == nil {
			failures = append(failures, fmt.Sprintf("%s: External spec is nil", svc.Name))
			continue
		}
		if err := p.rollbackOne(ctx, runner, group, svc, lastGoodTag); err != nil {
			fmt.Printf("  rollback %s: %v\n", svc.Name, err)
			failures = append(failures, fmt.Sprintf("%s: %v", svc.Name, err))
			continue
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("external rollback: %d failure(s): %s",
			len(failures), strings.Join(failures, "; "))
	}
	return nil
}

func (p ExternalProvider) runner() commandRunner {
	if p.Runner != nil {
		return p.Runner
	}
	return defaultRunner
}

func (p ExternalProvider) projectDir() string {
	if p.ProjectDir != "" {
		return p.ProjectDir
	}
	return "."
}

// deployOne ships a single external service. Splits the work out so
// the outer Deploy loop stays readable.
func (p ExternalProvider) deployOne(ctx context.Context, runner commandRunner, group ServiceGroup, svc ResolvedService) error {
	spec := svc.External
	tag := resolveExternalTag(spec, group)
	fmt.Printf("  deploying %s via external command (tag %s)...\n", svc.Name, tag)

	// Build the substitution map. LAST_TAG is left empty on deploy —
	// it's only meaningful in the rollback path.
	vars := externalVars(spec, group, svc.Name, p.projectDir(), tag, "")

	// Deploy phase — required; the schema check enforces non-empty
	// deploy_cmd.
	expanded := expandVars(spec.DeployCmd, vars)

	// Dry-run: print what we would run for each phase and short-
	// circuit. No exec, no state-file write — the same "preview
	// without side effects" contract --dry-run carries on the K8s
	// cluster path.
	if group.DryRun {
		fmt.Printf("  [DRY-RUN] would exec: sh -c %s\n", expanded)
		if spec.HealthCmd != "" {
			expandedHealth := expandVars(spec.HealthCmd, vars)
			fmt.Printf("  [DRY-RUN] would exec: sh -c %s\n", expandedHealth)
		}
		return nil
	}

	// Load env_file (if declared) and merge into the exec'd process
	// env so the user-supplied CLI sees the variables without having
	// to remember to wire `--env-file ${ENV_FILE}` themselves.
	envOverlay, ferr := loadExternalEnvFile(spec.EnvFile)
	if ferr != nil {
		return fmt.Errorf("external %s: env_file: %w", svc.Name, ferr)
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

	if err := runner.RunWithEnv(ctx, envOverlay, "sh", "-c", expanded); err != nil {
		return fmt.Errorf("external %s: deploy_cmd: %w", svc.Name, err)
	}

	// Health phase — optional. A failing health_cmd short-circuits
	// before the state-file write so a deploy that "succeeded" but
	// didn't come up healthy doesn't clobber the previous good tag.
	if spec.HealthCmd != "" {
		expandedHealth := expandVars(spec.HealthCmd, vars)
		if err := runner.RunWithEnv(ctx, envOverlay, "sh", "-c", expandedHealth); err != nil {
			return fmt.Errorf("external %s: health check: %w", svc.Name, err)
		}
	}

	// State-file write — runs only after deploy+health succeed.
	if _, err := WriteDeployState(p.projectDir(), "external", group.Env, svc.Name, DeployState{
		Image: spec.Image,
		Tag:   tag,
	}); err != nil {
		return fmt.Errorf("external %s: record state: %w", svc.Name, err)
	}
	return nil
}

// rollbackOne runs the user-supplied rollback_cmd against the state-
// file's recorded tag, with ${LAST_TAG} substituted. Two failure
// modes:
//
//   - No state file (and no fallback tag): error loudly — there's
//     nothing to roll back to and guessing would risk shipping a
//     regression.
//   - No rollback_cmd set: error loudly — the user opted out of the
//     rollback path and forge can't synthesise one for an arbitrary
//     CLI.
func (p ExternalProvider) rollbackOne(ctx context.Context, runner commandRunner, group ServiceGroup, svc ResolvedService, lastGoodTag string) error {
	spec := svc.External
	prev, err := ReadDeployState(p.projectDir(), "external", group.Env, svc.Name)
	if err != nil {
		return err
	}
	target := lastGoodTag
	if prev != nil && prev.Tag != "" {
		target = prev.Tag
	}
	if target == "" {
		return errors.New("no previous tag recorded; cannot rollback")
	}
	if spec.RollbackCmd == "" {
		return errors.New("no rollback_cmd declared; cannot rollback (set External.rollback_cmd to enable)")
	}
	// Current tag is whatever the dispatcher passed in on the group.
	currentTag := resolveExternalTag(spec, group)
	vars := externalVars(spec, group, svc.Name, p.projectDir(), currentTag, target)
	expanded := expandVars(spec.RollbackCmd, vars)
	if group.DryRun {
		fmt.Printf("  [DRY-RUN] would exec: sh -c %s\n", expanded)
		return nil
	}
	envOverlay, ferr := loadExternalEnvFile(spec.EnvFile)
	if ferr != nil {
		return fmt.Errorf("env_file: %w", ferr)
	}
	if err := runner.RunWithEnv(ctx, envOverlay, "sh", "-c", expanded); err != nil {
		return fmt.Errorf("rollback_cmd: %w", err)
	}
	fmt.Printf("  rollback %s: ok (tag %s)\n", svc.Name, target)
	return nil
}

// loadExternalEnvFile parses a dotenv file into the env overlay the
// runner merges onto os.Environ() before exec. Empty path means "no
// overlay" (returns nil). Missing-file is a warning rather than a hard
// error — it matches hostlaunch's secrets_file semantics and lets
// users commit an env_file path that's optional on some dev machines.
//
// File-format errors (permission denied, malformed line) DO propagate —
// silently dropping a misconfigured file would let the deploy proceed
// with the wrong env, which is exactly the failure mode env_file is
// meant to prevent.
func loadExternalEnvFile(path string) (map[string]string, error) {
	if path == "" {
		return nil, nil
	}
	m, err := readDotEnvFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			fmt.Printf("  Warning: env_file %s not found — skipping (no env overlay applied).\n", path)
			return nil, nil
		}
		return nil, err
	}
	return m, nil
}

// resolveExternalTag picks the tag for an external deploy. Precedence
// is group-level ImageTag (set by the dispatcher from build-state or
// --image-tag) over the empty fallback. External has no per-spec tag
// override — the user can interpolate any value they want directly
// into deploy_cmd, and pinning a tag on the spec would diverge from
// the build-state.
func resolveExternalTag(_ *ExternalSpec, group ServiceGroup) string {
	if group.ImageTag != "" {
		return group.ImageTag
	}
	return ""
}

// externalVars builds the substitution map for the ${X} tokens the
// kcl/schema.k External contract advertises. The user-declared `env`
// map is merged in first so the built-ins win on conflict — that's
// the lesson from shell tooling generally (PATH, USER, HOME shouldn't
// be overridable by the user-declared env block).
func externalVars(spec *ExternalSpec, group ServiceGroup, svcName, projectDir, tag, lastTag string) map[string]string {
	vars := map[string]string{}
	// User-declared env first so the built-ins win on conflict.
	for k, v := range spec.Env {
		vars[k] = v
	}
	vars["IMAGE"] = spec.Image
	vars["TAG"] = tag
	vars["LAST_TAG"] = lastTag
	vars["SERVICE"] = svcName
	vars["ENV"] = group.Env
	vars["ENV_FILE"] = spec.EnvFile
	vars["PROJECT_DIR"] = projectDir
	return vars
}
