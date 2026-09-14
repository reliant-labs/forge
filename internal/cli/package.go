package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cli/cmdutil"
	"github.com/reliant-labs/forge/internal/generator"
	"github.com/reliant-labs/forge/internal/generator/contract"
	"github.com/reliant-labs/forge/internal/templates"
)

// validGoPackageName matches a valid Go package name: lowercase letters, digits, underscores.
var validGoPackageName = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)

// validPackageKinds lists the supported --kind values.
var validPackageKinds = map[string]bool{
	"eventbus": true,
	"client":   true,
}

// validPackageTypes lists the supported --type values for `forge scaffold package`.
//
// There are two, because there are only two things forge can tell apart:
//
//   - service: default; the Service/Deps/New(Deps) Service shape that
//     codegen wires. Every internal package is this shape, including the
//     ones you would call orchestrators or use-case interactors — those
//     are services whose deps happen to be other services. Nothing in
//     forge behaves differently for them, so there is no separate flag.
//   - adapter: the same shape, plus the `// forge:outbound-io` marker,
//     which asserts the package calls OUT and serves nothing inbound. The
//     marker is the difference — it is what lint and the observe heuristic
//     read. See `forge skill load adapter`.
var validPackageTypes = map[string]bool{
	"service": true,
	"adapter": true,
}

// packageTypeHelp is the long-form help text shown under `--type`.
const packageTypeHelp = `package shape: service|adapter (default service)

  service     Standard internal/<name>/ with Service/Deps/New — wired into
              the composition, callable by handlers. The default, and the
              right answer for a use-case orchestrator too: an orchestrator
              is a service whose Deps are other services' interfaces.
  adapter     The same shape plus '// forge:outbound-io', which asserts the
              package calls OUT to a third-party system and serves nothing
              inbound. Lint keeps RPC handlers out of it and the observe
              heuristic treats it as doing I/O.
              See: forge skill load adapter`

func newPackageCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "package",
		Short: "Manage internal packages",
		Long: `Manage internal packages with Go interface contracts.

Internal packages live under internal/<name>/ and define their boundary
through a Go interface in contract.go. Unlike proto API services, internal
package contracts use native Go interfaces — supporting channels, complex
types, factories, and other constructs that proto cannot express.

Subcommands:
  forge package new <name>   Create a new internal package`,
	}

	packageNewCmd := &cobra.Command{
		Use:   "new <name>",
		Short: "Create a new internal package with contract interface",
		Long: `Create a new internal package under internal/<name>/ with:
  - contract.go  — Go interface that IS the package contract
  - service.go   — Implementation with unexported concrete type

After creation, define your interface methods in contract.go, then run
'forge generate' to produce mock_gen.go and middleware_gen.go.

` + packageTypeHelp + `

Example:
  forge package new cache
  forge package new notifications
  forge package new events --kind eventbus
  forge package new stripe-adapter --type adapter`,
		Args: cobra.ExactArgs(1),
		RunE: runPackageNew,
	}

	packageNewCmd.Flags().String("kind", "", "package kind template (e.g. eventbus, client)")
	packageNewCmd.Flags().String("type", "service", "package shape: service|adapter (see --help for details)")

	cmd.AddCommand(packageNewCmd)

	return cmdutil.StrictGroup(cmd)
}

func runPackageNew(cmd *cobra.Command, args []string) error {
	name := args[0]

	kind, _ := cmd.Flags().GetString("kind")
	kind = strings.TrimSpace(kind)

	// --type defaults to "service" when the flag isn't registered (e.g.
	// older callers or test commands that don't wire it). Treat empty
	// the same as the default so the call site always gets a valid value.
	pkgType, _ := cmd.Flags().GetString("type")
	pkgType = strings.TrimSpace(pkgType)
	if pkgType == "" {
		pkgType = "service"
	}
	if !validPackageTypes[pkgType] {
		// Sort the names so the error message is stable across runs (map
		// iteration order is unspecified).
		valid := make([]string, 0, len(validPackageTypes))
		for k := range validPackageTypes {
			valid = append(valid, k)
		}
		sort.Strings(valid)
		return fmt.Errorf("invalid package type %q: valid types are %s", pkgType, strings.Join(valid, ", "))
	}

	// --kind and --type compose only on the default "service" type.
	// An adapter gets a fixed scaffold; layering an eventbus/client kind
	// on top would silently overwrite it.
	if pkgType != "service" && kind != "" {
		return fmt.Errorf("--kind cannot be combined with --type=%s; the type owns the scaffold shape", pkgType)
	}

	// Validate --kind if provided
	if kind != "" && !validPackageKinds[kind] {
		valid := make([]string, 0, len(validPackageKinds))
		for k := range validPackageKinds {
			valid = append(valid, k)
		}
		return fmt.Errorf("invalid package kind %q: valid kinds are %s", kind, strings.Join(valid, ", "))
	}

	// Validate name is a valid Go package name
	if !validGoPackageName.MatchString(name) {
		return fmt.Errorf("invalid package name %q: must be lowercase, start with a letter, and contain only letters, digits, and underscores", name)
	}
	if cmdutil.GoKeywords[name] {
		return fmt.Errorf("%q is a Go keyword", name)
	}

	root, err := projectRoot()
	if err != nil {
		return err
	}

	// The directory IS the registry: a package exists because
	// internal/<name>/ holds a contract.go, which is what both the bootstrap
	// codegen and every reporter discover it from.
	pkgDir := filepath.Join(root, "internal", name)
	if dirExists(pkgDir) {
		return fmt.Errorf("internal package %q already exists at %s", name, pkgDir)
	}

	configPath := filepath.Join(root, "forge.yaml")
	cfg, err := generator.ReadProjectConfig(configPath)
	if err != nil {
		return fmt.Errorf("read project config: %w", err)
	}

	fmt.Printf("Creating internal package '%s'...\n", name)

	// Create directory
	if err := os.MkdirAll(pkgDir, 0o755); err != nil {
		return fmt.Errorf("create package directory: %w", err)
	}

	// Template data. ImportPath mirrors Name for now because `forge package
	// new` only accepts flat names (validGoPackageName rejects "/"). When
	// nested-path support lands, ImportPath should carry the full
	// module-relative path under internal/ (e.g. "mcp/database").
	//
	// Flavor identifies the scaffold shape: the --kind when given, else the
	// --type ("service" for the default empty-interface scaffold). The owned
	// observe_chain.go seam is flavor-independent (per-method wrappers live in
	// the GENERATED decorator, not the seam), so Flavor no longer selects
	// observe scaffolding — it still routes the contract/service/test tree.
	flavor := pkgType
	if kind != "" {
		flavor = kind
	}
	data := struct {
		Name       string
		ImportPath string
		Module     string
		Flavor     string
		LogLevel   string
	}{
		Name:       name,
		ImportPath: name,
		Module:     cfg.ModulePath,
		Flavor:     flavor,
		// Seed the owned observe_chain.go seam's success-log level from the
		// project's observability config (default: slog.LevelDebug).
		LogLevel: cfg.Observability.SlogLevelExpr(),
	}

	// Render the scaffold. --type=adapter and --kind both render a full
	// template tree from a dedicated subdir under internal-package/; the
	// default renders the generic contract/service/test set.
	switch {
	case pkgType == "adapter":
		if err := renderPackageKindTree(pkgDir, pkgType, data, true); err != nil {
			return err
		}
	case kind != "":
		if err := renderPackageKindTree(pkgDir, kind, data, false); err != nil {
			return err
		}
	default:
		if err := renderDefaultPackageScaffold(pkgDir, data); err != nil {
			return err
		}
	}

	// EVERY scaffold that produced a contract.go gets its stub mock_gen.go
	// here, not just the default one. When only the default path emitted it,
	// an author who scaffolded an adapter or an eventbus opened the directory,
	// saw no mock, and hand-rolled a fake — which is how a `fakeStore` ends up
	// in the same package as, and adjacent to, a generated `MockStore`. The
	// generator mocks EVERY interface in contract.go, so the stub also covers
	// the dep interfaces declared alongside Service.
	//
	// Soft warning rather than a hard error: the canonical fix for any
	// generator hiccup is to run `forge generate` once the project is
	// quiescent, and a broken stub should not block package creation.
	if contractPath := filepath.Join(pkgDir, "contract.go"); fileExists(contractPath) {
		if err := contract.Generate(contractPath); err != nil {
			fmt.Fprintf(os.Stderr, "warning: could not emit stub mock_gen.go for %s: %v\n", name, err)
			fmt.Fprintln(os.Stderr, "         run `forge generate` to retry.")
		}
	}

	// Every contract package ALSO gets the OWNED observability seam
	// (observe_chain.go: newObserveChain() — the in-process middleware chain
	// the generated decorator routes through). It is the user's file from day
	// 0 and is THE extension point (add/drop a middleware, change the log
	// level). The per-method wrapper itself is GENERATED from the interface
	// (middleware_gen.go) — adding a Service method regenerates it, no
	// hand-maintenance. The composition site wires `pkg.New<Concrete>WithForgeMiddleware(pkg.New(...))`
	// automatically whenever this seam exists and New returns the interface.
	observeContent, err := templates.InternalPkgTemplates().Render("observe_chain.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render observe_chain.go: %w", err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "observe_chain.go"), observeContent, 0o644); err != nil {
		return fmt.Errorf("write observe_chain.go: %w", err)
	}

	// forge.yaml is NOT touched. A package is declared by its own source —
	// internal/<name>/contract.go, plus the `//forge:outbound-io` marker the
	// adapter scaffold stamps — and that is what codegen wires and what
	// `forge project map` / `forge project audit` / the architecture doc
	// report. A `packages:` list next to it could only ever be a copy that
	// goes stale.

	// Report what was ACTUALLY written, by the noun the user asked for.
	//
	// The summary is the most-read documentation forge emits — every
	// invocation ends here, and it is what decides which file the author
	// opens next. It used to announce "Internal package created" for a
	// `--type adapter` request (reading like a silently downgraded ask,
	// since `scaffold package` is a different noun) and name ONE of the six
	// files it wrote — the one that is not where the work is. An author who
	// trusted it edited contract.go and never opened adapter.go, cache.go
	// or observe_chain.go, which is where the scaffold's best guidance
	// lives.
	printScaffoldedPackageSummary(pkgDir, name, pkgType)

	return nil
}

// packageFileRoles describes what each scaffolded file is for, so the
// summary teaches rather than just lists. A file with no entry here is
// still printed — an unexplained file beats an omitted one, and this map
// going stale must not silently hide a new template.
var packageFileRoles = map[string]string{
	"adapter.go":       "Deps + concrete service + New — THE implementation seam, start here",
	"adapter_test.go":  "httptest-backed test, already passing",
	"cache.go":         "empty by design: where the TTL/rate-budget goes when you add one",
	"contract.go":      "the Service interface — the boundary callers depend on",
	"service.go":       "the implementation",
	"contract_test.go": "contract-level test (yours after this scaffold)",
	"mock_gen.go":      "generated: one mock per interface in contract.go",
	"observe_chain.go": "owned observability seam (add/drop middleware, set log level)",
	"client.go":        "HTTP client implementation",
	"eventbus.go":      "event bus implementation",
}

// printScaffoldedPackageSummary lists every file that landed in pkgDir with
// a one-line role, then points at the seam to open next.
//
// The list is read off DISK rather than from a hardcoded set, so a template
// added to a scaffold tree shows up here automatically instead of being
// silently under-reported — the exact failure this replaced.
func printScaffoldedPackageSummary(pkgDir, name, pkgType string) {
	noun := "Internal package"
	if pkgType == "adapter" {
		noun = "Adapter"
	}
	fmt.Printf("\n✅ %s '%s' created!\n", noun, name)

	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		// Non-fatal: the files exist regardless, and a summary that cannot
		// enumerate them should not fail the scaffold.
		fmt.Fprintf(os.Stderr, "warning: could not list %s: %v\n", pkgDir, err)
		return
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, file := range names {
		if role := packageFileRoles[file]; role != "" {
			fmt.Printf("   internal/%s/%-18s %s\n", name, file, role)
		} else {
			fmt.Printf("   internal/%s/%s\n", name, file)
		}
	}

	// The next step differs by shape: an adapter arrives with a working
	// HealthCheck to extend, so the work is in adapter.go; the default
	// package arrives with an empty interface, so the work starts in
	// contract.go.
	if pkgType == "adapter" {
		fmt.Printf("\n   Next: implement the downstream calls in internal/%s/adapter.go, declaring each\n", name)
		fmt.Printf("         one on the Service interface in internal/%s/contract.go as you go, then run\n", name)
		fmt.Printf("         `forge generate` to refresh internal/%s/mock_gen.go — one mock per interface.\n", name)
		return
	}
	fmt.Printf("\n   Next: edit internal/%s/contract.go to declare the Service interface (and any dep interfaces),\n", name)
	fmt.Printf("         then run `forge generate` to refresh internal/%s/mock_gen.go — one mock per interface, ready for tests.\n", name)
}

// renderPackageKindTree renders every template in the internal-package
// kindOrType subdir (used for both --type=adapter and --kind, which share the
// full-tree shape) into pkgDir, stripping the .tmpl suffix. When
// requireNonEmpty is set, an empty template set is a hard error (the adapter
// path — an empty set there is a forge bug).
func renderPackageKindTree(pkgDir, kindOrType string, data any, requireNonEmpty bool) error {
	tmplFiles, err := templates.InternalPkgKindTemplates(kindOrType).ListFlat("")
	if err != nil {
		return fmt.Errorf("list %s templates: %w", kindOrType, err)
	}
	if requireNonEmpty && len(tmplFiles) == 0 {
		return fmt.Errorf("no templates found for --type=%s (this is a forge bug — please report)", kindOrType)
	}
	for _, tmplFile := range tmplFiles {
		content, err := templates.InternalPkgKindTemplates(kindOrType).Render(tmplFile, data)
		if err != nil {
			return fmt.Errorf("render %s: %w", tmplFile, err)
		}
		// Strip .tmpl suffix for the output filename.
		outName := strings.TrimSuffix(tmplFile, ".tmpl")
		if err := os.WriteFile(filepath.Join(pkgDir, outName), content, 0o644); err != nil {
			return fmt.Errorf("write %s: %w", outName, err)
		}
	}
	return nil
}

// renderDefaultPackageScaffold renders the default internal-package set:
// contract.go, service.go, and a once-only contract_test.go (owned by the
// user after the first scaffold). The stub mock_gen.go is emitted by the
// caller, which does it for every scaffold shape rather than just this one.
func renderDefaultPackageScaffold(pkgDir string, data any) error {
	contractContent, err := templates.InternalPkgTemplates().Render("contract.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render contract.go: %w", err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "contract.go"), contractContent, 0o644); err != nil {
		return fmt.Errorf("write contract.go: %w", err)
	}

	serviceContent, err := templates.InternalPkgTemplates().Render("service.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render service.go: %w", err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "service.go"), serviceContent, 0o644); err != nil {
		return fmt.Errorf("write service.go: %w", err)
	}

	contractTestContent, err := templates.InternalPkgTemplates().Render("contract_test.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render contract_test.go: %w", err)
	}
	if err := os.WriteFile(filepath.Join(pkgDir, "contract_test.go"), contractTestContent, 0o644); err != nil {
		return fmt.Errorf("write contract_test.go: %w", err)
	}

	return nil
}
