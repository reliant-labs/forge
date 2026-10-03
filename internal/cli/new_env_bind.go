package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/reliant-labs/forge/internal/cliutil"
	"github.com/reliant-labs/forge/internal/deploytarget"
	"github.com/reliant-labs/forge/internal/hostedimage"
	"github.com/reliant-labs/forge/internal/naming"
)

// envBinders maps a `--bind <workload>=<target>` target onto the binder a
// scaffolded env declares for it (deploy/kcl/cloud/main.k.tmpl). The binder
// is what applies the runtime; a derived env that has been restructured and
// no longer declares it fails the render with KCL naming the lambda, rather
// than forge guessing at a runtime literal.
var envBinders = map[string]string{
	"hosted":  "_hosted",
	"cluster": "_on_cluster",
}

// frontendBinders is the same map for a FRONTEND's binding line: a frontend
// binds a runtime exactly as a workload does (ADR 0002 §6), through its own
// binders, since its runtimes differ (no cluster; a bucket instead).
var frontendBinders = map[string]string{
	"hosted": "_hosted_frontend",
	"bucket": "_on_bucket",
}

// bindingLine matches one scaffolded workload binding, `<binder>(wl.<ident>)`,
// capturing the binder.
func bindingLine(ident string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^(\s*)(_[a-z_]+)\(wl\.` + regexp.QuoteMeta(ident) + `\)`)
}

// frontendBindingLine matches one scaffolded frontend binding,
// `<binder>(_<ident>_frontend)`.
func frontendBindingLine(ident string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^(\s*)(_[a-z_]+)\(_` + regexp.QuoteMeta(ident) + `_frontend\)`)
}

// bindTargetKnown reports whether target names a binder for a workload or a
// frontend. Which one applies is decided against the env file, by the line
// the name binds.
func bindTargetKnown(target string) bool {
	_, w := envBinders[target]
	_, f := frontendBinders[target]
	return w || f
}

// bindUsage is the one spelling of the --bind grammar every refusal names.
const bindUsage = "write it as <workload>=hosted|cluster, or <frontend>=hosted|bucket"

var (
	bundleProjectLine = regexp.MustCompile(`(?m)^(\s+)project = .*$`)
	controlPlaneField = regexp.MustCompile(`(?m)^\s+control_plane\s*=`)
)

// hostedOrgPlaceholder is the `organization` value a scaffolded hosted
// binding carries until the author replaces it. One spelling, shared with the
// lint rule that reports it (internal/hostedimage.OrgPlaceholder).
const hostedOrgPlaceholder = hostedimage.OrgPlaceholder

// validateEnvBinds checks every `--bind` before anything is written, so a
// typo never leaves a half-made env directory behind.
func validateEnvBinds(binds []string) error {
	for _, b := range binds {
		name, target, ok := strings.Cut(b, "=")
		if !ok || name == "" || !bindTargetKnown(target) {
			return cliutil.UserErr("forge env new", fmt.Sprintf("--bind %q", b), "", bindUsage)
		}
	}
	return nil
}

// applyEnvBinds rebinds workloads and frontends in a freshly derived env's
// main.k: each `<name>=<target>` rewrites that name's one binding line — a
// workload's `_<binder>(wl.<name>)` or a frontend's
// `_<binder>(_<name>_frontend)`, whichever the env declares. Binding anything
// to hosted also declares the env's control plane (Reliant cloud) when it has
// none, since that is where a hosted workload or frontend is published.
func applyEnvBinds(env string, binds []string) error {
	projectDir, err := projectRoot()
	if err != nil {
		return err
	}
	path := filepath.Join(projectDir, "deploy", "kcl", env, "main.k")
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	content := string(raw)
	hosted := false
	for _, b := range binds {
		name, target, ok := strings.Cut(b, "=")
		if !ok || name == "" || !bindTargetKnown(target) {
			return cliutil.UserErr("forge env new", fmt.Sprintf("--bind %q", b), "", bindUsage)
		}
		rebound, err := rebindOne(content, env, b, naming.KCLIdentifier(name), target)
		if err != nil {
			return err
		}
		content = rebound
		hosted = hosted || target == "hosted"
	}
	if hosted && !controlPlaneField.MatchString(content) {
		loc := bundleProjectLine.FindStringSubmatchIndex(content)
		if loc == nil {
			return cliutil.UserErr("forge env new",
				fmt.Sprintf("deploy/kcl/%s/main.k binds something hosted but has no Bundle `project = ` line to add control_plane after", env),
				"", fmt.Sprintf("add `control_plane = forge.ControlPlane {organization = %q}` to the env's Bundle by hand", hostedOrgPlaceholder))
		}
		indent := content[loc[2]:loc[3]]
		decl := "\n" + indent + "# Where the hosted-bound workloads and frontends are published: Reliant cloud (set\n" +
			indent + "# `endpoint` for another control plane).\n" +
			indent + "control_plane = forge.ControlPlane {\n" +
			indent + "    # YOUR ORGANIZATION'S ID. forge pushes this env's hosted images and its\n" +
			indent + "    # config bundle to <registry_host>/<organization>/<project>, so there is\n" +
			indent + "    # no address until this is set — `forge lint` fails while it reads\n" +
			indent + "    # " + hostedOrgPlaceholder + ", and the registry refuses a push outside your own org's\n" +
			indent + "    # subtree, so a wrong value fails at the first push rather than silently.\n" +
			indent + "    organization = \"" + hostedOrgPlaceholder + "\"\n" +
			indent + "    # registry_host defaults to Reliant's registry; set it to name another.\n" +
			indent + "}"
		content = content[:loc[1]] + decl + content[loc[1]:]
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Printf("\nRebound in deploy/kcl/%s/main.k: %s\n", env, strings.Join(binds, ", "))
	return nil
}

// rebindOne rewrites the ONE binding line that binds ident — a workload's or
// a frontend's — to target's binder. Exactly one of the two may match: a name
// is unique across an env's workloads and frontends (the Bundle refuses a
// duplicate), so a hit on both, or on neither, is a file forge will not guess
// about.
func rebindOne(content, env, bind, ident, target string) (string, error) {
	wl, fe := bindingLine(ident), frontendBindingLine(ident)
	wlHits, feHits := len(wl.FindAllStringIndex(content, -1)), len(fe.FindAllStringIndex(content, -1))
	switch {
	case wlHits == 1 && feHits == 0:
		binder, ok := envBinders[target]
		if !ok {
			return "", cliutil.UserErr("forge env new",
				fmt.Sprintf("--bind %s: %s is a workload, which binds hosted or cluster", bind, ident), "", bindUsage)
		}
		return wl.ReplaceAllString(content, "${1}"+binder+"(wl."+ident+")"), nil
	case feHits == 1 && wlHits == 0:
		binder, ok := frontendBinders[target]
		if !ok {
			return "", cliutil.UserErr("forge env new",
				fmt.Sprintf("--bind %s: %s is a frontend, which binds hosted or bucket", bind, ident), "", bindUsage)
		}
		return fe.ReplaceAllString(content, "${1}"+binder+"(_"+ident+"_frontend)"), nil
	}
	return "", cliutil.UserErr("forge env new",
		fmt.Sprintf("--bind %s: deploy/kcl/%s/main.k has no single `_<binder>(wl.%s)` or `_<binder>(_%s_frontend)` line to rebind", bind, env, ident, ident),
		"", "bind it by hand in the env's `_workloads` or `frontends` list")
}

// checkHostedAdmissible runs the hosted deploy path's own admission plan over
// what a render publishes to a control plane, offline. nil when the env
// publishes nothing.
func checkHostedAdmissible(name string, raw []byte) error {
	e, err := parseKCLEntities(raw)
	if err != nil {
		return cliutil.WrapUserErr("forge env new --check", fmt.Sprintf("decode the %s render", name), "", "", err)
	}
	if !e.HasHosted() {
		return nil
	}
	group, err := buildHostedGroup(name, e)
	if err != nil {
		return cliutil.WrapUserErr("forge env new --check",
			fmt.Sprintf("env %q cannot be published to its control plane", name), "",
			"fix the declaration the error names", err)
	}
	if group == nil {
		return nil
	}
	if _, err := deploytarget.PreflightHosted(*group); err != nil {
		return cliutil.WrapUserErr("forge env new --check",
			fmt.Sprintf("the control plane would refuse env %q", name), "",
			"each refusal names the workload and the field; drop the field, or bind that workload to a cluster you operate",
			err)
	}
	return nil
}
