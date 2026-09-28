package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/reliant-labs/forge/internal/cliutil"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/deploytarget"
	"github.com/reliant-labs/forge/internal/generator"
	"github.com/reliant-labs/forge/internal/templates"
)

// runNewEnvForRuntime scaffolds deploy/kcl/<name>/ from forge's template for
// runtime — the template `forge project new` renders its own envs from —
// and then the env's config.k, exactly as `forge generate` would write it,
// so the new env renders without a generate in between.
//
// Nothing is copied from a sibling env, so there is no inherited-wrong knob
// to neutralize except the ones the cluster template names itself: its
// kubectl context, registry and platform are stamped REPLACE_ME, the same
// placeholders `--check` refuses. The host and hosted templates have none:
// a host env's facts are this machine's, and a hosted env's placement
// belongs to the control plane.
func runNewEnvForRuntime(ctx context.Context, name, runtime string, force bool) error {
	projectDir, err := projectRoot()
	if err != nil {
		return err
	}
	if err := validateEnvName(name); err != nil {
		return err
	}
	tmpl, err := templates.EnvTemplateName(runtime)
	if err != nil {
		return cliutil.UserErr("forge env new", fmt.Sprintf("unknown --runtime %q", runtime), "",
			"pass one of: "+strings.Join(templates.EnvRuntimes, ", "))
	}
	envDir := filepath.Join(projectDir, "deploy", "kcl", name)
	if _, err := os.Stat(envDir); err == nil && !force {
		return cliutil.UserErr("forge env new",
			fmt.Sprintf("deploy/kcl/%s already exists", name), "",
			fmt.Sprintf("pick a different env name, pass --force to overwrite, or run 'forge env new %s --check' to verify the existing one", name))
	}
	cfg, err := loadProjectConfig()
	if err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(projectDir, codegen.WorkloadsKCLRelPath)); err != nil {
		return cliutil.UserErr("forge env new",
			codegen.WorkloadsKCLRelPath+" is missing", "",
			"every env binds the workloads declared there; run 'forge generate' to scaffold it")
	}

	data := envTemplateDataFor(projectDir, cfg, name)
	body, err := templates.DeployTemplates().Render(tmpl, data)
	if err != nil {
		return fmt.Errorf("render %s: %w", tmpl, err)
	}
	content := string(body)
	if runtime == templates.EnvRuntimeCluster {
		content = transformEnvFile(content, name, name)
	}
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", envDir, err)
	}
	// The host and cluster templates import this env's ingress overrides;
	// the hosted one has none (the platform routes the exposed port).
	if data.IngressEnabled && runtime != templates.EnvRuntimeHosted {
		if err := ensureEnvIngress(projectDir, name); err != nil {
			return fmt.Errorf("write deploy/kcl/%s/ingress.k: %w", name, err)
		}
	}
	mainK := filepath.Join(envDir, "main.k")
	if err := os.WriteFile(mainK, []byte(content), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", mainK, err)
	}

	// The env's config.k, from the same codegen `forge generate` runs. It is
	// write-if-absent per env, so no other env's file is touched.
	cs, err := generator.LoadChecksums(projectDir)
	if err != nil {
		return fmt.Errorf("load checksums: %w", err)
	}
	if err := generatePerEnvDeployConfig(projectDir, cfg, cs); err != nil {
		return fmt.Errorf("scaffold deploy/kcl/%s/config.k: %w", name, err)
	}
	if err := generator.SaveChecksums(projectDir, cs); err != nil {
		return fmt.Errorf("save checksums: %w", err)
	}

	fmt.Printf("Scaffolded environment %q on the %s runtime:\n  deploy/kcl/%s/main.k\n  deploy/kcl/%s/config.k\n", name, runtime, name, name)
	n := Name()
	fmt.Println("\nNext steps:")
	if placeholders := countPlaceholders(envDir); placeholders > 0 {
		fmt.Printf("  1. Replace the %d REPLACE_ME_* placeholder(s) in deploy/kcl/%s/main.k (see the inline 'check:' guidance).\n", placeholders, name)
	} else {
		fmt.Printf("  1. Review deploy/kcl/%s/main.k — it binds every workload in deploy/kcl/workloads.k.\n", name)
	}
	fmt.Printf("  2. Run '%s env new %s --check' to confirm it renders", n, name)
	if runtime == templates.EnvRuntimeHosted {
		fmt.Print(" and the control plane would admit it")
	}
	fmt.Println(".")
	switch runtime {
	case templates.EnvRuntimeHosted:
		fmt.Printf("  3. '%s login', then '%s release cut <version> --env %s' and '%s env deploy %s'.\n", n, n, name, n, name)
	case templates.EnvRuntimeCluster:
		fmt.Printf("  3. '%s build %s --push', then '%s env deploy %s'.\n", n, name, n, name)
	default:
		fmt.Printf("  3. '%s env up %s'.\n", n, name)
	}
	return nil
}

// envTemplateDataFor gathers the project facts an env template needs from
// the project itself: its name, its first frontend, its first service (what
// a hosted frontend's API_URL names, and what the examples refine), and
// whether it has a database to manage.
func envTemplateDataFor(projectDir string, cfg *config.ProjectConfig, env string) templates.EnvTemplateData {
	data := templates.EnvTemplateData{
		ProjectName:     cfg.Name,
		EnvName:         env,
		PrimaryWorkload: cfg.Name,
		IngressEnabled:  fileExists(filepath.Join(projectDir, "deploy", "kcl", "ingress.k")),
		HasDatabase:     projectHasMigrations(projectDir),
	}
	for _, c := range codegen.DiscoverProjectComponents(projectDir, cfg.Name) {
		if codegen.WorkloadKindFor(c.EffectiveKind()) == codegen.WorkloadKindService {
			data.PrimaryWorkload, data.HasPrimaryService = c.Name, true
			break
		}
	}
	if len(cfg.Frontends) > 0 {
		fe := cfg.Frontends[0]
		data.HasFrontend = true
		data.FrontendName = fe.Name
		if fe.Type == "vite" || fe.Type == "vite-spa" {
			data.FrontendType = "vite"
		}
	}
	return data
}

// ensureEnvIngress writes deploy/kcl/<env>/ingress.k re-exporting the base
// topology when the env has none, so `import .ingress` resolves. The
// project's own envs carry per-env overrides; a new one starts from the
// base.
func ensureEnvIngress(projectDir, env string) error {
	path := filepath.Join(projectDir, "deploy", "kcl", env, "ingress.k")
	if fileExists(path) {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body := fmt.Sprintf(`"""
Ingress for the %s environment: the base topology in deploy/kcl/ingress.k,
unchanged. Override hosts, listeners or TLS here with KCL's `+"`|`"+` merge.
"""

import ..ingress as base

GATEWAYS = base.GATEWAYS
HTTP_ROUTES = base.HTTP_ROUTES
GRPC_ROUTES = base.GRPC_ROUTES
`, env)
	return os.WriteFile(path, []byte(body), 0o644)
}

// projectHasMigrations reports whether the project ships SQL migrations —
// the signal that it owns a database the hosted env should manage.
func projectHasMigrations(projectDir string) bool {
	entries, err := os.ReadDir(filepath.Join(projectDir, "db", "migrations"))
	if err != nil {
		return false
	}
	return slices.ContainsFunc(entries, func(e os.DirEntry) bool {
		return !e.IsDir() && strings.HasSuffix(e.Name(), ".sql")
	})
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
