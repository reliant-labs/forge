package generator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/buildinfo"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/kclvendor"
	"github.com/reliant-labs/forge/internal/naming"
	"github.com/reliant-labs/forge/internal/templates"
)

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

	// Dev builds of forge (no ldflags-stamped published forge/pkg
	// version — the same release/dev discriminator resolveForgePkgDep
	// uses for go.mod) have no resolvable published `kcl-vX.Y.Z` tag
	// either. Vendor the binary's embedded copy of the forge KCL module
	// into `.forge-kcl/` and point the freshly-rendered kcl.mod at it,
	// so the project is born rendering — the exact mechanism `forge
	// generate` maintains from here on (internal/cli sync step + the
	// shared internal/kclvendor patcher).
	if buildinfo.PkgVersion() == "" {
		if _, err := kclvendor.Materialize(g.Path); err != nil {
			return fmt.Errorf("dev-vendor forge KCL module: %w", err)
		}
		res, err := kclvendor.EnsureVendorDep(kclModPath, g.Path)
		if err != nil {
			return fmt.Errorf("point kcl.mod at %s: %w", kclvendor.VendorDirName, err)
		}
		if res.Warning != "" {
			// Freshly rendered from our own template — a warning here
			// means the template and patcher drifted apart.
			return fmt.Errorf("kcl.mod vendor patch: %s", res.Warning)
		}
	}

	// The legacy in-tree `deploy/kcl/schema.k` + `base.k` + `render.k`
	// files were retired in favor of the upstream `forge` KCL module.
	// Projects now `import forge` from each env's main.k.

	// Templated per-env files. binary=shared projects emit a parallel
	// set of templates that produce a single MultiServiceApplication
	// (one image, N Deployments) instead of N copies of Application.
	// Both shapes pin to the same schema/render lambdas; only the
	// composition at the env level differs.
	envTemplates := []struct {
		templateName string
		dest         string
	}{
		{"kcl/dev/main.k.tmpl", "dev/main.k"},
		{"kcl/staging/main.k.tmpl", "staging/main.k"},
		{"kcl/prod/main.k.tmpl", "prod/main.k"},
	}
	if g.isBinaryShared() {
		envTemplates = []struct {
			templateName string
			dest         string
		}{
			{"kcl/dev/main-shared.k.tmpl", "dev/main.k"},
			{"kcl/staging/main-shared.k.tmpl", "staging/main.k"},
			{"kcl/prod/main-shared.k.tmpl", "prod/main.k"},
		}
	}

	// The per-env main.k imports the project's own `deploy/kcl/workloads.k`
	// and lets the forge.workloads KCL schema expand each declaration.
	// The only data the env templates need is the project name, the ingress
	// toggle, and the scaffold-time migrate default.

	// Ingress is experimental but we still scaffold the wiring files at
	// `forge project new` so the user has a complete starting point. The
	// runtime gate (cert-manager install, audit category) lives on the
	// `forge cluster up` / `forge cluster urls` paths and reads
	// IngressEnabled() at call time. Setting IngressEnabled: true here
	// flips the wiring lines in main.k so an opt-in just needs the
	// experimental.ingress: true flag with no rescaffold.
	//
	// HasFrontend rides along for the same reason docker-compose.yml
	// takes it: dev/main.k names this environment's compose services
	// one by one, and the dev IdP is only among them when the project
	// ships a browser. The two files must agree — a KCL declaration of
	// a compose service that was never scaffolded would fail the deploy
	// — so both derive the answer from the same place.
	ingressOn := true

	// The workload named in each env file's worked refinement example. Using
	// a workload the project ACTUALLY has makes the commented line
	// copy-pasteable; with none yet it falls back to the project name, which
	// is what the primary service will be called.
	primaryWorkload := g.Name
	if born := g.bornComponents(); len(born) > 0 {
		primaryWorkload = born[0].Name
	}

	// No MigrateCommand here any more. The deploy-time migration step is an
	// ordinary workload now (codegen.MigrateWorkloadStanza), declared once in
	// deploy/kcl/workloads.k and inherited by every env through `wl.ALL` —
	// rather than a literal re-emitted into each env's main.k.
	templateData := struct {
		ProjectName     string
		IngressEnabled  bool
		HasFrontend     bool
		PrimaryWorkload string
	}{
		ProjectName:     g.Name,
		IngressEnabled:  ingressOn,
		HasFrontend:     g.forScaffold().HasFrontend,
		PrimaryWorkload: primaryWorkload,
	}

	for _, f := range envTemplates {
		content, err := templates.DeployTemplates().Render(f.templateName, templateData)
		if err != nil {
			return fmt.Errorf("render deploy template %s: %w", f.templateName, err)
		}
		destPath := filepath.Join(deployDir, f.dest)
		if err := os.MkdirAll(filepath.Dir(destPath), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(destPath, content, 0644); err != nil {
			return fmt.Errorf("write %s: %w", f.dest, err)
		}
	}

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
	if err := ScaffoldWorkloadsKCL(g.Path, g.Name,
		g.bornComponents(), g.forScaffold().HasFrontend); err != nil {
		return fmt.Errorf("scaffold %s: %w", codegen.WorkloadsKCLRelPath, err)
	}

	return nil
}

// bornComponents is the component set a freshly-scaffolded project declares:
// whatever discovery can see on disk, plus the services this generator is in
// the middle of creating (which discovery cannot see yet, because they are
// declared by a proto descriptor that does not exist until the generate
// pipeline extracts it).
func (g *ProjectGenerator) bornComponents() codegen.Inventory {
	inv := codegen.DiscoverProjectComponents(g.Path, g.Name)
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
func ScaffoldWorkloadsKCL(projectDir, projectName string, components codegen.Inventory, hasFrontend bool) error {
	if codegen.WorkloadsKCLExists(projectDir) {
		return nil
	}
	var stanzas strings.Builder
	for i, c := range components {
		if i > 0 {
			stanzas.WriteString("\n")
		}
		stanzas.WriteString(codegen.WorkloadStanza(projectName, c))
	}

	// The deploy-time migration step, scaffolded as an ordinary one-shot
	// workload. It goes LAST in the file but FIRST in the ALL list below:
	// k8s runs initContainers in declaration order, so a project that later
	// adds a second one-shot (seed data, provision an IdP) gets it ordered
	// after the migration, and that job sees a current schema.
	if len(components) > 0 {
		stanzas.WriteString("\n")
	}
	stanzas.WriteString(codegen.MigrateWorkloadStanza(projectName))

	// The dev-IdP convergence step. Not broadcast-gated (see the stanza's
	// own comment for why), so its position in the file has no ordering
	// consequence — it is appended after migrate simply because migrate
	// is the project's oldest one-shot.
	if hasFrontend {
		stanzas.WriteString("\n")
		stanzas.WriteString(codegen.IDPProvisionWorkloadStanza(projectName))
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
