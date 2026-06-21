package pack

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cli/cmdutil"
	"github.com/reliant-labs/forge/internal/cli/factory"
	"github.com/reliant-labs/forge/internal/cliutil"
	"github.com/reliant-labs/forge/internal/packs"
)

// newInfoCmd builds the `forge pack info <name>` subcommand. The
// summary is derived from the embedded pack.yaml manifest and (when the
// command runs inside a project) from the project's existing files so we
// can flag potential collisions before the user runs `pack install`.
//
// This is the LLM-friendly "what does this pack do without me reading
// every template?" surface — it lists proto outputs, Go package outputs,
// migrations, dependencies, generate hooks, conflicts, and the
// forge.yaml additions the install would record.
func newInfoCmd(f *factory.Factory) *cobra.Command {
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
			if err := packsFeatureGate(f); err != nil {
				return err
			}
			return runPackInfo(f, args[0], jsonFlag)
		},
	}
	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Emit machine-readable JSON instead of human-readable text")
	return cmd
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
func runPackInfo(f *factory.Factory, name string, asJSON bool) error {
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
	if _, err := f.LoadProjectStore(); err == nil {
		if root, rerr := cmdutil.ProjectRoot(); rerr == nil {
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
