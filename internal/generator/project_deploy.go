package generator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/naming"
	"github.com/reliant-labs/forge/internal/templates"
)

// hostedEnvNames are the deployed envs `forge project new` scaffolds, in
// promotion order. Each is born hosted on the forge control plane
// (deploy/kcl/cloud/main.k.tmpl).
func hostedEnvNames() []string { return []string{"staging", "prod"} }

func (g *ProjectGenerator) generateKCLDeploy() error {
	deployDir := filepath.Join(g.Path, "deploy", "kcl")

	// Generate kcl.mod at deploy/kcl/ — the KCL package root for the
	// deploy manifests, so the env main.k files' package-rooted imports
	// (`import config_projection`, `import .config`, `import ..ingress`)
	// resolve. It also declares the `forge` module dependency the env
	// files import — the schemas live upstream in `forge/kcl/`, not in
	// the project's tree.
	kclModData := struct {
		ProjectName string
	}{
		ProjectName: g.Name,
	}
	kclModContent, err := templates.DeployTemplates().Render("kcl/kcl.mod.tmpl", kclModData)
	if err != nil {
		return fmt.Errorf("render kcl.mod template: %w", err)
	}
	if err := os.MkdirAll(deployDir, 0755); err != nil {
		return err
	}
	kclModPath := filepath.Join(deployDir, "kcl.mod")
	if err := os.WriteFile(kclModPath, kclModContent, 0644); err != nil {
		return fmt.Errorf("write kcl.mod: %w", err)
	}

	// kcl.mod declares no `forge` dependency: every render supplies the
	// module from the running binary (internal/kclvendor), so a scaffold is
	// resolvable the instant it is written, on any build of forge, with
	// nothing materialized into the project.

	// The legacy in-tree `deploy/kcl/schema.k` + `base.k` + `render.k`
	// files were retired in favor of the upstream `forge` KCL module.
	// Projects now `import forge` from each env's main.k.

	// The per-env main.k files. An env BINDS each workload declared once in
	// deploy/kcl/workloads.k to where it runs — one line per workload, since
	// there is no env-level runtime (ADR 0002 §2) — and states its own
	// values. dev renders from the local-loop template (host processes, the
	// local k3d cluster, host-run postgres); staging and prod from the cloud
	// template: hosted on the forge control plane (every admissible workload
	// OnHosted, a managed database, managed secrets, static hosting), with
	// the binders for a cluster you operate declared beside them, unused.
	// Each one written is recorded in g.hostedEnvs, so the CI scaffolded
	// next is the hosted pipeline for exactly those envs.
	//
	// The project's binary mode does not reach these files. Every component
	// is a subcommand of the project binary in both modes (`<bin> <name>`),
	// and a workload's `args` select it on every runtime.
	//
	// Ingress is experimental but the wiring is scaffolded at `forge project
	// new` so an opt-in needs no rescaffold; the runtime gate reads
	// IngressEnabled() at call time.
	born := g.bornComponents()
	hasFrontend := g.forScaffold().HasFrontend
	primaryWorkload := g.Name
	for _, c := range born {
		if codegen.WorkloadKindFor(c.EffectiveKind()) == codegen.WorkloadKindService {
			primaryWorkload = c.Name
			break
		}
	}
	envs := []struct{ env, template string }{{codegen.DevEnvName, "kcl/dev/main.k.tmpl"}}
	for _, env := range hostedEnvNames() {
		envs = append(envs, struct{ env, template string }{env, "kcl/cloud/main.k.tmpl"})
	}
	for _, e := range envs {
		data := templates.EnvTemplateData{
			ProjectName:     g.Name,
			EnvName:         e.env,
			PrimaryWorkload: primaryWorkload,
			PrimaryIdent:    naming.KCLIdentifier(primaryWorkload),
			IngressEnabled:  true,
			HasFrontend:     hasFrontend,
			FrontendName:    g.FrontendName,
			FrontendIdent:   naming.KCLIdentifier(g.FrontendName),
			Bindings:        scaffoldEnvBindings(e.env, born, hasFrontend),
		}
		content, err := templates.DeployTemplates().Render(e.template, data)
		if err != nil {
			return fmt.Errorf("render deploy template %s for %s: %w", e.template, e.env, err)
		}
		destPath := filepath.Join(deployDir, e.env, "main.k")
		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(destPath, content, 0644); err != nil {
			return fmt.Errorf("write %s/main.k: %w", e.env, err)
		}
		if e.env != codegen.DevEnvName {
			g.hostedEnvs = append(g.hostedEnvs, e.env)
		}
	}
	ingressOn := true
	templateData := struct{ ProjectName string }{ProjectName: g.Name}

	// Gateway API ingress scaffolding. The base topology
	// (`deploy/kcl/ingress.k`) is user-owned and shared across envs;
	// each env's `deploy/kcl/<env>/ingress.k` re-exports the base
	// with optional overrides. Both render once at `forge project new`; not
	// regenerated on subsequent `forge generate` runs.
	if ingressOn {
		ingressFiles := []struct {
			templateName string
			dest         string
		}{
			{"kcl/ingress.k.tmpl", "ingress.k"},
			{"kcl/dev/ingress.k.tmpl", "dev/ingress.k"},
			{"kcl/staging/ingress.k.tmpl", "staging/ingress.k"},
			{"kcl/prod/ingress.k.tmpl", "prod/ingress.k"},
		}
		for _, f := range ingressFiles {
			content, err := templates.DeployTemplates().Render(f.templateName, templateData)
			if err != nil {
				return fmt.Errorf("render ingress template %s: %w", f.templateName, err)
			}
			destPath := filepath.Join(deployDir, f.dest)
			if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
				return err
			}
			if err := os.WriteFile(destPath, content, 0644); err != nil {
				return fmt.Errorf("write %s: %w", f.dest, err)
			}
		}
	}

	// The component declaration — what this project is made of. SCAFFOLDED
	// ONCE and owned by the project from then on: forge writes it only when
	// it does not exist, and `forge scaffold <kind>` appends to it. Nothing
	// regenerates it, so a hand-edit survives every later forge run.
	//
	// The file is ALWAYS written, because every env's main.k imports it —
	// a missing workloads.k is an unresolvable import, not an empty list.
	//
	// Seeding it is the subtle part. This runs BEFORE the proto descriptor
	// exists, so discovery cannot yet see the services this very command is
	// scaffolding. Discovery is still asked first (it finds workers,
	// operators and secondary binaries on an upgrade path, and re-scaffolds
	// of an existing tree), and the services THIS generator knows it is
	// creating are unioned in — g.ServiceName and g.AdditionalServices are
	// that knowledge, and they are the only source for it at this point.
	//
	// Without the union, a project created with `--service billing` would be
	// born with an empty declaration; the generate pipeline that runs
	// moments later correctly refuses to rewrite a user-owned file, so
	// nothing would ever fill it in and the project could not deploy the
	// service it was created with.
	if err := ScaffoldWorkloadsKCL(g.Path, g.ModulePath, g.Name,
		g.bornComponents(), g.forScaffold().HasFrontend); err != nil {
		return fmt.Errorf("scaffold %s: %w", codegen.WorkloadsKCLRelPath, err)
	}

	return nil
}

// scaffoldEnvBindings is the body of a scaffolded env's `_workloads` list:
// one binding per workload workloads.k declares, in the same order as its
// ALL list (migrate first — it gates the rest). The dev-IdP convergence job
// is bound only in dev, whose IdP it registers against.
func scaffoldEnvBindings(env string, components codegen.Inventory, hasFrontend bool) string {
	lines := []string{codegen.EnvBinding(env, codegen.WorkloadKindJob, codegen.MigrateWorkloadName)}
	if hasFrontend && env == codegen.DevEnvName {
		lines = append(lines, codegen.EnvBinding(env, codegen.WorkloadKindJob, codegen.IDPProvisionWorkloadName))
	}
	for _, c := range components {
		lines = append(lines, codegen.EnvBinding(env, codegen.WorkloadKindFor(c.EffectiveKind()), c.Name))
	}
	return strings.Join(lines, "\n")
}

// bornComponents is the component set a freshly-scaffolded project declares:
// whatever discovery can see on disk, plus the services this generator is in
// the middle of creating (which discovery cannot see yet, because they are
// declared by a proto descriptor that does not exist until the generate
// pipeline extracts it).
func (g *ProjectGenerator) bornComponents() codegen.Inventory {
	inv := codegen.DiscoverProjectComponents(g.treeDir(), g.Name)
	for _, name := range append([]string{g.ServiceName}, g.AdditionalServices...) {
		if name == "" {
			continue
		}
		if _, exists := inv.Named(name); exists {
			continue
		}
		inv = append(inv, config.ComponentConfig{
			Name: name,
			Kind: config.ComponentKindServer,
		})
	}
	return inv
}

// ScaffoldWorkloadsKCL writes deploy/kcl/workloads.k when the project does
// not already have one, seeded with the workloads discovered so far.
//
// It is a NO-OP when the file exists. That is the whole contract: the file is
// user-owned, so the only safe automatic write is the FIRST one. Drift
// between the tree and this file afterwards is REPORTED by `forge lint`
// (which prints the exact stanza to paste) rather than silently repaired —
// forge cannot know whether a component missing from an env is an oversight
// or a deliberate choice.
//
// Callers that run before the proto descriptor exists should skip an empty
// inventory rather than write an empty file, or that empty file becomes the
// one automatic write and the real components never land (see the call in
// generateKCLDeploy).
//
// hasFrontend gates the second scaffolded one-shot, idp-provision — the
// same gate docker-compose.yml's `idp` service and the `auth.go` command
// scaffold both use. A project with no browser never gets an IdP to
// converge, so it never gets a job for converging one either.
func ScaffoldWorkloadsKCL(projectDir, modulePath, projectName string, components codegen.Inventory, hasFrontend bool) error {
	if codegen.WorkloadsKCLExists(projectDir) {
		return nil
	}
	var stanzas strings.Builder
	for i, c := range components {
		if i > 0 {
			stanzas.WriteString("\n")
		}
		stanzas.WriteString(codegen.WorkloadStanza(modulePath, projectName, c))
	}

	// The deploy-time migration step, scaffolded as an ordinary one-shot
	// workload. It goes LAST in the file but FIRST in the ALL list below:
	// k8s runs initContainers in declaration order, so a project that later
	// adds a second one-shot (seed data, provision an IdP) gets it ordered
	// after the migration, and that job sees a current schema.
	if len(components) > 0 {
		stanzas.WriteString("\n")
	}
	stanzas.WriteString(codegen.MigrateWorkloadStanza(modulePath, projectName))

	// The dev-IdP convergence step. Not broadcast-gated (see the stanza's
	// own comment for why), so its position in the file has no ordering
	// consequence — it is appended after migrate simply because migrate
	// is the project's oldest one-shot.
	if hasFrontend {
		stanzas.WriteString("\n")
		stanzas.WriteString(codegen.IDPProvisionWorkloadStanza(modulePath, projectName))
	}

	// The identifier the docstring uses in its worked `wl.<name> | {...}`
	// example. Naming a workload the project ACTUALLY has makes the example
	// copy-pasteable; with no workloads yet it falls back to a placeholder
	// the alternate (empty) branch of the template explains.
	primaryIdent := "billing"
	componentIdents := make([]string, 0, len(components))
	for _, c := range components {
		componentIdents = append(componentIdents, naming.KCLIdentifier(c.Name))
	}
	if len(componentIdents) > 0 {
		primaryIdent = componentIdents[0]
	}

	// migrate FIRST in ALL. The list order is the init-container order on
	// every gated pod, so putting the migration ahead of everything is what
	// makes a later-added one-shot run against a current schema.
	idents := []string{naming.KCLIdentifier(codegen.MigrateWorkloadName)}
	if hasFrontend {
		idents = append(idents, naming.KCLIdentifier(codegen.IDPProvisionWorkloadName))
	}
	idents = append(idents, componentIdents...)

	content, err := templates.DeployTemplates().Render("kcl/workloads.k.tmpl", struct {
		ProjectName    string
		Workloads      string
		PrimaryIdent   string
		WorkloadIdents string
	}{
		ProjectName:    projectName,
		Workloads:      strings.TrimRight(stanzas.String(), "\n"),
		PrimaryIdent:   primaryIdent,
		WorkloadIdents: strings.Join(idents, ", "),
	})
	if err != nil {
		return fmt.Errorf("render workloads.k template: %w", err)
	}
	dest := filepath.Join(projectDir, codegen.WorkloadsKCLRelPath)
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return err
	}
	return os.WriteFile(dest, content, 0644)
}

// generateDevConfig writes the k3d cluster configuration for local development.
func (g *ProjectGenerator) generateDevConfig() error {
	data := struct {
		ProjectName string
	}{
		ProjectName: g.Name,
	}

	content, err := templates.DeployTemplates().Render("k3d.yaml.tmpl", data)
	if err != nil {
		return fmt.Errorf("render k3d.yaml: %w", err)
	}

	destPath := filepath.Join(g.Path, "deploy", "k3d.yaml")
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return err
	}
	return os.WriteFile(destPath, content, 0644)
}

func (g *ProjectGenerator) generateAlloyConfig() error {
	port := g.ServicePort
	if port == 0 {
		port = 8080
	}
	data := struct {
		ProjectName string
		Services    []ServiceInfo
	}{
		ProjectName: g.Name,
		Services:    []ServiceInfo{{Name: "app", Port: port}},
	}
	content, err := templates.ProjectTemplates().Render("alloy-config.alloy.tmpl", data)
	if err != nil {
		return fmt.Errorf("render alloy-config.alloy: %w", err)
	}
	destPath := filepath.Join(g.Path, "deploy", "alloy-config.alloy")
	if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
		return err
	}
	return os.WriteFile(destPath, content, 0644)
}

func (g *ProjectGenerator) generateDockerCompose() error {
	// The shared scaffold payload rather than a local two-field struct: the
	// compose template now branches on HasFrontend (the dev IdP is only
	// scaffolded for a project that ships a browser), and forScaffold is
	// where that derivation lives for BOTH lanes. A local struct here would
	// be a third place to keep in sync with the upgrade lane's render.
	data := g.forScaffold()
	content, err := templates.ProjectTemplates().Render("docker-compose.yml.tmpl", data)
	if err != nil {
		return fmt.Errorf("render docker-compose.yml: %w", err)
	}
	destPath := filepath.Join(g.Path, "docker-compose.yml")
	if err := os.WriteFile(destPath, content, 0644); err != nil {
		return err
	}
	return g.generateIDPSteps()
}

// generateIDPSteps writes the dev IdP's declared state, which the `idp`
// compose service mounts as its setup steps. It rides alongside the
// compose file because the two are one artifact: the service is
// meaningless without the file it converges to.
func (g *ProjectGenerator) generateIDPSteps() error {
	data := struct {
		ProjectName string
	}{
		ProjectName: g.Name,
	}
	content, err := templates.ProjectTemplates().Render("idp-steps.yaml.tmpl", data)
	if err != nil {
		return fmt.Errorf("render idp-steps.yaml: %w", err)
	}
	return os.WriteFile(filepath.Join(g.Path, "idp-steps.yaml"), content, 0644)
}
