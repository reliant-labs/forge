package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/reliant-labs/forge/internal/cliutil"
	"github.com/reliant-labs/forge/internal/deploytarget"
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

// bindingLine matches one scaffolded binding, `<binder>(wl.<ident>)`,
// capturing the binder.
func bindingLine(ident string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^(\s*)(_[a-z_]+)\(wl\.` + regexp.QuoteMeta(ident) + `\)`)
}

var (
	bundleProjectLine = regexp.MustCompile(`(?m)^(\s+)project = .*$`)
	controlPlaneField = regexp.MustCompile(`(?m)^\s+control_plane\s*=`)
)

// validateEnvBinds checks every `--bind` before anything is written, so a
// typo never leaves a half-made env directory behind.
func validateEnvBinds(binds []string) error {
	for _, b := range binds {
		workload, target, ok := strings.Cut(b, "=")
		if _, known := envBinders[target]; !ok || workload == "" || !known {
			return cliutil.UserErr("forge env new", fmt.Sprintf("--bind %q", b), "",
				"write it as <workload>=hosted or <workload>=cluster")
		}
	}
	return nil
}

// applyEnvBinds rebinds workloads in a freshly derived env's main.k: each
// `<workload>=<target>` rewrites that workload's one binding line. Binding
// any workload to hosted also declares the env's control plane (Reliant
// cloud) when it has none, since that is where a hosted workload is
// published.
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
		workload, target, ok := strings.Cut(b, "=")
		binder, known := envBinders[target]
		if !ok || workload == "" || !known {
			return cliutil.UserErr("forge env new", fmt.Sprintf("--bind %q", b), "",
				"write it as <workload>=hosted or <workload>=cluster")
		}
		re := bindingLine(naming.KCLIdentifier(workload))
		if len(re.FindAllStringIndex(content, -1)) != 1 {
			return cliutil.UserErr("forge env new",
				fmt.Sprintf("--bind %s: deploy/kcl/%s/main.k has no single `_<binder>(wl.%s)` line to rebind", b, env, naming.KCLIdentifier(workload)),
				"", "bind it by hand in the env's `_workloads` list")
		}
		content = re.ReplaceAllString(content, "${1}"+binder+"(wl."+naming.KCLIdentifier(workload)+")")
		hosted = hosted || target == "hosted"
	}
	if hosted && !controlPlaneField.MatchString(content) {
		loc := bundleProjectLine.FindStringSubmatchIndex(content)
		if loc == nil {
			return cliutil.UserErr("forge env new",
				fmt.Sprintf("deploy/kcl/%s/main.k binds a hosted workload but has no Bundle `project = ` line to add control_plane after", env),
				"", "add `control_plane = forge.ControlPlane {}` to the env's Bundle by hand")
		}
		indent := content[loc[2]:loc[3]]
		decl := "\n" + indent + "# Where the hosted-bound workloads are published: Reliant cloud (set\n" +
			indent + "# `endpoint` for another control plane).\n" +
			indent + "control_plane = forge.ControlPlane {}"
		content = content[:loc[1]] + decl + content[loc[1]:]
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	fmt.Printf("\nRebound in deploy/kcl/%s/main.k: %s\n", env, strings.Join(binds, ", "))
	return nil
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
