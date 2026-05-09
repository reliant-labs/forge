// Package packs implements the pack system: pre-built, opinionated
// implementations that Forge can install into a project. Think of a
// pack like a Rails generator gem — it adds real, working code for a
// specific concern (auth, payments, email, etc.).
package packs

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"

	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/templates"
)

// PackKind identifies the language/runtime a pack targets.
const (
	// PackKindGo is the default — Go code under pkg/, Go module deps.
	PackKindGo = "go"
	// PackKindFrontend installs TypeScript/React assets under
	// frontends/<name>/ and adds npm dependencies. Files and output
	// paths are templated against each frontend in the project.
	PackKindFrontend = "frontend"
)

// Pack represents a loadable pack with its manifest and embedded templates.
type Pack struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Version     string `yaml:"version"`
	// Kind selects the language/runtime the pack targets. "go" (default —
	// or empty for backward compatibility) installs Go files under pkg/ and
	// runs `go get` / `go mod tidy`. "frontend" installs TypeScript/React
	// assets under each frontends/<name>/ directory and runs `npm install`
	// in those directories. See PackKindGo, PackKindFrontend.
	Kind string `yaml:"kind"`
	// Subpath is an informational hint declaring where, under pkg/, the pack
	// prefers its non-proto/non-migration code to live. It documents the pack's
	// chosen organization (e.g. "middleware/auth/jwtauth") and is surfaced by
	// `forge pack info` so a user can see at a glance what subtree the pack
	// touches. Forge does NOT enforce categories or matrix rules — output
	// paths in `files:` and `generate:` are the source of truth. If omitted,
	// the pack is treated as living at the top level under pkg/.
	//
	// For frontend packs, Subpath is informational and describes the path
	// under each frontends/<name>/ directory (e.g. "src/components/data-table").
	Subpath      string          `yaml:"subpath"`
	Config       PackConfig      `yaml:"config"`
	Files        []PackFile      `yaml:"files"`
	Dependencies []string        `yaml:"dependencies"`
	// NPMDependencies lists npm package specs (`name` or `name@version`)
	// installed via `npm install` into each frontend directory. Only
	// honoured when Kind == "frontend".
	NPMDependencies []string `yaml:"npm_dependencies"`
	// ProviderNPMDependencies pulls extra npm deps in keyed by the value of
	// `pack_config.provider`. Lets a single frontend pack ship variant-specific
	// SDKs (e.g. `@clerk/nextjs` for `provider=clerk`, `firebase` for
	// `provider=firebase-auth`) without forcing every install to pay for them.
	// Only honoured when Kind == "frontend".
	ProviderNPMDependencies map[string][]string `yaml:"provider_npm_dependencies"`
	// AllowedThirdParty is the per-pack opt-out for the frontendpacklint
	// soft rule that flags pack templates importing third-party UI libs
	// (@radix-ui/*, @headlessui/*, @tanstack/react-table, ...). Each entry
	// is a package prefix that this pack legitimately needs to wrap
	// (e.g. "@tanstack/react-table" — a headless engine forge wraps with
	// base library primitives, or "recharts" for a charts pack).
	// Only honoured when Kind == "frontend".
	AllowedThirdParty []string        `yaml:"allowed_third_party"`
	Generate          []PackFile      `yaml:"generate"`
	Migrations        []PackMigration `yaml:"migrations"`

	// DependsOn lists the names of OTHER PACKS this pack requires to be
	// installed first. Distinct from Dependencies (Go module deps) and
	// NPMDependencies (npm package deps): DependsOn captures pack-to-pack
	// ordering — e.g. api-key depends on audit-log because the api-key
	// generate hook writes audit entries through the audit_events table
	// that audit-log creates. Forge topologically sorts at install time
	// (auto-installing transitive deps) and at generate time (so consumer
	// generate hooks run after producer hooks).
	//
	// Cycle detection is the loader's responsibility — a cycle is a pack
	// authoring bug, not a project bug, so we surface it loudly. Empty
	// for the common case (most packs are leaves with no pack-to-pack
	// ordering need).
	DependsOn []string `yaml:"depends_on,omitempty"`
}

// EffectiveKind returns the pack kind, defaulting to "go" so that legacy
// pack manifests without a kind field continue to behave as Go packs.
func (p *Pack) EffectiveKind() string {
	switch strings.ToLower(strings.TrimSpace(p.Kind)) {
	case PackKindFrontend:
		return PackKindFrontend
	default:
		return PackKindGo
	}
}

// IsFrontendKind reports whether the pack targets a frontend (TypeScript/React).
func (p *Pack) IsFrontendKind() bool { return p.EffectiveKind() == PackKindFrontend }

// PackMigration describes a single migration that the pack contributes to
// db/migrations/. The migration ID (numeric prefix) is allocated at install
// time by scanning existing migrations — this avoids hardcoded IDs colliding
// across packs and keeps zero-padding consistent with the scaffold (5 digits).
type PackMigration struct {
	// Name is the slug appended after the allocated ID (e.g. "api_keys" →
	// "00002_api_keys.up.sql"). Required.
	Name string `yaml:"name"`
	// Up is the template that renders the up-migration SQL. Required.
	Up string `yaml:"up"`
	// Down is the template that renders the down-migration SQL. Required.
	Down string `yaml:"down"`
	// Description is an optional human description.
	Description string `yaml:"description"`
}

// migrationIDPattern matches the leading numeric prefix of a migration file
// name (e.g. "00001_init.up.sql" → "00001"). Both 4- and 5-digit prefixes
// are recognised so we coexist with legacy projects, but we always emit
// 5-digit IDs (matching the scaffold).
var migrationIDPattern = regexp.MustCompile(`^(\d+)_`)

// migrationIDFormat is the printf format for newly allocated migration IDs.
// Matches the scaffold (00001_init) — 5-digit zero-pad.
const migrationIDFormat = "%05d"

// PackConfig describes the configuration section a pack adds to
// forge.yaml.
type PackConfig struct {
	Section  string         `yaml:"section"`
	Defaults map[string]any `yaml:"defaults"`
}

// PackFile describes a single template→output file mapping.
type PackFile struct {
	Template    string `yaml:"template"`
	Output      string `yaml:"output"`
	Overwrite   string `yaml:"overwrite"`   // "always" | "once" | "never"
	Description string `yaml:"description"` // optional human description
}

// LoadPack loads a pack manifest from the embedded filesystem.
func LoadPack(name string) (*Pack, error) {
	data, err := packsFS.ReadFile(filepath.Join(name, "pack.yaml"))
	if err != nil {
		return nil, fmt.Errorf("pack %q not found: %w", name, err)
	}

	var p Pack
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("parse pack %q manifest: %w", name, err)
	}

	return &p, nil
}

// Install renders and writes pack files into the project, adds
// dependencies, and records the pack in forge.yaml. Behaviour branches
// on EffectiveKind — Go packs run `go get`/`go mod tidy`, frontend packs
// iterate over each project frontend and run `npm install` per frontend.
//
// Equivalent to InstallWithConfig(projectDir, cfg, nil).
func (p *Pack) Install(projectDir string, cfg *config.ProjectConfig) error {
	return p.InstallWithConfig(projectDir, cfg, nil)
}

// InstallWithConfig is Install with per-install config overrides. Overrides
// are merged on top of the pack's `config.defaults` block before templates
// are rendered, so users can pass e.g. `--config provider=clerk` to pick a
// variant exposed by the pack templates as `{{ .PackConfig.provider }}`.
//
// Overrides are surfaced to templates via the standard PackConfig data key.
// Unknown keys are accepted (the pack's templates decide whether to honour
// them) — validation is the pack author's responsibility.
//
// Idempotency: a re-install (pack already listed in cfg.Packs) operates in
// resync mode — files with overwrite=once that already exist are skipped,
// and migrations whose slug already lives in db/migrations/ are skipped
// rather than re-allocated under a fresh sequential ID. Surfacing both as
// "skipping" notes lets `forge pack install <name>` be safely re-run after
// a partial-failure or after the pack ships a new file the project lacks.
//
// Collision safety: for a fresh install (pack not yet in cfg.Packs), if a
// pack file with overwrite=once would land on an existing file the pack did
// not previously emit, the install fails fast with a rename recipe. This
// catches the case where a pack ships a service handler/proto whose name
// the user has already scaffolded — a silent skip would yield a build that
// still references the user's version while the pack thinks it installed.
func (p *Pack) InstallWithConfig(projectDir string, cfg *config.ProjectConfig, overrides map[string]any) error {
	alreadyInstalled := IsInstalled(p.Name, cfg)

	// Merge overrides into config defaults. The merge is shallow — top-level
	// keys win — which is enough for the variant-selection use case
	// (`provider: clerk`) and keeps the contract simple.
	effectiveCfg := mergePackConfig(p.Config.Defaults, overrides)

	if p.IsFrontendKind() {
		return p.installFrontend(projectDir, cfg, effectiveCfg, alreadyInstalled)
	}

	// Build template data from project config
	data := map[string]any{
		"ModulePath":  cfg.ModulePath,
		"ProjectName": cfg.Name,
		"PackConfig":  effectiveCfg,
	}

	// Fresh-install collision detection: scan all pack file outputs and
	// migration slugs before writing anything. If any overwrite=once target
	// already exists (and the pack isn't in cfg.Packs, so we know we didn't
	// write it last time), refuse to proceed and surface the full list with
	// a rename recipe. This is the load-bearing guard against pack-vs-scaffold
	// collisions (e.g. audit-log's handler.go landing on a hand-written file).
	if !alreadyInstalled {
		if collisions := p.detectFreshInstallCollisions(projectDir, data); len(collisions) > 0 {
			return fmt.Errorf("pack %q install would clobber %d existing file(s):\n%s\n\nThe pack was not previously installed (not in forge.yaml's `packs:` list), so these files were authored outside the pack. To proceed, either:\n  - rename or delete the conflicting file(s) so the pack can install cleanly, or\n  - move the conflicting code into a different package and re-run install.\n\nIf you intend to RE-install the pack (the previous install half-completed and forge.yaml lost the entry), add %q under `packs:` in forge.yaml and re-run `forge pack install %s` — that triggers resync mode which respects existing files.",
				p.Name, len(collisions), strings.Join(collisions, "\n"), p.Name, p.Name)
		}
	}

	// Render and write each file
	for _, f := range p.Files {
		if err := p.renderFile(f, projectDir, data); err != nil {
			return fmt.Errorf("render file %s: %w", f.Output, err)
		}
	}

	// Render and write any pack-contributed migrations, allocating sequential
	// IDs based on the project's existing migrations. This is the source of
	// truth for migration filenames — pack manifests do NOT hardcode IDs.
	//
	// Honour the project-level `pack_overrides.<name>.skip_migrations` knob:
	// when set, the pack still installs its files/dependencies/generate hooks
	// but emits no migration files. Useful when the project already owns the
	// schema (e.g. a forge migration of a repo whose own migrations supersede
	// the pack's).
	skipMigrations := false
	if cfg.PackOverrides != nil {
		if ov, ok := cfg.PackOverrides[p.Name]; ok {
			skipMigrations = ov.SkipMigrations
		}
	}
	if skipMigrations && len(p.Migrations) > 0 {
		fmt.Printf("  Skipping %d pack migration(s) (pack_overrides.%s.skip_migrations=true)\n", len(p.Migrations), p.Name)
	} else if len(p.Migrations) > 0 {
		nextID, err := nextMigrationID(projectDir)
		if err != nil {
			return fmt.Errorf("allocate migration ID: %w", err)
		}
		for _, m := range p.Migrations {
			// Idempotency: if a migration with this slug is already on disk
			// (any 4- or 5-digit prefix), skip it rather than allocating a
			// duplicate ID. This covers the partial-install retry path and
			// the resync path equally — re-running `forge pack install
			// audit-log` against a project that already owns the audit_log
			// migration is a no-op for that file.
			existingID, exists, err := findMigrationIDBySlug(projectDir, m.Name)
			if err != nil {
				return fmt.Errorf("check existing migration %s: %w", m.Name, err)
			}
			if exists {
				fmt.Printf("  Skipping migration %s (already at %05d, slug match)\n", m.Name, existingID)
				continue
			}
			if err := p.renderMigration(m, projectDir, data, nextID); err != nil {
				return fmt.Errorf("render migration %s: %w", m.Name, err)
			}
			nextID++
		}
	}

	// Record pack in config BEFORE running go get / go mod tidy. Tidy is a
	// post-action — by this point pack files and migrations are durably on
	// disk, so the pack should be considered installed regardless of whether
	// dep resolution succeeds. Persisting the cfg.Packs entry up front means
	// a tidy/get failure leaves the project in resync mode (forge.yaml
	// reflects what's on disk) instead of a half-installed dead-zone where
	// the next `forge pack install` trips collision detection on its own
	// templated files. The caller persists cfg even when InstallWithConfig
	// returns an error so the on-disk state matches the in-memory state.
	if !alreadyInstalled {
		cfg.Packs = append(cfg.Packs, p.Name)
	}

	// Add go dependencies
	for _, dep := range p.Dependencies {
		fmt.Printf("  Adding dependency: %s\n", dep)
		cmd := exec.Command("go", "get", dep)
		cmd.Dir = projectDir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("go get %s: %w", dep, err)
		}
	}

	// Detect newly-emitted .proto files. If the pack added any, skip tidy:
	// the pack's Go imports point at gen/<proto>/v1 paths that don't exist
	// until `forge generate` runs. Tidy would otherwise fail with "no
	// required module provides package …/gen/<x>/v1". The user must run
	// `forge generate` next; tidy runs there.
	if hasNewProtoFile(p.Files) {
		fmt.Println("  Skipping go mod tidy: pack added .proto files; run 'forge generate' to produce gen/ output and tidy.")
		return nil
	}

	// Defer tidy if a previously-installed pack emitted .proto files whose
	// gen/ counterparts haven't been rendered yet. Without this guard,
	// installing pack B (no proto) after pack A (proto, deferred tidy) would
	// fail tidy because pack A's Go files still import gen/<a>/v1 which
	// doesn't exist on disk yet. The user just needs to run `forge generate`
	// once after the pack-cluster install; surfacing that here keeps later
	// installs from blocking on a known-broken module graph.
	if pending := installedPacksWithUnrenderedProto(projectDir, cfg); len(pending) > 0 {
		fmt.Printf("  Skipping go mod tidy: pack(s) %v emitted .proto files but no gen/ output yet; run 'forge generate' once after this pack-cluster install to render gen/ and tidy.\n", pending)
		return nil
	}

	// Run go mod tidy
	fmt.Println("  Running go mod tidy...")
	cmd := exec.Command("go", "mod", "tidy")
	cmd.Dir = projectDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("go mod tidy: %w", err)
	}

	return nil
}

// installedPacksWithUnrenderedProto returns the names of installed packs
// whose .proto file outputs lack a corresponding gen/<ns>/v1/ directory.
// This is the signal that `forge generate` hasn't been run since the pack
// installed — and therefore that `go mod tidy` will fail because the pack's
// adjacent Go files import gen/<ns>/v1/* that don't exist yet.
//
// Returns nil when all installed packs are either proto-free or have their
// gen output rendered. Best-effort: pack-load failures are skipped silently
// so a missing pack manifest never blocks an install of another pack.
func installedPacksWithUnrenderedProto(projectDir string, cfg *config.ProjectConfig) []string {
	var pending []string
	for _, name := range cfg.Packs {
		ip, err := LoadPack(name)
		if err != nil {
			continue
		}
		for _, f := range ip.Files {
			if !strings.HasSuffix(f.Output, ".proto") {
				continue
			}
			// proto/<ns>/<version>/<file>.proto → gen/<ns>/<version>/
			// We only need the directory, not the file, so check the parent
			// dir of the .proto path remapped from "proto/" to "gen/".
			rel := strings.TrimPrefix(f.Output, "proto/")
			if rel == f.Output {
				// Not under proto/ — can't predict gen/ path; skip.
				continue
			}
			genDir := filepath.Join(projectDir, "gen", filepath.Dir(rel))
			if _, err := os.Stat(genDir); err != nil {
				pending = append(pending, name)
				break
			}
		}
	}
	return pending
}

// hasNewProtoFile reports whether any pack file output is a `.proto` source
// file (i.e. lives under proto/ or has a .proto suffix). Pack-emitted protos
// require `forge generate` before `go mod tidy` can succeed because the Go
// imports in adjacent pack files point at not-yet-generated gen/<x>/v1 paths.
func hasNewProtoFile(files []PackFile) bool {
	for _, f := range files {
		if strings.HasSuffix(f.Output, ".proto") {
			return true
		}
	}
	return false
}

// installFrontend renders and writes pack files into each frontend in the
// project. Output paths and templates are rendered with `FrontendName` and
// `FrontendPath` in scope so a single pack manifest can target every
// frontend the project declares. Migrations are rejected — frontend packs
// don't own DB schema.
//
// alreadyInstalled disables the fresh-install collision check so that
// re-running install on a project that already lists the pack is a safe
// resync rather than a hard error.
func (p *Pack) installFrontend(projectDir string, cfg *config.ProjectConfig, effectiveCfg map[string]any, alreadyInstalled bool) error {
	if len(p.Migrations) > 0 {
		return fmt.Errorf("frontend pack %q must not declare migrations", p.Name)
	}
	if len(cfg.Frontends) == 0 {
		return fmt.Errorf("pack %q is a frontend pack but the project has no frontends — pass --frontend <name> to forge new", p.Name)
	}

	// Resolve any provider-keyed extra npm dependencies. Frontend packs that
	// expose a `provider:` config knob can declare a `provider_npm_dependencies`
	// map keyed by provider value to pull in the right SDK at install time.
	npmDeps := append([]string(nil), p.NPMDependencies...)
	if extra := p.providerNPMDeps(effectiveCfg); len(extra) > 0 {
		npmDeps = append(npmDeps, extra...)
	}

	for _, fe := range cfg.Frontends {
		fmt.Printf("  Installing into frontend %q...\n", fe.Name)
		feDir := fe.Path
		if feDir == "" {
			feDir = filepath.Join("frontends", fe.Name)
		}
		data := map[string]any{
			"ModulePath":   cfg.ModulePath,
			"ProjectName":  cfg.Name,
			"PackConfig":   effectiveCfg,
			"FrontendName": fe.Name,
			"FrontendPath": feDir,
			"FrontendType": fe.Type,
			"FrontendKind": fe.Kind,
		}

		// Per-frontend fresh-install collision check — same semantics as the
		// Go-pack path above.
		if !alreadyInstalled {
			if collisions := p.detectFreshInstallCollisions(projectDir, data); len(collisions) > 0 {
				return fmt.Errorf("pack %q install would clobber %d existing file(s) in frontend %q:\n%s\n\nThe pack was not previously installed. Rename or delete the conflicting file(s), or move the existing code into a different module before re-running install.",
					p.Name, len(collisions), fe.Name, strings.Join(collisions, "\n"))
			}
		}

		for _, f := range p.Files {
			if err := p.renderFile(f, projectDir, data); err != nil {
				return fmt.Errorf("render file %s for frontend %s: %w", f.Output, fe.Name, err)
			}
		}

		// Install npm dependencies into this frontend.
		if len(npmDeps) > 0 {
			absFE := filepath.Join(projectDir, feDir)
			if _, err := os.Stat(absFE); err != nil {
				return fmt.Errorf("frontend directory %s not found: %w", feDir, err)
			}
			args := append([]string{"install", "--save"}, npmDeps...)
			fmt.Printf("  Running npm install in %s: %s\n", feDir, strings.Join(npmDeps, " "))
			cmd := exec.Command("npm", args...)
			cmd.Dir = absFE
			cmd.Stdout = os.Stdout
			cmd.Stderr = os.Stderr
			if err := cmd.Run(); err != nil {
				return fmt.Errorf("npm install in %s: %w", feDir, err)
			}
		}
	}

	if !alreadyInstalled {
		cfg.Packs = append(cfg.Packs, p.Name)
	}
	return nil
}

// Remove deletes files created by the pack and removes it from the
// project config. Dependencies (go modules or npm packages) are left in
// place since they may be used by other code.
func (p *Pack) Remove(projectDir string, cfg *config.ProjectConfig) error {
	// Build the iteration set: Go packs have a single render context,
	// frontend packs have one per declared frontend.
	dataSets := []map[string]any{
		{"ModulePath": cfg.ModulePath, "ProjectName": cfg.Name, "PackConfig": p.Config.Defaults},
	}
	if p.IsFrontendKind() {
		dataSets = dataSets[:0]
		for _, fe := range cfg.Frontends {
			feDir := fe.Path
			if feDir == "" {
				feDir = filepath.Join("frontends", fe.Name)
			}
			dataSets = append(dataSets, map[string]any{
				"ModulePath":   cfg.ModulePath,
				"ProjectName":  cfg.Name,
				"PackConfig":   p.Config.Defaults,
				"FrontendName": fe.Name,
				"FrontendPath": feDir,
				"FrontendType": fe.Type,
				"FrontendKind": fe.Kind,
			})
		}
	}

	// Delete files created by the pack (one rendered set per frontend for
	// frontend packs; one set total for Go packs).
	for _, ds := range dataSets {
		for _, f := range p.Files {
			rendered, err := renderPathTemplate(f.Output, ds)
			if err != nil {
				fmt.Printf("  Warning: could not resolve %s: %v\n", f.Output, err)
				continue
			}
			target := filepath.Join(projectDir, rendered)
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				fmt.Printf("  Warning: could not remove %s: %v\n", rendered, err)
			} else if err == nil {
				fmt.Printf("  Removed: %s\n", rendered)
			}
		}
	}

	// Delete pack-contributed migrations by matching the slug suffix —
	// the numeric prefix was allocated at install time so we discover it.
	for _, m := range p.Migrations {
		removed, err := removeMigrationsBySlug(projectDir, m.Name)
		if err != nil {
			fmt.Printf("  Warning: could not remove migration %s: %v\n", m.Name, err)
		}
		for _, path := range removed {
			fmt.Printf("  Removed: %s\n", path)
		}
	}

	// Also remove generate-hook outputs
	for _, ds := range dataSets {
		for _, f := range p.Generate {
			rendered, err := renderPathTemplate(f.Output, ds)
			if err != nil {
				fmt.Printf("  Warning: could not resolve %s: %v\n", f.Output, err)
				continue
			}
			target := filepath.Join(projectDir, rendered)
			if err := os.Remove(target); err != nil && !os.IsNotExist(err) {
				fmt.Printf("  Warning: could not remove %s: %v\n", rendered, err)
			} else if err == nil {
				fmt.Printf("  Removed: %s\n", rendered)
			}
		}
	}

	// Remove from packs list
	filtered := cfg.Packs[:0]
	for _, name := range cfg.Packs {
		if name != p.Name {
			filtered = append(filtered, name)
		}
	}
	cfg.Packs = filtered

	return nil
}

// RenderGenerateFiles re-renders the pack's generate-hook templates.
// Called during `forge generate` to keep pack-generated code up to date.
func (p *Pack) RenderGenerateFiles(projectDir string, cfg *config.ProjectConfig) error {
	data := map[string]any{
		"ModulePath":  cfg.ModulePath,
		"ProjectName": cfg.Name,
		"PackConfig":  p.Config.Defaults,
	}

	for _, f := range p.Generate {
		if err := p.renderFile(f, projectDir, data); err != nil {
			return fmt.Errorf("render generate file %s: %w", f.Output, err)
		}
	}
	return nil
}

// renderFile renders a single template file and writes it to the project.
// The output path itself is treated as a Go template so frontend packs can
// write into frontends/{{.FrontendName}}/... without hardcoding a name.
// For pack manifests with no `{{` in the path the input is returned
// unchanged (no parsing cost).
func (p *Pack) renderFile(f PackFile, projectDir string, data map[string]any) error {
	resolvedOutput, err := renderPathTemplate(f.Output, data)
	if err != nil {
		return fmt.Errorf("render output path %q: %w", f.Output, err)
	}
	target := filepath.Join(projectDir, resolvedOutput)

	// Check overwrite policy
	if f.Overwrite == "never" || f.Overwrite == "once" {
		if _, err := os.Stat(target); err == nil {
			if f.Overwrite == "never" {
				fmt.Printf("  Skipping (exists): %s\n", resolvedOutput)
				return nil
			}
			// "once" means skip if already written by pack before
			// For simplicity, treat same as "never" on re-install
			fmt.Printf("  Skipping (already exists): %s\n", resolvedOutput)
			return nil
		}
	}

	// Render template using the shared template engine
	basePath := filepath.Join(p.Name, "templates")
	content, err := templates.RenderFromFS(packsFS, basePath, f.Template, data)
	if err != nil {
		return fmt.Errorf("render template %s: %w", f.Template, err)
	}

	// Ensure output directory exists
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return fmt.Errorf("create directory for %s: %w", resolvedOutput, err)
	}

	// Write the file
	if err := os.WriteFile(target, content, 0644); err != nil {
		return fmt.Errorf("write %s: %w", resolvedOutput, err)
	}

	fmt.Printf("  Created: %s\n", resolvedOutput)
	return nil
}

// renderPathTemplate evaluates a Go-template string against data. Plain
// (template-free) inputs short-circuit so the common case has no cost.
func renderPathTemplate(in string, data map[string]any) (string, error) {
	if !strings.Contains(in, "{{") {
		return in, nil
	}
	tmpl, err := template.New("path").Parse(in)
	if err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// IsInstalled checks whether a pack is in the installed list.
func IsInstalled(name string, cfg *config.ProjectConfig) bool {
	for _, p := range cfg.Packs {
		if p == name {
			return true
		}
	}
	return false
}

// InstalledPacks returns the list of Pack structs for all installed packs.
func InstalledPacks(cfg *config.ProjectConfig) ([]*Pack, error) {
	var result []*Pack
	for _, name := range cfg.Packs {
		pack, err := LoadPack(name)
		if err != nil {
			// Pack was removed from Forge but still referenced in config
			fmt.Fprintf(os.Stderr, "  Warning: installed pack %q not found: %v\n", name, err)
			continue
		}
		result = append(result, pack)
	}
	return result, nil
}

// renderMigration renders a single pack migration template pair (up + down)
// using the supplied numeric ID. The output filename is built from the ID and
// the migration's Name (e.g. id=2, name="api_keys" → "00002_api_keys.up.sql").
func (p *Pack) renderMigration(m PackMigration, projectDir string, data map[string]any, id int) error {
	if m.Name == "" {
		return fmt.Errorf("migration entry missing required 'name'")
	}
	if m.Up == "" || m.Down == "" {
		return fmt.Errorf("migration %q missing required 'up' or 'down' template", m.Name)
	}

	prefix := fmt.Sprintf(migrationIDFormat, id)
	upOutput := filepath.Join("db", "migrations", fmt.Sprintf("%s_%s.up.sql", prefix, m.Name))
	downOutput := filepath.Join("db", "migrations", fmt.Sprintf("%s_%s.down.sql", prefix, m.Name))

	upFile := PackFile{Template: m.Up, Output: upOutput, Overwrite: "once", Description: m.Description}
	downFile := PackFile{Template: m.Down, Output: downOutput, Overwrite: "once", Description: m.Description}

	if err := p.renderFile(upFile, projectDir, data); err != nil {
		return err
	}
	return p.renderFile(downFile, projectDir, data)
}

// detectFreshInstallCollisions returns the relative paths of every pack file
// (Files + Generate, plus migration up/down for the listed slug) whose
// rendered target already exists on disk. It is only meaningful for fresh
// installs — once a pack is recorded in cfg.Packs we treat existing pack
// files as expected (resync mode skips them per overwrite policy).
//
// Files declared with overwrite=always are intentionally ignored: the pack
// author has explicitly opted into clobbering on every install.
//
// migration files are detected by slug (any digit prefix) so a previously
// half-installed migration with a now-stale ID still trips the guard and
// surfaces the rename recipe.
func (p *Pack) detectFreshInstallCollisions(projectDir string, data map[string]any) []string {
	var collisions []string
	check := func(f PackFile) {
		if strings.EqualFold(strings.TrimSpace(f.Overwrite), "always") {
			return
		}
		rendered, err := renderPathTemplate(f.Output, data)
		if err != nil {
			// If we can't even render the path, fall back to flagging the
			// raw template — better a confusing message than a silent skip.
			rendered = f.Output
		}
		target := filepath.Join(projectDir, rendered)
		if _, err := os.Stat(target); err == nil {
			collisions = append(collisions, "  - "+rendered)
		}
	}
	for _, f := range p.Files {
		check(f)
	}
	for _, f := range p.Generate {
		check(f)
	}
	for _, m := range p.Migrations {
		if id, exists, err := findMigrationIDBySlug(projectDir, m.Name); err == nil && exists {
			collisions = append(collisions,
				fmt.Sprintf("  - db/migrations/%05d_%s.{up,down}.sql", id, m.Name))
		}
	}
	sort.Strings(collisions)
	return collisions
}

// findMigrationIDBySlug returns the existing migration ID for a slug if any
// file matching `<digits>_<slug>.up.sql` (or `.down.sql`) is present under
// db/migrations/. Reports exists=false (no error) when the directory is
// missing — that's the fresh-project case, not a failure.
func findMigrationIDBySlug(projectDir, slug string) (id int, exists bool, err error) {
	dir := filepath.Join(projectDir, "db", "migrations")
	entries, statErr := os.ReadDir(dir)
	if statErr != nil {
		if os.IsNotExist(statErr) {
			return 0, false, nil
		}
		return 0, false, statErr
	}
	suffixes := []string{"_" + slug + ".up.sql", "_" + slug + ".down.sql"}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		match := migrationIDPattern.FindStringSubmatch(name)
		if len(match) < 2 {
			continue
		}
		hit := false
		for _, suf := range suffixes {
			if strings.HasSuffix(name, suf) {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		n, convErr := strconv.Atoi(match[1])
		if convErr != nil {
			continue
		}
		return n, true, nil
	}
	return 0, false, nil
}

// nextMigrationID returns the next available numeric migration ID by scanning
// db/migrations/ for existing files. If the directory is empty or absent, the
// next ID is 1. Both 4- and 5-digit zero-padded prefixes are recognised.
func nextMigrationID(projectDir string) (int, error) {
	dir := filepath.Join(projectDir, "db", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 1, nil
		}
		return 0, err
	}

	max := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		match := migrationIDPattern.FindStringSubmatch(e.Name())
		if len(match) < 2 {
			continue
		}
		n, err := strconv.Atoi(match[1])
		if err != nil {
			continue
		}
		if n > max {
			max = n
		}
	}
	return max + 1, nil
}

// removeMigrationsBySlug deletes migration files whose name matches
// "<digits>_<slug>.{up,down}.sql". Returns the relative paths that were
// removed so callers can log them.
func removeMigrationsBySlug(projectDir, slug string) ([]string, error) {
	dir := filepath.Join(projectDir, "db", "migrations")
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	suffixes := []string{"_" + slug + ".up.sql", "_" + slug + ".down.sql"}
	var removed []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		match := migrationIDPattern.FindStringSubmatch(name)
		if len(match) < 2 {
			continue
		}
		hit := false
		for _, suf := range suffixes {
			if strings.HasSuffix(name, suf) {
				hit = true
				break
			}
		}
		if !hit {
			continue
		}
		full := filepath.Join(dir, name)
		if err := os.Remove(full); err != nil {
			return removed, err
		}
		removed = append(removed, filepath.Join("db", "migrations", name))
	}
	sort.Strings(removed)
	return removed, nil
}

// ValidPackName checks that a pack name contains only safe characters.
func ValidPackName(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
			return false
		}
	}
	return !strings.HasPrefix(name, "-") && !strings.HasPrefix(name, "_")
}

// mergePackConfig produces the PackConfig map exposed to templates: a
// shallow copy of `defaults` with `overrides` keys winning. Either side may
// be nil. The merge is intentionally shallow — it covers the variant-knob
// use case (`provider: clerk`) and keeps the contract auditable. Packs that
// need deeper merges should declare a single nested map under one knob.
func mergePackConfig(defaults, overrides map[string]any) map[string]any {
	out := make(map[string]any, len(defaults)+len(overrides))
	for k, v := range defaults {
		out[k] = v
	}
	for k, v := range overrides {
		out[k] = v
	}
	return out
}

// providerNPMDeps returns the extra npm deps to install for the provider
// selected via PackConfig (key: "provider"). Returns nil if no provider key
// is set or the pack does not declare provider-keyed deps.
func (p *Pack) providerNPMDeps(effectiveCfg map[string]any) []string {
	if len(p.ProviderNPMDependencies) == 0 {
		return nil
	}
	rawProvider, ok := effectiveCfg["provider"]
	if !ok {
		return nil
	}
	provider, ok := rawProvider.(string)
	if !ok || provider == "" {
		return nil
	}
	return p.ProviderNPMDependencies[provider]
}

// ResolveInstallOrder takes a set of pack names the user wants installed
// (or that are already installed) and returns those names PLUS any
// transitive `depends_on` packs in topological order — producers first,
// consumers last. Names that are already in `existingInstalled` are
// preserved at the head of the returned slice (existing order respected)
// and any new transitive deps surface AFTER them but BEFORE the
// requested-but-not-yet-installed packs.
//
// Returns an error on:
//   - unknown pack name (typo / pack removed from forge)
//   - dependency cycle (pack-author bug — surfaces the cycle path)
//
// `requested` may include packs that are already in `existingInstalled`;
// the result deduplicates. Caller is responsible for skipping the
// install-side effects on already-installed packs (resync mode does
// this naturally).
func ResolveInstallOrder(requested []string, existingInstalled []string) ([]string, error) {
	// Walk the dep graph from each requested pack. We use iterative DFS
	// with three colors so we can both topo-sort and detect cycles in
	// one pass.
	const (
		white = 0 // unvisited
		gray  = 1 // on current DFS stack
		black = 2 // done
	)

	color := map[string]int{}
	var order []string

	// Pre-mark already-installed packs so they appear at the front of
	// the returned slice in their existing order. These don't need to
	// be re-visited; we just emit them first.
	installedSet := map[string]bool{}
	for _, name := range existingInstalled {
		installedSet[name] = true
	}

	var visit func(name string, path []string) error
	visit = func(name string, path []string) error {
		switch color[name] {
		case black:
			return nil
		case gray:
			// Cycle. Surface the path including the re-encountered node
			// so the user sees A → B → C → A.
			cycle := append([]string(nil), path...)
			cycle = append(cycle, name)
			return fmt.Errorf("pack dependency cycle: %s", strings.Join(cycle, " → "))
		}
		color[name] = gray

		p, err := LoadPack(name)
		if err != nil {
			return fmt.Errorf("resolve pack dependencies for %q: %w", name, err)
		}
		for _, dep := range p.DependsOn {
			if err := visit(dep, append(path, name)); err != nil {
				return err
			}
		}
		color[name] = black
		// Skip emission for already-installed packs (we'll prepend them).
		if !installedSet[name] {
			order = append(order, name)
		}
		return nil
	}

	for _, name := range requested {
		if err := visit(name, nil); err != nil {
			return nil, err
		}
	}

	// Emit existing installed first (preserving their order), then the
	// freshly-resolved order. The existing-order preservation matters
	// because cfg.Packs reflects historical install order and downstream
	// (forge audit, forge pack list) prints based on that ordering.
	out := make([]string, 0, len(existingInstalled)+len(order))
	out = append(out, existingInstalled...)
	out = append(out, order...)
	return out, nil
}

// SortInstalledByDependencies returns the input pack names in
// dependency-respecting order: producers (depended-on) before consumers.
// Used by `forge generate` so pack generate hooks run in the right order
// when one pack's hook references another pack's generated output.
//
// Unknown pack names are silently dropped (matching InstalledPacks's
// "warn-and-continue" semantics — a pack removed from forge but still
// listed in cfg.Packs is a known soft failure mode).
func SortInstalledByDependencies(installed []string) ([]string, error) {
	const (
		white = 0
		gray  = 1
		black = 2
	)
	color := map[string]int{}
	var order []string

	// Build set membership for "is this pack actually installed?". Deps
	// the manifest declares but the project hasn't installed are skipped
	// during generate-time sort — install-time is where missing deps get
	// surfaced, not generate-time.
	inSet := map[string]bool{}
	for _, name := range installed {
		inSet[name] = true
	}

	var visit func(name string, path []string) error
	visit = func(name string, path []string) error {
		if !inSet[name] {
			return nil
		}
		switch color[name] {
		case black:
			return nil
		case gray:
			cycle := append([]string(nil), path...)
			cycle = append(cycle, name)
			return fmt.Errorf("pack dependency cycle: %s", strings.Join(cycle, " → "))
		}
		color[name] = gray
		p, err := LoadPack(name)
		if err != nil {
			// Mirror InstalledPacks: warn-and-continue (don't fail
			// generate just because a pack manifest is missing).
			color[name] = black
			return nil
		}
		for _, dep := range p.DependsOn {
			if err := visit(dep, append(path, name)); err != nil {
				return err
			}
		}
		color[name] = black
		order = append(order, name)
		return nil
	}

	for _, name := range installed {
		if err := visit(name, nil); err != nil {
			return nil, err
		}
	}
	return order, nil
}

// MissingDependencies returns the names of packs that the listed installed
// packs declare in `depends_on` but which are NOT in `installed`. Used by
// `forge audit` to surface "pack graph health" issues — e.g. someone
// hand-edited cfg.Packs to remove audit-log while leaving api-key.
//
// Unknown packs are skipped silently. The result is deduplicated and
// sorted for stable output.
func MissingDependencies(installed []string) []string {
	inSet := map[string]bool{}
	for _, n := range installed {
		inSet[n] = true
	}
	missingSet := map[string]struct{}{}
	for _, name := range installed {
		p, err := LoadPack(name)
		if err != nil {
			continue
		}
		for _, dep := range p.DependsOn {
			if !inSet[dep] {
				missingSet[dep] = struct{}{}
			}
		}
	}
	out := make([]string, 0, len(missingSet))
	for k := range missingSet {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ParseConfigOverrides parses `key=value` strings (typically from a CLI
// `--config` flag) into a config map. Bare booleans/integers are passed
// through as strings — the templates can coerce as needed. Returns an
// error on a missing `=` separator or empty key.
func ParseConfigOverrides(pairs []string) (map[string]any, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	out := make(map[string]any, len(pairs))
	for _, p := range pairs {
		i := strings.Index(p, "=")
		if i < 0 {
			return nil, fmt.Errorf("invalid --config %q: expected key=value", p)
		}
		k := strings.TrimSpace(p[:i])
		v := strings.TrimSpace(p[i+1:])
		if k == "" {
			return nil, fmt.Errorf("invalid --config %q: empty key", p)
		}
		out[k] = v
	}
	return out, nil
}