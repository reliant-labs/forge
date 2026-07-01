package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cliutil"
	"github.com/reliant-labs/forge/internal/kclrender"
)

// newNewEnvCmd builds `forge new-env <name>` — scaffold a new deploy
// environment by deriving it from one of the project's EXISTING envs.
//
// WHY this command exists (the root-cause fix): authoring a new env is a
// multi-file, hundreds-of-lines manual transcribe of a sibling env's
// deploy/kcl/<env>/ directory. The boilerplate (the full_stack/ClusterTarget
// wiring, the sibling-repo build_cmds, the frontends, the in-cluster infra) is
// pure copy-paste — but buried inside it are ~7 DANGEROUS per-env knobs
// (cluster context, namespace, registry, platform, image_tag, supabase/issuer,
// secret-ref overrides). Copy-pasting a sibling SILENTLY inherits that
// sibling's values for every one of those knobs: forget to change `registry`
// and you push/deploy to the wrong registry; forget `platform` and an arm64
// dev Mac builds images that CrashLoopBackOff "exec format error" on amd64
// cloud nodes; forget `cluster`/`namespace` and you deploy into a SIBLING
// environment. Every one of those is a DEPLOY-TIME (or worse, silent runtime)
// failure today.
//
// new-env converts that into an AUTHOR-TIME error: it copies the sibling's
// boilerplate verbatim but REPLACES each dangerous knob's value with a
// `REPLACE_ME_*` placeholder carrying an inline `# check:` comment explaining
// what to put there and how to verify it. A forgotten knob is now a visible
// placeholder, not an inherited-wrong value. The `--check` flag (and the CI
// guard a project can wire around it) FAILS while any placeholder remains, so
// an un-filled env can't ship.
func newNewEnvCmd() *cobra.Command {
	var (
		fromEnv string
		check   bool
		force   bool
	)

	cmd := &cobra.Command{
		Use:   "new-env <name>",
		Short: "Scaffold a new deploy environment from an existing one",
		Long: `Scaffold deploy/kcl/<name>/ for a new environment by deriving it
from one of the project's existing envs (instead of hand-copying a sibling).

The boilerplate — the full_stack / ClusterTarget wiring, sibling-repo
build commands, frontends, in-cluster infra — is copied verbatim from the
template env. The DANGEROUS per-env knobs (cluster context, namespace,
registry, platform, image_tag, supabase URL / JWT issuer) are replaced
with REPLACE_ME_* placeholders carrying inline 'check:' guidance, so a
knob you forget to set is a visible author-time error rather than a value
silently inherited from the wrong environment.

The template env is auto-selected (a cloud-shaped sibling is preferred)
or chosen explicitly with --from. After filling the placeholders, run
'forge new-env <name> --check' (or just re-run with --check) to confirm
no placeholder remains and the env KCL-compiles.

Examples:
  forge new-env preview                 # derive from an auto-picked cloud sibling
  forge new-env preview --from staging  # derive explicitly from staging
  forge new-env preview --check         # verify no REPLACE_ME_* remains + it compiles`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runNewEnv(cmd.Context(), args[0], fromEnv, check, force)
		},
	}

	cmd.Flags().StringVar(&fromEnv, "from", "", "Existing env to derive the new env from (default: auto-pick a cloud-shaped sibling)")
	cmd.Flags().BoolVar(&check, "check", false, "Don't scaffold; verify the existing env has no REPLACE_ME_* placeholders left and KCL-compiles (CI gate)")
	cmd.Flags().BoolVar(&force, "force", false, "Overwrite deploy/kcl/<name>/ if it already exists")

	return cmd
}

// placeholderToken is the sentinel prefix new-env stamps in place of every
// dangerous knob value. It is intentionally NOT a valid cluster/registry/etc.
// value so a forgotten one is loud — and it is the exact string the --check
// gate (and any CI guard wired around it) greps for.
const placeholderToken = "REPLACE_ME"

// placeholderRe matches a stamped placeholder anywhere in a line.
var placeholderRe = regexp.MustCompile(`REPLACE_ME[A-Z_]*`)

func runNewEnv(ctx context.Context, name, fromEnv string, check, force bool) error {
	projectDir, err := projectRoot()
	if err != nil {
		return err
	}

	if err := validateEnvName(name); err != nil {
		return err
	}

	kclDir := filepath.Join(projectDir, "deploy", "kcl")
	envDir := filepath.Join(kclDir, name)

	// --check mode operates on the ALREADY-scaffolded env: it's the CI gate
	// that fails while any placeholder remains (and confirms the env still
	// compiles). It never writes anything.
	if check {
		return checkEnv(ctx, projectDir, name, envDir)
	}

	if _, err := os.Stat(envDir); err == nil && !force {
		return cliutil.UserErr("forge new-env",
			fmt.Sprintf("deploy/kcl/%s already exists", name),
			"",
			fmt.Sprintf("pick a different env name, pass --force to overwrite, or run 'forge new-env %s --check' to verify the existing one", name))
	}

	// Resolve the template env. Auto-pick prefers a cloud-shaped sibling
	// (one whose ClusterTarget carries the dangerous knobs worth templating);
	// the new env is almost always "another cloud env like staging", not a
	// host-mode dev fixture.
	envs, err := ListEnvs(projectDir)
	if err != nil {
		return cliutil.WrapUserErr("forge new-env", "list existing envs", "",
			"confirm deploy/kcl/<env>/main.k files exist", err)
	}
	if len(envs) == 0 {
		return cliutil.UserErr("forge new-env",
			"no existing environments to derive from",
			"",
			"author the first env's deploy/kcl/<env>/main.k by hand (or 'forge new' scaffolds a starter env); new-env templates from an existing one")
	}

	template, err := resolveTemplateEnv(projectDir, envs, fromEnv, name)
	if err != nil {
		return err
	}

	srcDir := filepath.Join(kclDir, template)
	written, err := scaffoldEnvFromTemplate(projectDir, srcDir, envDir, template, name)
	if err != nil {
		return err
	}

	fmt.Printf("Scaffolded environment %q from %q:\n", name, template)
	for _, f := range written {
		rel, _ := filepath.Rel(projectDir, f)
		fmt.Printf("  %s\n", rel)
	}

	// Best-effort: surface the placeholders the author must now fill, so the
	// next action is obvious without opening the file.
	if n := countPlaceholders(envDir); n > 0 {
		fmt.Printf("\n%d REPLACE_ME_* placeholder(s) need values before this env can deploy.\n", n)
		fmt.Println("Each carries an inline '# check:' comment explaining what to set.")
	}

	n := Name()
	fmt.Println("\nNext steps:")
	fmt.Printf("  1. Edit deploy/kcl/%s/main.k and replace every REPLACE_ME_* with the real value (see the inline 'check:' guidance).\n", name)
	fmt.Printf("  2. Add the sibling config.%s.yaml app-config (copy config.%s.yaml) and run '%s generate' to emit deploy/kcl/%s/config_gen.k.\n", name, template, n, name)
	fmt.Printf("  3. Run '%s new-env %s --check' to confirm no placeholder remains and the env KCL-compiles.\n", n, name)

	return nil
}

// validateEnvName rejects env names that wouldn't be a safe directory /
// KCL identifier segment. Mirrors the spirit of validateProjectName but
// scoped to the deploy/kcl/<env>/ directory contract.
func validateEnvName(name string) error {
	if name == "" {
		return cliutil.UserErr("forge new-env", "env name is required", "",
			"pass an env name, e.g. 'forge new-env preview'")
	}
	// Lowercase letters/digits/hyphen, must start with a letter. This is the
	// shape of every existing env dir (dev, dev-k8s, e2e, staging, preprod,
	// prod) and keeps the derived KCL local-var name (_<name>_k8s) valid once
	// hyphens are mapped to underscores.
	ok := regexp.MustCompile(`^[a-z][a-z0-9-]*$`).MatchString(name)
	if !ok {
		return cliutil.UserErr("forge new-env",
			fmt.Sprintf("invalid env name %q", name),
			"",
			"use lowercase letters, digits, and hyphens, starting with a letter (e.g. staging, preview, eu-west)")
	}
	return nil
}

// resolveTemplateEnv picks the env to derive from. An explicit --from must
// exist; otherwise auto-pick prefers a cloud-shaped sibling (a main.k whose
// ClusterTarget carries a `platform = ...` knob — the strongest signal of a
// cloud env worth templating), falling back to the alphabetically-first env.
func resolveTemplateEnv(projectDir string, envs []string, fromEnv, newName string) (string, error) {
	if fromEnv != "" {
		for _, e := range envs {
			if e == fromEnv {
				return e, nil
			}
		}
		return "", cliutil.UserErr("forge new-env",
			fmt.Sprintf("--from env %q not found", fromEnv),
			"",
			fmt.Sprintf("pick one of the existing envs: %s", strings.Join(envs, ", ")))
	}

	// Prefer a cloud-shaped sibling. Iterate in sorted order for determinism.
	sorted := append([]string(nil), envs...)
	sort.Strings(sorted)
	for _, e := range sorted {
		if e == newName {
			continue
		}
		body, err := os.ReadFile(filepath.Join(projectDir, "deploy", "kcl", e, "main.k"))
		if err != nil {
			continue
		}
		if looksCloudShaped(string(body)) {
			return e, nil
		}
	}
	for _, e := range sorted {
		if e != newName {
			return e, nil
		}
	}
	return "", cliutil.UserErr("forge new-env",
		"no env to derive from (only the new env name itself was found)",
		"",
		"pass --from <existing-env>")
}

// looksCloudShaped reports whether a main.k carries the cloud-env signal: a
// `platform = ...` knob on its cluster target. Host-mode dev fixtures and the
// local k3d envs omit it (they target the host arch deliberately), so this
// distinguishes "a staging-like cloud sibling" from "a dev fixture".
func looksCloudShaped(body string) bool {
	return regexp.MustCompile(`(?m)^\s*platform\s*=`).MatchString(body)
}

// scaffoldEnvFromTemplate copies the template env's authored .k files into
// the new env dir, applying the per-file knob-neutralizing transform. Returns
// the list of files written. config_gen.k is DELIBERATELY skipped — it's
// forge-generated (hash-locked) from the sibling config.<env>.yaml, so we
// point the author at `forge generate` rather than copying a stale, mislabeled
// ConfigMap.
func scaffoldEnvFromTemplate(projectDir, srcDir, envDir, template, name string) ([]string, error) {
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return nil, cliutil.WrapUserErr("forge new-env",
			fmt.Sprintf("read template env %s", template), "",
			"confirm the template env directory exists", err)
	}

	if err := os.MkdirAll(envDir, 0o755); err != nil {
		return nil, fmt.Errorf("create %s: %w", envDir, err)
	}

	var written []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".k" {
			continue
		}
		// Skip the generated config projection — it's owned by `forge
		// generate`, not hand-copied (its forge:hash header + env-stamped
		// ConfigMap name would be wrong for the new env).
		if e.Name() == "config_gen.k" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(srcDir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		transformed := transformEnvFile(string(body), template, name)
		dst := filepath.Join(envDir, e.Name())
		if err := os.WriteFile(dst, []byte(transformed), 0o644); err != nil {
			return nil, fmt.Errorf("write %s: %w", dst, err)
		}
		written = append(written, dst)
	}
	if len(written) == 0 {
		return nil, cliutil.UserErr("forge new-env",
			fmt.Sprintf("template env %q has no .k files to derive from", template),
			"",
			"pick a --from env whose deploy/kcl/<env>/ contains a main.k")
	}
	sort.Strings(written)
	return written, nil
}

// transformEnvFile applies the derive transform to one .k file:
//
//  1. Rename the template env's identity (the `_<template>_k8s` local var, the
//     env-string defaults) to the new env, so the file is self-consistent.
//  2. Neutralize the dangerous per-env knobs to REPLACE_ME_* placeholders with
//     inline `# check:` guidance.
//
// The transform is line-oriented and keys off forge KCL idioms (not project
// specifics), so it derives ANY forge project's env, not just control-plane.
func transformEnvFile(body, template, name string) string {
	// Map hyphenated env names to underscore for KCL identifier segments
	// (deploy/kcl/dev-k8s/ → _devk8s_k8s convention varies; we only rewrite
	// the exact `_<template>_k8s`/`_<template>_k8s`-style binding when it is a
	// literal identifier substring, which is safe).
	tIdent := identSegment(template)
	nIdent := identSegment(name)

	lines := strings.Split(body, "\n")
	out := make([]string, 0, len(lines)+8)
	for _, line := range lines {
		out = append(out, transformLine(line, template, name, tIdent, nIdent)...)
	}
	return strings.Join(out, "\n")
}

// identSegment maps an env name to the identifier segment forge envs use in
// their local cluster-target var name (`_<seg>_k8s`): hyphens are dropped
// (dev-k8s → devk8s), matching the dev-k8s/main.k `_devk8s_k8s` convention.
func identSegment(env string) string {
	return strings.ReplaceAll(env, "-", "")
}

// transformLine returns the rewritten line(s) for one source line. Most lines
// pass through unchanged; the dangerous-knob lines are rewritten to a
// REPLACE_ME_* placeholder preceded by a `# check:` guidance comment.
//
// Returning a slice lets a single source line expand into a guidance comment
// + the placeholder line.
func transformLine(line, template, name, tIdent, nIdent string) []string {
	indent := leadingWhitespace(line)

	// 1. option("X") or "<default>" — image_tag / registry / namespace.
	if m := optionDefaultRe.FindStringSubmatch(line); m != nil {
		opt := m[2]
		switch opt {
		case "image_tag":
			// image_tag defaults to the env NAME in most forge envs; that's a
			// safe, non-dangerous default (it's just a tag string), so we set
			// it to the new env name rather than forcing a placeholder.
			return []string{fmt.Sprintf(`%s_image_tag = option("image_tag") or "%s"`, indent, name)}
		case "registry":
			return knobLines(indent, "_registry", `option("registry") or "REPLACE_ME_REGISTRY"`,
				"the image registry forge PUSHES to and the deploy PULLS from",
				"a wrong/stale value SILENTLY ImagePullBackOff's at deploy time",
				`run 'forge build --env `+name+` --push <registry>' and confirm the pushed ref matches the deploy ref`)
		case "namespace":
			return knobLines(indent, "_namespace", `option("namespace") or "REPLACE_ME_NAMESPACE"`,
				"the k8s namespace every workload in this env deploys into",
				"inheriting a sibling's namespace deploys INTO that sibling environment",
				`kubectl get ns | grep `+name+` — the namespace must be unique to this env`)
		}
	}

	// 2. cluster = "<context>" inside a ClusterTarget/K8sCluster block.
	if m := clusterAssignRe.FindStringSubmatch(line); m != nil {
		return knobLines(indent, "cluster", `"REPLACE_ME_CLUSTER_CONTEXT"`,
			"the kubectl context / cluster name this env deploys to",
			"inheriting a sibling's cluster deploys to the WRONG cluster",
			`kubectl config get-contexts — the value must name THIS env's cluster`)
	}

	// 3. platform = "<arch>" — the build arch knob.
	if m := platformAssignRe.FindStringSubmatch(line); m != nil {
		return knobLines(indent, "platform", `"REPLACE_ME_PLATFORM"`,
			"the node arch (amd64/arm64). forge derives ${TARGETARCH} + GOARCH from it",
			"a wrong value builds images that CrashLoopBackOff 'exec format error' on the nodes",
			`kubectl get nodes -o jsonpath='{.items[*].status.nodeInfo.architecture}' for THIS env's cluster`)
	}

	// 4. _supabase_url / _supabase_jwt_issuer — the identity knobs.
	if m := supabaseAssignRe.FindStringSubmatch(line); m != nil {
		varName := m[1]
		switch varName {
		case "_supabase_url":
			return knobLines(indent, "_supabase_url", `"REPLACE_ME_SUPABASE_URL"`,
				"the Supabase project URL backing this env's auth",
				"a wrong project means logins validate against the wrong identity source",
				"the Supabase dashboard project URL for THIS env")
		case "_supabase_jwt_issuer":
			return knobLines(indent, "_supabase_jwt_issuer", `"REPLACE_ME_SUPABASE_JWT_ISSUER"`,
				"the JWT issuer (iss) tokens for this env carry — the REAL project issuer, not a custom domain",
				"a wrong issuer fails issuer validation on EVERY login",
				"curl <supabase-url>/auth/v1/.well-known/openid-configuration | jq .issuer")
		}
	}

	// 5. Rename the template's cluster-target local var to the new env's, so
	//    the derived file is self-consistent (_staging_k8s → _preview_k8s).
	if tIdent != nIdent {
		oldVar := "_" + tIdent + "_k8s"
		newVar := "_" + nIdent + "_k8s"
		if strings.Contains(line, oldVar) {
			line = strings.ReplaceAll(line, oldVar, newVar)
		}
	}

	return []string{line}
}

// knobLines renders a dangerous knob as a `# check:` guidance comment block
// followed by the placeholder assignment. assignRHS is the right-hand side
// (everything after `<knob> = `). The result is a small, self-documenting
// block the author can't miss.
func knobLines(indent, knob, assignRHS, what, danger, check string) []string {
	return []string{
		fmt.Sprintf("%s# REPLACE_ME — %s.", indent, what),
		fmt.Sprintf("%s#   danger: %s", indent, danger),
		fmt.Sprintf("%s#   check:  %s", indent, check),
		fmt.Sprintf("%s%s = %s", indent, knob, assignRHS),
	}
}

var (
	// option("X") or "<default>" assigned to a top-level _var.
	optionDefaultRe = regexp.MustCompile(`^\s*(_\w+)\s*=\s*option\("(\w+)"\)\s*or\s*"[^"]*"`)
	// cluster = "<value>" (inside a ClusterTarget / K8sCluster block).
	clusterAssignRe = regexp.MustCompile(`^\s*cluster\s*=\s*"[^"]*"`)
	// platform = "<value>".
	platformAssignRe = regexp.MustCompile(`^\s*platform\s*=\s*"[^"]*"`)
	// _supabase_url / _supabase_jwt_issuer = "<value>".
	supabaseAssignRe = regexp.MustCompile(`^\s*(_supabase_url|_supabase_jwt_issuer)\s*=\s*"[^"]*"`)
)

func leadingWhitespace(line string) string {
	return line[:len(line)-len(strings.TrimLeft(line, " \t"))]
}

// checkEnv is the --check gate: it fails if any REPLACE_ME_* placeholder
// remains in the env's .k files, then confirms the env KCL-compiles. This is
// the build-time error that an un-filled (or broken) env can't pass — wire it
// into CI to keep a half-authored env from shipping.
func checkEnv(ctx context.Context, projectDir, name, envDir string) error {
	if _, err := os.Stat(envDir); err != nil {
		return cliutil.UserErr("forge new-env --check",
			fmt.Sprintf("env %q not found (deploy/kcl/%s/ missing)", name, name),
			"",
			fmt.Sprintf("scaffold it first: 'forge new-env %s'", name))
	}

	// 1. No placeholders may remain.
	offenders, err := placeholderOffenders(envDir)
	if err != nil {
		return err
	}
	if len(offenders) > 0 {
		var b strings.Builder
		fmt.Fprintf(&b, "%d REPLACE_ME_* placeholder(s) still unset in deploy/kcl/%s/:", len(offenders), name)
		for _, o := range offenders {
			fmt.Fprintf(&b, "\n    %s", o)
		}
		return cliutil.UserErr("forge new-env --check",
			b.String(),
			"",
			"replace every REPLACE_ME_* with the real value per its inline 'check:' guidance")
	}

	// 2. The env must KCL-compile. This is the same render seam forge
	//    deploy/up/build use, so a green check here means the env is
	//    deployable (modulo runtime), not just placeholder-free.
	if _, err := kclrender.Run(projectDir, envDir, []string{"env=" + name}); err != nil {
		return cliutil.WrapUserErr("forge new-env --check",
			fmt.Sprintf("env %q has no REPLACE_ME_* left but does not KCL-compile", name),
			"",
			"fix the KCL error above (a placeholder may have been replaced with a malformed value); then re-run --check",
			err)
	}

	fmt.Printf("✓ env %q: no placeholders remaining and KCL compiles.\n", name)
	return nil
}

// placeholderOffenders returns "file:line: <text>" for each line in the env's
// .k files that still carries a REPLACE_ME_* token. The guidance-comment lines
// new-env emits ALSO contain the bare word REPLACE_ME, so we only flag lines
// where the token appears in a non-comment position (i.e. a real assignment),
// keeping the gate precise.
func placeholderOffenders(envDir string) ([]string, error) {
	entries, err := os.ReadDir(envDir)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", envDir, err)
	}
	var offenders []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".k" {
			continue
		}
		body, err := os.ReadFile(filepath.Join(envDir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		for i, line := range strings.Split(string(body), "\n") {
			if lineHasLivePlaceholder(line) {
				offenders = append(offenders, fmt.Sprintf("%s:%d: %s", e.Name(), i+1, strings.TrimSpace(line)))
			}
		}
	}
	return offenders, nil
}

// lineHasLivePlaceholder reports whether a line carries a REPLACE_ME_* token in
// an ACTIVE (non-comment) position. The leading `#` guidance comments new-env
// stamps mention REPLACE_ME too; those are not offenders. A line is an
// offender when a placeholder token appears BEFORE any `#`.
func lineHasLivePlaceholder(line string) bool {
	code := line
	if h := strings.Index(line, "#"); h >= 0 {
		code = line[:h]
	}
	return placeholderRe.MatchString(code)
}

// countPlaceholders counts live (non-comment) placeholders across the env's
// .k files — the post-scaffold "N knobs to fill" hint.
func countPlaceholders(envDir string) int {
	offenders, err := placeholderOffenders(envDir)
	if err != nil {
		return 0
	}
	return len(offenders)
}
