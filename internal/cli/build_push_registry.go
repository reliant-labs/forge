package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/reliant-labs/forge/internal/cliutil"
	"github.com/reliant-labs/forge/internal/config"
)

// pushDeclaredSentinel is --push's NoOptDefVal: the value pflag records for a
// BARE `--push`, meaning "push to the registry the env declares". No registry
// reference can contain parentheses, so it never collides with a real value.
// RunE translates it into buildOptions.pushDeclared; it never reaches a build.
const pushDeclaredSentinel = "(declared)"

// pushRegistryChoice is where a build pushes, why, and anything the user
// should know about that choice.
type pushRegistryChoice struct {
	// registry is the push destination; empty means the build pushes nothing.
	registry string
	// source says which precedence step chose registry, for the build header.
	source string
	// warning, when set, is printed before the build: an explicit --push that
	// disagrees with the registry the env declares.
	warning string
}

// renderBuildInputs renders the env (renderBuildEntities) and resolves where
// the build pushes from the FULL render — the registry is an env-wide fact,
// and --target narrowing can drop the one service that states it. The
// resolved registry is written back into opts.pushRegistry so every
// downstream push reads the one resolved value. Returns the narrowed entity
// set the build acts on.
func renderBuildInputs(ctx context.Context, cfg *config.ProjectConfig, opts *buildOptions) (*KCLEntities, pushRegistryChoice, error) {
	declared, entities, err := renderBuildEntities(ctx, cfg, *opts)
	if err != nil {
		return nil, pushRegistryChoice{}, err
	}
	push, err := resolvePushRegistry(ctx, *opts, declared)
	if err != nil {
		return nil, pushRegistryChoice{}, err
	}
	opts.pushRegistry = push.registry
	return entities, push, nil
}

// printHeader prints the build header's push line: where this build pushes
// and which precedence step chose it, plus the override warning when there
// is one. Nothing when the build pushes nothing.
func (c pushRegistryChoice) printHeader() {
	if c.registry != "" {
		fmt.Printf("[build]   Push:     %s (%s)\n", c.registry, c.source)
	}
	if c.warning != "" {
		fmt.Printf("[build]   Warning: %s\n", c.warning)
	}
}

// resolvePushRegistry is the ONE place a build decides where it pushes. The
// precedence is explicit and ordered:
//
//  1. --push <registry> — an explicit value always wins. When the env also
//     declares a registry and the two differ, the choice carries a warning:
//     `forge env deploy` pulls from the DECLARED one, so the deploy would not
//     run what this build pushed.
//  2. The env's declaration — cluster_target.registry, else the first
//     forge.K8sCluster.registry — read by k8sClusterFieldFromEntities, the
//     same resolution `forge env up` and `forge env deploy` use
//     (k8sClusterRegistryForEnv), so build and deploy agree by construction.
//  3. The platform default — platformDefaultRegistry, a seam that provides
//     nothing today (see its doc).
//  4. Otherwise a bare --push fails with a runbook naming the file and field
//     to set.
//
// declared is the env's FULL render, before --target narrowing: the registry
// is an env-wide fact, and a narrowed set can drop the one service that
// states it. nil when there is no env or the env has no KCL directory.
func resolvePushRegistry(ctx context.Context, opts buildOptions, declared *KCLEntities) (pushRegistryChoice, error) {
	if opts.pushRegistry == "" && !opts.pushDeclared {
		return pushRegistryChoice{}, nil
	}
	envRegistry := k8sClusterFieldFromEntities(declared, "registry")

	if opts.pushRegistry != "" {
		choice := pushRegistryChoice{registry: opts.pushRegistry, source: "--push flag"}
		if envRegistry != "" && !samePushRegistry(opts.pushRegistry, envRegistry) {
			choice.warning = fmt.Sprintf("--push %s overrides the registry deploy/kcl/%s/main.k declares (%s). "+
				"`forge env deploy %s` pulls from the declared registry, so it will not deploy what this build pushes",
				opts.pushRegistry, opts.env, envRegistry, opts.env)
		}
		return choice, nil
	}

	if opts.env == "" {
		return pushRegistryChoice{}, errBarePushNeedsEnv()
	}
	if envRegistry != "" {
		return pushRegistryChoice{registry: envRegistry, source: fmt.Sprintf("declared in deploy/kcl/%s/main.k", opts.env)}, nil
	}
	registry, ok, err := platformDefaultRegistry(ctx, opts.env, declared)
	if err != nil {
		return pushRegistryChoice{}, fmt.Errorf("resolve the platform default registry for env %q: %w", opts.env, err)
	}
	if ok {
		return pushRegistryChoice{registry: registry, source: "platform default"}, nil
	}
	return pushRegistryChoice{}, undeclaredPushRegistryError(opts.env, declared)
}

// platformDefaultRegistry is step 3 of resolvePushRegistry: the registry the
// PLATFORM provides for an env that declares none.
//
// It is the seam the Reliant-hosted registry slots into (control-plane ADR
// 0003). For an env whose Bundle declares forge.ControlPlane, that work fills
// this from the control plane's image_push_base — `<registry_base>/<org>`,
// already returned on every environment read (deploytarget's
// wireEnvironment.ImagePushBase) and already enforced at publish time — so a
// hosted `forge build <env> --push` needs no registry at all. It stays a
// DEFAULT: an env-declared registry (step 2) and --push <registry> (step 1)
// both win, so no project is locked into it.
//
// Today it provides nothing — ("", false, nil) for every env — so an env that
// declares no registry fails with a runbook naming what to set.
func platformDefaultRegistry(ctx context.Context, env string, declared *KCLEntities) (registry string, ok bool, err error) {
	_, _, _ = ctx, env, declared
	return "", false, nil
}

// samePushRegistry reports whether an explicit --push value and the env's
// declared registry name the same destination. A k3d `localhost:<port>` push
// is also tagged `registry.localhost:<port>` (expandPushRegistries), so either
// spelling of a local registry matches the other.
func samePushRegistry(flagRegistry, envRegistry string) bool {
	want := strings.TrimSuffix(envRegistry, "/")
	for _, r := range expandPushRegistries(strings.TrimSuffix(flagRegistry, "/")) {
		if r == want {
			return true
		}
	}
	return false
}

// errBarePushNeedsEnv is a bare --push with no environment argument: there is
// no declaration to read the registry from.
func errBarePushNeedsEnv() error {
	return cliutil.UserErr("forge build --push",
		"a bare --push pushes to the registry the environment declares, and no environment argument was given",
		"",
		"name the env (forge build <env> --push), or the registry (forge build --push <registry>)")
}

// undeclaredPushRegistryError is the runbook for a bare --push against an env
// that declares no registry, shaped by WHY it declares none.
func undeclaredPushRegistryError(env string, declared *KCLEntities) error {
	context := fmt.Sprintf("forge build %s --push", env)
	mainK := fmt.Sprintf("deploy/kcl/%s/main.k", env)
	switch {
	case declared == nil:
		return cliutil.UserErr(context,
			fmt.Sprintf("env %q has no %s, so it declares no image registry for a bare --push to use", env, mainK),
			"",
			fmt.Sprintf("pass one explicitly (forge build %s --push <registry>); if %q was meant as the registry, write --push=%s", env, env, env))
	case declared.ControlPlane != nil && !isLocalControlPlaneEnv(declared):
		return cliutil.UserErr(context,
			fmt.Sprintf("env %q declares no image registry: it is hosted (its Bundle declares forge.ControlPlane), "+
				"and a hosted env's image destination is not declared in its KCL", env),
			mainK,
			fmt.Sprintf("pass it explicitly: forge build %s --push <image push base>", env))
	default:
		return cliutil.UserErr(context,
			fmt.Sprintf("env %q declares no image registry, and a bare --push pushes to the registry the env declares", env),
			mainK,
			fmt.Sprintf("set `registry` on the env's forge.ClusterTarget (Bundle.cluster_target) — the registry `forge env deploy %s` pulls from — "+
				"or pass one explicitly: forge build %s --push <registry>", env, env))
	}
}

// splitBuildArgs separates `forge build`'s positionals into the environment
// and, after a BARE --push, a registry written with a space.
//
// A bare --push (NoOptDefVal) never consumes the next word, so
// `forge build --push ghcr.io/acme` arrives with "ghcr.io/acme" as a
// positional. That spelling is in every existing script and must keep
// working, so a positional that cannot be an env name — it contains '.', ':'
// or '/', or is "localhost"; env names are [a-z][a-z0-9-]* — is taken as the
// registry. Two env-shaped positionals after a bare --push are refused rather
// than guessed: `forge build prod --push acme` reads either way round.
func splitBuildArgs(args []string, barePush bool) (env, pushRegistry string, err error) {
	if !barePush {
		if len(args) > 1 {
			return "", "", fmt.Errorf("accepts at most 1 arg(s), received %d", len(args))
		}
		if len(args) == 1 {
			env = args[0]
		}
		return env, "", nil
	}
	var envs, registries []string
	for _, a := range args {
		if looksLikeRegistry(a) {
			registries = append(registries, a)
		} else {
			envs = append(envs, a)
		}
	}
	switch {
	case len(registries) > 1:
		return "", "", fmt.Errorf("--push takes one registry, got %s", strings.Join(registries, " and "))
	case len(envs) > 1:
		return "", "", cliutil.UserErr("forge build --push",
			fmt.Sprintf("%q and %q could each be the environment or the registry: a registry with no '.', ':' or '/' reads like an env name", envs[0], envs[1]),
			"",
			"write the registry with '=' (forge build <env> --push=<registry>), or in full (e.g. docker.io/<namespace>)")
	}
	if len(envs) == 1 {
		env = envs[0]
	}
	if len(registries) == 1 {
		pushRegistry = registries[0]
	}
	return env, pushRegistry, nil
}

// looksLikeRegistry reports whether a positional can only be a registry: env
// names never contain '.', ':' or '/', and no env is called "localhost".
func looksLikeRegistry(s string) bool {
	return s == "localhost" || strings.ContainsAny(s, ".:/")
}
