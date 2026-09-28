package cli

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/reliant-labs/forge/internal/cliutil"
	"github.com/reliant-labs/forge/internal/config"
)

// An image registry is DECLARED in the env's KCL (deploy/kcl/<env>/main.k) and
// nowhere else. No flag, no `-D` binding forge owns, no environment variable
// and no forge.yaml key carries one. `forge build <env> --push` is a switch —
// "push this build" — and the destination is whatever the env declares, the
// same value `forge env deploy` pulls from, so the two agree by construction
// rather than by a CI script restating the registry correctly.

// pushRegistryChoice is where a build pushes and why.
type pushRegistryChoice struct {
	// registry is the push destination; empty means the build pushes nothing.
	registry string
	// source names the declaration it came from, for the build header.
	source string
	// env and local describe a build that pushes nothing: the env, and the
	// registry it declares (which its images are tagged under), for the
	// header.
	env, local string
}

// renderBuildInputs renders the env (renderBuildEntities) and resolves where
// the build pushes from the FULL render — the registry is an env-wide fact,
// and --target narrowing can drop the one workload that states it. The
// resolved registry is written back into opts.pushRegistry so every
// downstream push reads the one resolved value. Returns the narrowed entity
// set the build acts on.
func renderBuildInputs(ctx context.Context, cfg *config.ProjectConfig, opts *buildOptions) (*KCLEntities, pushRegistryChoice, error) {
	declared, entities, err := renderBuildEntities(ctx, cfg, *opts)
	if err != nil {
		return nil, pushRegistryChoice{}, err
	}
	push, err := resolvePushRegistry(*opts, declared)
	if err != nil {
		return nil, pushRegistryChoice{}, err
	}
	opts.pushRegistry = push.registry
	opts.envRegistry = declaredRegistry(declared)
	push.env, push.local = opts.env, opts.envRegistry
	return entities, push, nil
}

// printHeader prints where this build's images are tagged and pushed.
// Nothing when the env declares no registry and the build pushes nothing.
func (c pushRegistryChoice) printHeader() {
	switch {
	case c.registry != "":
		fmt.Printf("[build]   Push:     %s (%s)\n", c.registry, c.source)
	case c.local != "":
		fmt.Printf("[build]   Registry: %s (declared in deploy/kcl/%s/main.k; tagged locally, not pushed)\n", c.local, c.env)
	}
}

// resolvePushRegistry is the ONE place a build decides where it pushes: the
// registry the env declares (declaredRegistry), or a runbook naming the file
// and field to set. There is no other source and no precedence to reason
// about.
//
// declared is the env's FULL render, before --target narrowing. nil when there
// is no env or the env has no KCL directory.
//
// `forge env up` (opts.pushIfDeclared) pushes to the same declaration, but an
// env that declares none builds locally instead of failing: a host-only env
// has no cluster to pull from.
func resolvePushRegistry(opts buildOptions, declared *KCLEntities) (pushRegistryChoice, error) {
	if !opts.push {
		if opts.pushIfDeclared && opts.env != "" {
			if registry := declaredRegistry(declared); registry != "" {
				return pushRegistryChoice{registry: registry, source: fmt.Sprintf("declared in deploy/kcl/%s/main.k", opts.env)}, nil
			}
		}
		return pushRegistryChoice{}, nil
	}
	if opts.env == "" {
		return pushRegistryChoice{}, errPushNeedsEnv()
	}
	if registry := declaredRegistry(declared); registry != "" {
		return pushRegistryChoice{registry: registry, source: fmt.Sprintf("declared in deploy/kcl/%s/main.k", opts.env)}, nil
	}
	return pushRegistryChoice{}, undeclaredRegistryError(fmt.Sprintf("forge build %s --push", opts.env), opts.env, declared)
}

// declaredRegistry is the image registry an env's KCL declares — the single
// resolution `forge build --push`, `forge registry login` and `forge env up`
// share:
//
//  1. the env's cluster target (Bundle.cluster_target.registry, else the first
//     cluster-bound workload's) — k8sClusterFieldFromEntities, the same read
//     `forge env deploy` makes, so build and deploy agree by construction;
//  2. else the registry a hosted env declares on forge.ControlPlane.
//
// "" when the env declares none (or there is no render).
func declaredRegistry(e *KCLEntities) string {
	if r := k8sClusterFieldFromEntities(e, "registry"); r != "" {
		return r
	}
	if e != nil && e.ControlPlane != nil {
		return e.ControlPlane.Registry
	}
	return ""
}

// buildEnvArgRe is the shape of every env name (validateEnvName's rule): a
// deploy/kcl/<env>/ directory and a KCL identifier segment.
var buildEnvArgRe = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)

// validateBuildEnvArg refuses a `forge build` positional that cannot be an
// env name. Without it, a registry passed as --push's value the way older
// forges took it (ghcr.io/acme, written with a space) parses as a bool --push
// plus an ENV named "ghcr.io/acme", renders nothing for it, and would build under a nonsense
// env instead of saying the registry is declared, not passed.
func validateBuildEnvArg(arg string) error {
	if buildEnvArgRe.MatchString(arg) {
		return nil
	}
	if strings.ContainsAny(arg, ".:/") {
		return cliutil.UserErr("forge build",
			fmt.Sprintf("%q is not an environment name — it looks like an image registry, and forge build takes no registry", arg),
			"",
			"declare the registry in the env's KCL (forge.ClusterTarget.registry, or forge.ControlPlane.registry for a hosted env, "+
				"in deploy/kcl/<env>/main.k) and run forge build <env> --push")
	}
	return cliutil.UserErr("forge build",
		fmt.Sprintf("invalid environment name %q", arg),
		"",
		"an env name is lowercase letters, digits and hyphens, starting with a letter — one deploy/kcl/<env>/ directory")
}

// errPushNeedsEnv is --push with no environment argument: there is no
// declaration to read the registry from.
func errPushNeedsEnv() error {
	return cliutil.UserErr("forge build --push",
		"--push pushes to the image registry an environment's KCL declares, and no environment argument was given",
		"",
		"name the env whose KCL declares the registry: forge build <env> --push")
}

// undeclaredRegistryError is the runbook for an env that declares no image
// registry, shaped by WHY it declares none. context is the command that needed
// one (`forge build prod --push`, `forge registry login prod`).
func undeclaredRegistryError(context, env string, declared *KCLEntities) error {
	mainK := fmt.Sprintf("deploy/kcl/%s/main.k", env)
	switch {
	case declared == nil:
		return cliutil.UserErr(context,
			fmt.Sprintf("env %q has no %s, so it declares no image registry", env, mainK),
			"",
			fmt.Sprintf("create the env (forge env new %s) and declare its registry on the env's forge.ClusterTarget", env))
	case declared.ControlPlane != nil && !isLocalControlPlaneEnv(declared):
		return cliutil.UserErr(context,
			fmt.Sprintf("env %q is hosted (its Bundle declares forge.ControlPlane) and declares no image registry", env),
			mainK,
			fmt.Sprintf("set `registry` on the env's forge.ControlPlane (the registry subtree the control plane admits "+
				"this org's images from), e.g. control_plane = forge.ControlPlane { registry = \"<registry-host>/<org>\" }. "+
				"%s", hostedPushBaseFollowUp))
	default:
		return cliutil.UserErr(context,
			fmt.Sprintf("env %q declares no image registry", env),
			mainK,
			fmt.Sprintf("set `registry` on the env's forge.ClusterTarget (Bundle.cluster_target) — the registry `forge env deploy %s` pulls from", env))
	}
}

// hostedPushBaseFollowUp documents the one registry source a hosted env may
// eventually get WITHOUT declaring it: the control plane's advertised
// image_push_base (`<registry_base>/<org>`, already returned on every
// environment read — deploytarget wireEnvironment.ImagePushBase — and already
// enforced at publish time). It would be the declared-ABSENT default, never an
// override: a registry declared on forge.ControlPlane always wins.
//
// It is not consulted today. The Reliant-hosted registry gateway that would
// make that base pushable is a draft ADR (control-plane #334), not a running
// service, and forge does not invent a default for a registry that does not
// exist yet. When the gateway lands, declaredRegistry gains that one fallback
// for a hosted env with no declared registry, and this sentence goes.
const hostedPushBaseFollowUp = "forge does not yet read the control plane's advertised image push base, so a hosted env must declare its registry"
