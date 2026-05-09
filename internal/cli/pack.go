package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cliutil"
	"github.com/reliant-labs/forge/internal/generator"
	"github.com/reliant-labs/forge/internal/packs"
)

func newPackCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "pack",
		Short: "Manage installable packs",
		Long: `Manage installable packs — pre-built, opinionated implementations
that add real, working code for specific concerns (auth, payments, etc.).

Subcommands:
  forge pack list              List available packs
  forge pack add <name>        Install a pack into the project (alias: install)
  forge pack remove <name>     Remove a pack from the project (alias: uninstall)`,
	}

	cmd.AddCommand(newPackListCmd())
	cmd.AddCommand(newPackInstallCmd())
	cmd.AddCommand(newPackRemoveCmd())
	cmd.AddCommand(newPackInfoCmd())

	return cmd
}

// newPackInfoCmd builds the `forge pack info <name>` subcommand. The
// summary is derived from the embedded pack.yaml manifest and (when the
// command runs inside a project) from the project's existing files so we
// can flag potential collisions before the user runs `pack install`.
//
// This is the LLM-friendly "what does this pack do without me reading
// every template?" surface — it lists proto outputs, Go package outputs,
// migrations, dependencies, generate hooks, conflicts, and the
// forge.yaml additions the install would record.
func newPackInfoCmd() *cobra.Command {
	var jsonFlag bool
	cmd := &cobra.Command{
		Use:   "info <name>",
		Short: "Show summary for a pack: outputs, deps, conflicts, install hooks",
		Long: `Print a human-readable summary of what a pack would emit.

Pulls from the pack manifest (embedded pack.yaml) and, when run inside a
project, cross-references existing files to highlight potential collisions
before install.

Output fields:
  - description: pack manifest description
  - kind, version, subpath: manifest header
  - proto files emitted: every .proto under proto/<ns>/ the pack would write
  - Go packages emitted: derived from non-proto Go file outputs
  - npm dependencies: resolved including provider-keyed extras
  - dependencies: Go module deps that 'forge pack install' will 'go get'
  - migrations: pack-contributed db/migrations/ files
  - generate hooks: codegen outputs run on 'forge generate'
  - forge.yaml additions: cfg.Packs entry + any pack_config keys
  - post-install hooks: tidy, npm install, generate notes
  - conflicts: pack outputs that already exist in the project (project mode only)

Examples:
  forge pack info audit-log
  forge pack info jwt-auth --json`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPackInfo(args[0], jsonFlag)
		},
	}
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Emit machine-readable JSON instead of human-readable text")
	return cmd
}

func newPackListCmd() *cobra.Command {
	var depsFlag bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List available packs",
		Long: `List packs available for installation, with installed-status when run
inside a project.

Use --deps to render the pack-to-pack dependency graph: which packs
declare depends_on which other packs. Producers (audit-log, …) appear
as roots; consumers (api-key, …) hang under them. Useful for figuring
out the right install order before running 'forge pack add'.

Examples:
  forge pack list
  forge pack list --deps`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if depsFlag {
				return runPackListDeps()
			}
			return runPackList()
		},
	}
	cmd.Flags().BoolVar(&depsFlag, "deps", false, "Show the pack-to-pack dependency graph instead of the table")
	return cmd
}

func newPackInstallCmd() *cobra.Command {
	var configPairs []string
	cmd := &cobra.Command{
		Use:   "install <name>",
		// `add` is the alias to mirror `forge add operator/worker/crd` — the
		// "install vs add" inconsistency was a real LLM friction point during
		// the control-plane-next port. Both verbs map to the same RunE.
		Aliases: []string{"add"},
		Short:   "Install a pack into the project",
		Long: `Install a pack into the current Forge project. This will:

  1. Read the pack manifest
  2. Render templates with project config (module path, service name, etc.)
  3. Write files to the project
  4. Add Go dependencies
  5. Record the pack in forge.yaml
  6. Run go mod tidy

Per-pack config knobs declared in pack.yaml (under config.defaults) can be
overridden at install time with --config key=value (repeatable). The
override is shallow-merged on top of defaults and surfaced to templates as
{{ .PackConfig.<key> }}.

Examples:
  forge pack install jwt-auth
  forge pack install auth-ui --config provider=clerk`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPackInstall(args[0], configPairs)
		},
	}
	cmd.Flags().StringSliceVar(&configPairs, "config", nil,
		"Override pack config values (key=value). Repeatable; e.g. --config provider=clerk")
	return cmd
}

func newPackRemoveCmd() *cobra.Command {
	return &cobra.Command{
		Use: "remove <name>",
		// `uninstall` is accepted as an alias for symmetry with `install`.
		Aliases: []string{"uninstall"},
		Short:   "Remove a pack from the project",
		Long: `Remove a pack from the current Forge project. This will:

  1. Delete files created by the pack
  2. Remove the pack from forge.yaml
  3. Note: Go dependencies are NOT removed (they may be used elsewhere)

Example:
  forge pack remove jwt-auth`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPackRemove(args[0])
		},
	}
}

func runPackList() error {
	available, err := packs.ListPacks()
	if err != nil {
		return fmt.Errorf("list packs: %w", err)
	}

	if len(available) == 0 {
		fmt.Println("No packs available.")
		return nil
	}

	// Check which are installed (if we're in a project)
	var installed map[string]bool
	cfg, cfgErr := loadProjectConfig()
	if cfgErr == nil {
		installed = make(map[string]bool)
		for _, name := range cfg.Packs {
			installed[name] = true
		}
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "NAME\tVERSION\tSUBPATH\tSTATUS\tDESCRIPTION")
	for _, p := range available {
		status := ""
		if installed != nil && installed[p.Name] {
			status = "installed"
		}
		// Surface the pack-declared subpath under pkg/ so users can see at a
		// glance what subtree the install touches. Empty subpath = top-level.
		subpath := p.Subpath
		if subpath == "" {
			subpath = "(root)"
		}
		// Truncate description on first newline so the table stays tidy.
		desc := p.Description
		if i := indexByte(desc, '\n'); i >= 0 {
			desc = desc[:i]
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\n", p.Name, p.Version, subpath, status, desc)
	}
	return w.Flush()
}

// runPackListDeps renders the pack-to-pack dependency graph as a
// roots-first tree. Roots are packs no other pack depends on (or that
// declare no `depends_on` themselves and aren't depended on); each node
// indents its dependents one level. Useful as an LLM-friendly reference
// for "what installs which" before `forge pack add`.
func runPackListDeps() error {
	available, err := packs.ListPacks()
	if err != nil {
		return fmt.Errorf("list packs: %w", err)
	}
	if len(available) == 0 {
		fmt.Println("No packs available.")
		return nil
	}

	// Build forward (consumer→producer) and reverse (producer→consumers) maps.
	consumers := map[string][]string{}
	declares := map[string][]string{}
	for _, p := range available {
		declares[p.Name] = append([]string(nil), p.DependsOn...)
		for _, dep := range p.DependsOn {
			consumers[dep] = append(consumers[dep], p.Name)
		}
	}

	// Roots: packs with no producer (no depends_on entries). These are
	// the install-first leaves of the topological order. We render roots
	// alphabetically; consumers indent under each root.
	var roots []string
	for _, p := range available {
		if len(p.DependsOn) == 0 {
			roots = append(roots, p.Name)
		}
	}
	sort.Strings(roots)

	fmt.Println("Pack dependency graph (producers → consumers):")
	fmt.Println()

	// Print each root + its dependents recursively. Use a visited set so
	// a future cycle (forbidden but possible in author error) doesn't
	// loop forever.
	visited := map[string]bool{}
	var print func(name string, depth int)
	print = func(name string, depth int) {
		if visited[name] {
			fmt.Printf("%s- %s (cycle detected — already visited)\n",
				strings.Repeat("  ", depth), name)
			return
		}
		visited[name] = true
		marker := "•"
		if depth == 0 {
			marker = "▸"
		}
		fmt.Printf("%s%s %s\n", strings.Repeat("  ", depth), marker, name)
		kids := append([]string(nil), consumers[name]...)
		sort.Strings(kids)
		for _, k := range kids {
			print(k, depth+1)
		}
	}
	for _, r := range roots {
		print(r, 0)
	}

	// Orphan check: any pack neither in roots nor reachable from a root
	// (i.e. part of a cycle, or unreferenced consumer with bad deps).
	for _, p := range available {
		if !visited[p.Name] {
			fmt.Printf("⚠ %s (unreachable — likely cycle or missing producer)\n", p.Name)
		}
	}

	return nil
}

// indexByte returns the first index of c in s, or -1 if absent. Inlined to
// avoid pulling in strings just for this one call.
func indexByte(s string, c byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func runPackInstall(name string, configPairs []string) error {
	ctxLabel := fmt.Sprintf("forge pack add %s", name)

	if !packs.ValidPackName(name) {
		return cliutil.UserErr(ctxLabel,
			fmt.Sprintf("invalid pack name %q", name),
			"",
			"run 'forge pack list' to see available packs (names are lowercase letters, digits, hyphens)")
	}

	root, err := projectRoot()
	if err != nil {
		return err
	}

	configPath := filepath.Join(root, "forge.yaml")
	cfg, err := generator.ReadProjectConfig(configPath)
	if err != nil {
		return cliutil.WrapUserErr(ctxLabel,
			"read project config",
			configPath,
			"verify forge.yaml is valid YAML and you are in a forge project root",
			err)
	}

	// Sanity-check the pack exists before doing dependency resolution so
	// the error message points at the actual root cause rather than at a
	// missing-producer chain.
	if _, err := packs.GetPack(name); err != nil {
		return cliutil.UserErr(ctxLabel,
			fmt.Sprintf("pack %q is not available in this forge build", name),
			"",
			"run 'forge pack list' to see installable packs; check the pack name spelling")
	}

	// Resolve the full install set: requested pack + any transitive
	// `depends_on` packs that aren't installed yet. ResolveInstallOrder
	// returns the existing-installed packs first (preserving order),
	// then the new packs in topological order — producers before
	// consumers. So if the user runs `forge pack add api-key` in a
	// fresh project, the resolved order is [audit-log, api-key].
	order, err := packs.ResolveInstallOrder([]string{name}, cfg.Packs)
	if err != nil {
		return cliutil.WrapUserErr(ctxLabel,
			"resolve pack dependencies",
			"",
			"run 'forge pack list --deps' to inspect the pack dependency graph",
			err)
	}

	// Carve out the packs we need to install (skip what's already there).
	alreadyInstalled := map[string]bool{}
	for _, n := range cfg.Packs {
		alreadyInstalled[n] = true
	}
	var toInstall []string
	for _, n := range order {
		if alreadyInstalled[n] {
			continue
		}
		toInstall = append(toInstall, n)
	}
	// If the user re-installs a pack that's already in cfg.Packs,
	// ResolveInstallOrder won't surface it (because installedSet skips
	// emission). But we still want to honour the resync semantics for
	// the explicitly-requested pack — append it back if we elided it.
	if len(toInstall) == 0 || toInstall[len(toInstall)-1] != name {
		if alreadyInstalled[name] {
			toInstall = append(toInstall, name)
		}
	}

	overrides, err := packs.ParseConfigOverrides(configPairs)
	if err != nil {
		return err
	}

	// Auto-install dep banner: when we're pulling in transitive packs,
	// surface the chain so the user knows install ordering wasn't a
	// surprise. We only print the banner when there's >1 pack to install
	// AND the user only asked for one.
	if len(toInstall) > 1 {
		var deps []string
		for _, n := range toInstall {
			if n != name {
				deps = append(deps, n)
			}
		}
		fmt.Printf("Pack '%s' depends on: %v — installing in topological order.\n", name, deps)
	}

	for _, packName := range toInstall {
		pack, err := packs.GetPack(packName)
		if err != nil {
			return err
		}

		// Config overrides only apply to the EXPLICITLY-requested pack.
		// Transitive deps install with their pack-defined defaults — the
		// user wasn't asking to configure those packs, only to satisfy
		// the dep. If you want to configure a transitive dep, install
		// it explicitly first.
		var thisOverrides map[string]any
		if packName == name {
			thisOverrides = overrides
		}

		if packs.IsInstalled(packName, cfg) {
			fmt.Printf("Re-installing pack '%s' v%s (resync — existing files preserved)...\n", pack.Name, pack.Version)
		} else {
			fmt.Printf("Installing pack '%s' v%s...\n", pack.Name, pack.Version)
		}
		if len(thisOverrides) > 0 {
			fmt.Printf("  Config overrides: %v\n", thisOverrides)
		}

		installErr := pack.InstallWithConfig(root, cfg, thisOverrides)

		// Persist cfg.Packs after EVERY successful pack so a partial
		// failure mid-chain leaves a coherent forge.yaml. If install
		// errored, still try to persist (InstallWithConfig appends to
		// cfg.Packs before running `go get` / `go mod tidy`).
		if writeErr := generator.WriteProjectConfigFile(cfg, configPath); writeErr != nil {
			if installErr != nil {
				return fmt.Errorf("install pack %q: %w (additionally, update project config failed: %v)", packName, installErr, writeErr)
			}
			return fmt.Errorf("update project config: %w", writeErr)
		}

		if installErr != nil {
			return fmt.Errorf("install pack %q: %w", packName, installErr)
		}

		fmt.Printf("\n✅ Pack '%s' installed successfully!\n", pack.Name)
		if len(pack.Generate) > 0 {
			fmt.Printf("\nThis pack has generate hooks. Run '%s generate' to generate pack code.\n", CLIName())
		}
	}

	return nil
}

func runPackRemove(name string) error {
	ctxLabel := fmt.Sprintf("forge pack remove %s", name)
	if !packs.ValidPackName(name) {
		return cliutil.UserErr(ctxLabel,
			fmt.Sprintf("invalid pack name %q", name),
			"",
			"run 'forge pack list' to see installed packs (names are lowercase letters, digits, hyphens)")
	}

	root, err := projectRoot()
	if err != nil {
		return err
	}

	configPath := filepath.Join(root, "forge.yaml")
	cfg, err := generator.ReadProjectConfig(configPath)
	if err != nil {
		return cliutil.WrapUserErr(ctxLabel,
			"read project config",
			configPath,
			"verify forge.yaml is valid YAML",
			err)
	}

	if !packs.IsInstalled(name, cfg) {
		return cliutil.UserErr(ctxLabel,
			fmt.Sprintf("pack %q is not installed", name),
			"",
			"run 'forge pack list' to see which packs are installed")
	}

	pack, err := packs.GetPack(name)
	if err != nil {
		return err
	}

	fmt.Printf("Removing pack '%s'...\n", pack.Name)

	if err := pack.Remove(root, cfg); err != nil {
		return fmt.Errorf("remove pack %q: %w", name, err)
	}

	// Write updated config
	if err := generator.WriteProjectConfigFile(cfg, configPath); err != nil {
		return fmt.Errorf("update project config: %w", err)
	}

	fmt.Printf("\n✅ Pack '%s' removed.\n", pack.Name)
	fmt.Println("Note: Go dependencies were not removed (they may be used by other code).")

	return nil
}

// packInfoSummary is the JSON shape emitted by `forge pack info --json`.
// Field names are stable, snake_case, and sorted within each list so a
// machine consumer can diff two summaries without spurious churn.
type packInfoSummary struct {
	Name              string         `json:"name"`
	Kind              string         `json:"kind"`
	Version           string         `json:"version"`
	Subpath           string         `json:"subpath,omitempty"`
	Description       string         `json:"description"`
	ProtoFiles        []string       `json:"proto_files"`
	GoFiles           []string       `json:"go_files"`
	GoPackages        []string       `json:"go_packages"`
	OtherFiles        []string       `json:"other_files,omitempty"`
	Dependencies      []string       `json:"dependencies"`
	NPMDependencies   []string       `json:"npm_dependencies,omitempty"`
	Migrations        []string       `json:"migrations,omitempty"`
	GenerateOutputs   []string       `json:"generate_outputs,omitempty"`
	ForgeYAMLConfig   map[string]any `json:"forge_yaml_config,omitempty"`
	ForgeYAMLSection  string         `json:"forge_yaml_section,omitempty"`
	PostInstallHooks  []string       `json:"post_install_hooks"`
	Conflicts         []string       `json:"conflicts,omitempty"`
	AllowedThirdParty []string       `json:"allowed_third_party,omitempty"`
}

// runPackInfo loads the pack manifest, builds a summary, and renders it
// as text or JSON. When invoked outside a project the conflicts list is
// empty (we have nothing to compare against); inside a project it lists
// every pack output that would land on an existing file.
func runPackInfo(name string, asJSON bool) error {
	ctxLabel := fmt.Sprintf("forge pack info %s", name)
	if !packs.ValidPackName(name) {
		return cliutil.UserErr(ctxLabel,
			fmt.Sprintf("invalid pack name %q", name),
			"",
			"run 'forge pack list' to see available packs (names are lowercase letters, digits, hyphens)")
	}
	pack, err := packs.GetPack(name)
	if err != nil {
		return cliutil.UserErr(ctxLabel,
			fmt.Sprintf("pack %q is not available in this forge build", name),
			"",
			"run 'forge pack list' to see installable packs; check the spelling")
	}

	summary := buildPackInfoSummary(pack)

	// Best-effort project-mode enrichment: if there's a forge.yaml in the
	// CWD chain, surface conflicts using a read-only stat against each
	// declared output path. Mirrors `pack install`'s fresh-install
	// collision check shape but never mutates anything.
	if _, err := loadProjectConfig(); err == nil {
		if root, rerr := projectRoot(); rerr == nil {
			summary.Conflicts = collectPackConflicts(pack, root)
		}
	}

	if asJSON {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(summary)
	}
	printPackInfoText(summary)
	return nil
}

// buildPackInfoSummary projects a Pack manifest into the cli-level
// summary type. Categorization rules:
//   - any output ending in `.proto` lands in proto_files
//   - any output ending in `.go` lands in go_files; its parent dir is
//     surfaced in go_packages (deduped + sorted)
//   - everything else lands in other_files
//   - generate hooks are listed separately so a user can see "this runs
//     on every forge generate" vs. "this writes once at install"
func buildPackInfoSummary(p *packs.Pack) packInfoSummary {
	out := packInfoSummary{
		Name:              p.Name,
		Kind:              p.EffectiveKind(),
		Version:           p.Version,
		Subpath:           p.Subpath,
		Description:       strings.TrimSpace(p.Description),
		Dependencies:      append([]string(nil), p.Dependencies...),
		NPMDependencies:   append([]string(nil), p.NPMDependencies...),
		AllowedThirdParty: append([]string(nil), p.AllowedThirdParty...),
		ForgeYAMLSection:  p.Config.Section,
		ForgeYAMLConfig:   p.Config.Defaults,
	}

	goPackages := map[string]struct{}{}
	for _, f := range p.Files {
		switch {
		case strings.HasSuffix(f.Output, ".proto"):
			out.ProtoFiles = append(out.ProtoFiles, f.Output)
		case strings.HasSuffix(f.Output, ".go"):
			out.GoFiles = append(out.GoFiles, f.Output)
			goPackages[filepath.Dir(f.Output)] = struct{}{}
		default:
			out.OtherFiles = append(out.OtherFiles, f.Output)
		}
	}

	for _, m := range p.Migrations {
		desc := m.Name
		if m.Description != "" {
			desc = fmt.Sprintf("%s — %s", m.Name, m.Description)
		}
		out.Migrations = append(out.Migrations, desc)
	}

	for _, g := range p.Generate {
		out.GenerateOutputs = append(out.GenerateOutputs, g.Output)
		if strings.HasSuffix(g.Output, ".go") {
			goPackages[filepath.Dir(g.Output)] = struct{}{}
		}
	}

	for pkg := range goPackages {
		out.GoPackages = append(out.GoPackages, pkg)
	}

	// Hooks are derived from manifest shape, not from the install code
	// path — kept in sync with pack.go's Install/installFrontend so the
	// LLM reading `forge pack info` sees what install will actually do.
	hasNewProto := false
	for _, f := range p.Files {
		if strings.HasSuffix(f.Output, ".proto") {
			hasNewProto = true
			break
		}
	}
	switch out.Kind {
	case packs.PackKindFrontend:
		out.PostInstallHooks = append(out.PostInstallHooks,
			"per-frontend npm install --save (resolves provider_npm_dependencies if --config provider=… is set)")
	default:
		if len(p.Dependencies) > 0 {
			out.PostInstallHooks = append(out.PostInstallHooks,
				fmt.Sprintf("go get for %d dependency(ies)", len(p.Dependencies)))
		}
		if hasNewProto {
			out.PostInstallHooks = append(out.PostInstallHooks,
				"go mod tidy is SKIPPED (pack adds .proto; run `forge generate` to produce gen/ output and tidy)")
		} else {
			out.PostInstallHooks = append(out.PostInstallHooks, "go mod tidy")
		}
	}
	if len(p.Generate) > 0 {
		out.PostInstallHooks = append(out.PostInstallHooks,
			fmt.Sprintf("`forge generate` re-renders %d generate-hook output(s) on every run", len(p.Generate)))
	}

	sort.Strings(out.ProtoFiles)
	sort.Strings(out.GoFiles)
	sort.Strings(out.OtherFiles)
	sort.Strings(out.GoPackages)
	sort.Strings(out.GenerateOutputs)
	sort.Strings(out.Migrations)

	return out
}

// collectPackConflicts returns the relative paths under projectDir that
// already exist and would be touched by `forge pack install`. Mirrors
// the shape of the install-time collision check but is read-only —
// suitable for an info command that should never mutate state.
func collectPackConflicts(p *packs.Pack, projectDir string) []string {
	var conflicts []string
	check := func(rel string) {
		target := filepath.Join(projectDir, rel)
		if _, err := os.Stat(target); err == nil {
			conflicts = append(conflicts, rel)
		}
	}
	for _, f := range p.Files {
		check(f.Output)
	}
	for _, g := range p.Generate {
		check(g.Output)
	}
	sort.Strings(conflicts)
	return conflicts
}

// printPackInfoText renders the summary in a tabular human-readable shape.
// Sections are omitted entirely when empty so trivial packs don't show
// stub headings.
func printPackInfoText(s packInfoSummary) {
	fmt.Printf("Pack: %s (v%s, kind=%s)\n", s.Name, s.Version, s.Kind)
	if s.Subpath != "" {
		fmt.Printf("  pkg/ subpath: %s\n", s.Subpath)
	}
	if s.Description != "" {
		fmt.Println()
		fmt.Println(indentBlock(s.Description, "  "))
	}

	section := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Println()
		fmt.Printf("%s:\n", title)
		for _, it := range items {
			fmt.Printf("  - %s\n", it)
		}
	}

	section("Proto files emitted", s.ProtoFiles)
	section("Go files emitted", s.GoFiles)
	section("Go packages touched", s.GoPackages)
	section("Other files emitted", s.OtherFiles)
	section("Migrations", s.Migrations)
	section("Generate-hook outputs", s.GenerateOutputs)
	section("Go dependencies (go get)", s.Dependencies)
	section("npm dependencies", s.NPMDependencies)

	if s.ForgeYAMLSection != "" || len(s.ForgeYAMLConfig) > 0 {
		fmt.Println()
		fmt.Println("forge.yaml additions:")
		fmt.Printf("  - cfg.Packs += %q\n", s.Name)
		if s.ForgeYAMLSection != "" {
			fmt.Printf("  - pack_config.%s defaults: %v\n", s.ForgeYAMLSection, s.ForgeYAMLConfig)
		}
	}

	section("Post-install hooks", s.PostInstallHooks)
	section("Allowed third-party (frontend lint opt-out)", s.AllowedThirdParty)

	if len(s.Conflicts) > 0 {
		fmt.Println()
		fmt.Println("⚠️  Conflicts in current project (paths already exist):")
		for _, c := range s.Conflicts {
			fmt.Printf("  - %s\n", c)
		}
		fmt.Println("\nFresh installs refuse to clobber non-pack-authored files. Either rename the conflict or re-record the pack in forge.yaml's `packs:` list to enter resync mode.")
	}
}

// indentBlock prefixes every line of in with prefix. Used to inset the
// pack description under the header without a separate template.
func indentBlock(in, prefix string) string {
	var out strings.Builder
	for _, line := range strings.Split(in, "\n") {
		out.WriteString(prefix)
		out.WriteString(line)
		out.WriteByte('\n')
	}
	return strings.TrimRight(out.String(), "\n")
}
