// Package scaffold holds the `forge scaffold` command — the one verb that
// writes new code into a forge project, at two granularities picked by ARITY:
//
//	forge scaffold                    everything the protos imply (the sweep;
//	                                  see sweep.go)
//	forge scaffold <noun> [args...]   one thing, explicitly (service / worker /
//	                                  operator / binary / frontend / webhook /
//	                                  package / adapter / library /
//	                                  handler-file / rpc / entity / crd /
//	                                  scenario)
//
// Both modes run the same primitives: `forge scaffold entity product
// --from-proto tasks` is the explicit spelling of one birth the sweep would
// perform from a `// forge:entity` marker. The nouns beyond entity (service,
// worker, …) have no marker to sweep from, so they are only reachable
// explicitly.
//
// It is a dir-nested command group (the devspace idiom forge ships in
// generated apps). The subcommands that trigger codegen — `scaffold service`,
// `scaffold worker`, `scaffold operator` — reach the generate pipeline and the
// pkg/app/services.go registration view through factory.GenAPI (function
// values internal/cli registers via SetGenAPI). The group never imports
// internal/cli, so the registry indirection in the factory package is what
// keeps the dependency one-directional (group → factory) and the import
// cycle broken. init() (in sweep.go) self-registers the command with the
// factory.
package scaffold

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/robfig/cron/v3"
	"github.com/spf13/cobra"

	"github.com/reliant-labs/forge/internal/cli/cmdutil"
	"github.com/reliant-labs/forge/internal/cli/factory"
	"github.com/reliant-labs/forge/internal/cliutil"
	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/config"
	"github.com/reliant-labs/forge/internal/generator"
	"github.com/reliant-labs/forge/internal/naming"
)

// validateServiceName / validateIdentifier / validateProjectName /
// validateFrontendName forward to the shared validators in cmdutil (the
// leaf home shared with `forge project new`). Unexported aliases keep the group's
// call sites terse and behavior identical.
func validateServiceName(name string) error  { return cmdutil.ValidateServiceName(name) }
func validateIdentifier(name string) error   { return cmdutil.ValidateIdentifier(name) }
func validateProjectName(name string) error  { return cmdutil.ValidateProjectName(name) }
func validateFrontendName(name string) error { return cmdutil.ValidateFrontendName(name) }

// projectRoot forwards to cmdutil.ProjectRoot — the shared project-root
// resolver. The unexported alias keeps the many call sites in this package
// unchanged from their pre-move form.
func projectRoot() (string, error) { return cmdutil.ProjectRoot() }

// requireNoComponentNamed enforces that no component in the discovered
// inventory already claims `name`, returning a user-facing UserErr (with a Fix
// clause) when one does. Components share a single name namespace —
// server/worker/cron/operator/binary all collide on name — so every
// `forge scaffold <component>` path funnels its conflict check through here to get a
// uniform message that also names the existing component's kind. `ctxLabel` is
// the caller's "forge scaffold <kind> <name>" boundary label.
//
// The comparison is on the CANONICAL Go-package form, not the raw string:
// "admin-server" and "admin_server" both normalize to "admin_server", so they
// would scaffold into the same directory and register the same symbol. The
// second one has to be rejected here, at the point the name is chosen, or the
// user meets it later as a confusing codegen collision.
func requireNoComponentNamed(inv codegen.Inventory, name, ctxLabel string) error {
	canonical := naming.ServicePackage(name)
	for _, comp := range inv {
		if comp.Name != name && naming.ServicePackage(comp.Name) != canonical {
			continue
		}
		detail := fmt.Sprintf("%q already exists in the project as a %s", name, comp.EffectiveKind())
		if comp.Name != name {
			detail = fmt.Sprintf("%q collides with the existing %s %q (both normalise to %q)",
				name, comp.EffectiveKind(), comp.Name, canonical)
		}
		return cliutil.UserErr(ctxLabel, detail, "",
			"pick a different name, or remove the existing component first")
	}
	return nil
}

// addNounCmds attaches every explicit-noun subcommand to the `forge scaffold`
// command built in sweep.go. Keeping the list here — next to the per-noun
// implementations — means adding a noun is a one-line edit in the file that
// already owns it.
func addNounCmds(cmd *cobra.Command, f *factory.Factory) {
	cmd.AddCommand(newServiceCmd(f))
	cmd.AddCommand(newWorkerCmd(f))
	cmd.AddCommand(newOperatorCmd(f))
	cmd.AddCommand(newCRDCmd(f))
	cmd.AddCommand(newFrontendCmd(f))
	cmd.AddCommand(newScenarioCmd(f))
	cmd.AddCommand(newWebhookCmd(f))
	cmd.AddCommand(newPackageCmd(f))
	cmd.AddCommand(newAdapterCmd(f))
	cmd.AddCommand(newBinaryCmd(f))
	cmd.AddCommand(newLibraryCmd(f))
	cmd.AddCommand(newHandlerFileCmd(f))
	cmd.AddCommand(newRPCCmd(f))
	cmd.AddCommand(newEntityCmd(f))
}

// requireServiceKind reads forge.yaml at root and returns an error if the
// project's kind is not "service". `forge scaffold service/operator/worker/webhook`
// only makes sense for server-shaped projects — CLI and library kinds have
// no Connect-RPC server to attach handlers to.
func requireServiceKind(root, action string) error {
	configPath := filepath.Join(root, "forge.yaml")
	cfg, err := generator.ReadProjectConfig(configPath)
	if err != nil {
		return cliutil.WrapUserErr(fmt.Sprintf("forge scaffold %s", action),
			"read project config", configPath,
			"verify forge.yaml is valid YAML", err)
	}
	if !cfg.IsServiceKind() {
		return cliutil.UserErr(fmt.Sprintf("forge scaffold %s", action),
			fmt.Sprintf("'forge scaffold %s' is only available for service projects (this project's kind: %s)",
				action, cfg.EffectiveKind()),
			"",
			"re-run 'forge project new' with --kind service to scaffold a server, or use 'forge scaffold package' for internal Go packages")
	}
	return nil
}

// componentSpec captures the kind-specific knobs of a single
// `forge scaffold <kind> <name>` invocation. scaffoldComponent owns the spine every
// component-appending path repeats — validate-name, projectRoot,
// requireServiceKind, ReadProjectConfig, conflict check, scaffold files, then
// the post-scaffold generate step — and calls back into the spec at the points
// where the paths
// genuinely differ. service/worker/operator/binary each build a spec and call
// scaffoldComponent; their per-kind preamble (resume/force, feature gate, reserved
// cobra names) and tail (pipeline vs none, success print) ride the hooks.
//
// The spine is stateful and behavior-preserving: the scaffold e2e tests are
// the guardrail. The hooks fire in the same order the inline code used to run,
// so on-disk + stdout effects are unchanged.
type componentSpec struct {
	// name is the component name as the user typed it.
	name string
	// ctxLabel is the "forge scaffold <kind> <name>" error boundary label.
	ctxLabel string

	// validate checks name validity and returns a UserErr-wrapped error.
	// Each kind owns its validator + invalid-name hint (service rejects
	// reserved worker/scheduler names; webhook/frontend use looser rules).
	validate func(name string) error

	// preflight runs after the project read but before the conflict check.
	// It is the hook for kind-specific gates that need project state: the
	// operator feature check and the service resume/force port resolution.
	// cfg is the freshly-read config, inv the freshly-discovered component
	// inventory, root the project root. May print. A nil preflight is a no-op.
	preflight func(cfg *config.ProjectConfig, inv codegen.Inventory, root string) error

	// checkConflict enforces name uniqueness against the discovered
	// inventory. Most kinds delegate to requireNoComponentNamed; service
	// overrides this to tolerate an existing component under
	// --resume/--force. A nil checkConflict falls back to
	// requireNoComponentNamed.
	checkConflict func(inv codegen.Inventory, name, ctxLabel string) error

	// announce prints the "Adding <kind> '<name>'..." line. Each kind owns
	// its exact wording (cron schedule, operator group/version, …).
	announce func(cfg *config.ProjectConfig)

	// scaffold writes the kind's scaffold files (service.go, worker.go,
	// operator skeleton, binary glue). root + cfg give it everything it
	// needs; it returns the generator error verbatim so callers keep their
	// "generate <kind> files: %w" wrapping.
	scaffold func(cfg *config.ProjectConfig, root string) error

	// postScaffold runs after the scaffold files are written. It owns the
	// post-scaffold generate step and the success print, which diverge the
	// most across kinds (full pipeline + E2E for service; no-generate branch
	// + bootstrap-only for worker; no pipeline for binary).
	postScaffold func(p postScaffoldParams) error

	// deferWorkloadDecl suppresses the deploy/kcl/workloads.k append at the
	// end of this run, handing the caller responsibility for it.
	//
	// Set by the BATCH path only. The declaration must happen after the
	// generate pipeline, because it re-reads the inventory and a service is
	// declared by the compiled proto descriptor the pipeline produces. In a
	// batch the pipeline runs once, after every name has been scaffolded, so
	// no per-name run is in a position to make the append — the batch makes
	// them all, in order, once the descriptor is current.
	deferWorkloadDecl bool
}

// postScaffoldParams bundles the state scaffoldComponent threads into the
// postScaffold hook so each kind can run its own generate-and-print tail.
type postScaffoldParams struct {
	cfg  *config.ProjectConfig
	root string
	name string
}

// scaffoldComponent owns the spine shared by every component-appending
// `forge scaffold <kind>` path. Each run* builds a componentSpec and calls
// here; the spec's hooks fill in the kind-specific behavior. The ordering and
// side effects are byte-for-byte the same as the inline implementations the
// hooks were lifted from.
func scaffoldComponent(spec componentSpec) error {
	if err := spec.validate(spec.name); err != nil {
		return err
	}

	root, err := projectRoot()
	if err != nil {
		return err
	}
	// requireServiceKind's action label is the verb between "forge scaffold " and
	// " <name>" in ctxLabel.
	action := strings.TrimSuffix(strings.TrimPrefix(spec.ctxLabel, "forge scaffold "), " "+spec.name)
	if err := requireServiceKind(root, action); err != nil {
		return err
	}

	cfg, inv, err := readProject(root, spec.ctxLabel)
	if err != nil {
		return err
	}

	if spec.preflight != nil {
		if err := spec.preflight(cfg, inv, root); err != nil {
			return err
		}
	}

	checkConflict := spec.checkConflict
	if checkConflict == nil {
		checkConflict = requireNoComponentNamed
	}
	if err := checkConflict(inv, spec.name, spec.ctxLabel); err != nil {
		return err
	}

	if spec.announce != nil {
		spec.announce(cfg)
	}

	if err := spec.scaffold(cfg, root); err != nil {
		return err
	}

	// Nothing is written back to forge.yaml: the scaffold step just wrote the
	// component's declaration (its proto, or its package under
	// internal/workers/ | internal/operators/ | cmd/), which is what the next
	// read discovers. The post-scaffold generate step turns those sources
	// into wiring.

	if err := spec.postScaffold(postScaffoldParams{
		cfg:  cfg,
		root: root,
		name: spec.name,
	}); err != nil {
		return err
	}

	// Declare the new workload in the project's deploy/kcl/workloads.k.
	// This is the ONE write forge makes to that user-owned file after it is
	// born, and it is a pure append — nothing above the insertion point is
	// touched. When the file has been restructured such that there is no
	// unambiguous place to insert, the stanza is PRINTED instead of guessed.
	//
	// AFTER postScaffold, deliberately: a service is declared by the proto
	// descriptor, which the post-scaffold generate step is what produces.
	// Re-reading the inventory here means the stanza carries what discovery
	// actually found — a cron's schedule, an operator's group/version/CRDs —
	// rather than a kind guessed from the subcommand name.
	if !spec.deferWorkloadDecl {
		declareWorkloadInKCL(root, cfg, spec)
	}
	return nil
}

// runPostScaffoldGenerate runs the worker-style post-scaffold generate step:
// a --no-generate short-circuit, then the bootstrap-only pipeline preset with
// the partial-failure messaging. It is the shared tail for component kinds that
// expose a --no-generate flag (currently worker). The exact stdout/stderr
// wording matches the inline runWorker code it was lifted from — friction
// fixes (the "files written, generate failed" partial-success line) live here.
func runPostScaffoldGenerate(f *factory.Factory, root, name string, noGenerate bool) error {
	// --no-generate: scaffold-only mode. Skip the post-scaffold generate
	// pipeline so parallel-agent rounds can stage worker scaffolding
	// without triggering project-wide codegen churn (which races sibling
	// agents holding the api-service / mock_gen / wire_gen lanes). The
	// operator is responsible for running `forge generate` at a
	// coordination point. See friction
	// forge-add-worker-runs-full-pipeline (kalshi-trader migration
	// round, filed-not-fixed entry: "no --no-generate / --steps=worker
	// flag, so the worker-add path always runs the full codegen
	// pipeline").
	if noGenerate {
		fmt.Printf("\n⏩ --no-generate: skipping post-scaffold `forge generate` run.\n")
		fmt.Printf("    Worker '%s' scaffolded. Run `forge generate` at a coordination point\n", name)
		fmt.Printf("    to update pkg/app/{bootstrap,wire_gen,testing}.go for the new worker.\n")
		fmt.Printf("\n✅ Worker '%s' scaffold-only mode complete!\n", name)
		return nil
	}

	// Run the generation pipeline, narrowed to the bootstrap-only step
	// preset, so adding a worker regenerates
	// pkg/app/{bootstrap,testing,migrate}.go and nothing else. The full
	// pipeline would also rewrite every Tier-1 file in its catalog
	// (.github/workflows/ci.yml, cmd/server.go, frontend mocks,
	// pkg/config/config.go) — friction reported by the cp-forge
	// port-workers round where `forge scaffold worker` × 7 rewrote 5
	// unrelated Tier-1 files per invocation. The step preset's allowed
	// step set lives in stepPresetAllowlist["bootstrap-only"]
	// (generate_pipeline.go).
	fmt.Println("\n🔧 Running generation pipeline (bootstrap-only step preset)...")
	err := f.Gen.RunPipelineBootstrapOnly(root)
	if err != nil {
		// Non-fatal: the worker files were created successfully, but the
		// pipeline failure usually means the project doesn't compile (a
		// sibling-package issue or a stale generated file). Print a
		// distinct partial-success line so a user skimming the output
		// doesn't see the unconditional ✅ below and assume the build is
		// healthy. Friction reported by the kalshi-trader migration round:
		// the prior code printed the green check directly after the
		// "warning: generation pipeline failed" line, hiding the failure
		// in the visual noise.
		fmt.Fprintf(os.Stderr, "\nwarning: generation pipeline failed: %v\n", err)
		fmt.Printf("\n⚠️  Worker '%s' files written, but `forge generate` failed — fix the build before running it again.\n", name)
		fmt.Printf("    Tip: pass --no-generate to `forge scaffold worker` to suppress the post-scaffold pipeline run.\n")
		return nil
	}

	fmt.Printf("\n✅ Worker '%s' added successfully!\n", name)
	return nil
}

// binaryName resolves the primary binary (the cmd/<bin>/ leaf) for wiring
// output — the forge.yaml project name, falling back to the project dir base.
func binaryName(cfg *config.ProjectConfig, root string) string {
	if cfg != nil && cfg.Name != "" {
		return cfg.Name
	}
	return filepath.Base(root)
}

// wireWorkerIntoTree scaffolds the scaffold-once per-worker subcommand file
// (cmd/<bin>/cmd/workers/<name>.go) and reports what is left for the dev.
//
// The compose.go field/construction and the lifecycle.go Worker<X>() accessor
// the subcommand calls are NOT hand-work: `forge generate` discovers the
// worker from internal/workers/<pkg>/ and reconciles both owned files
// additively (see codegen.GenerateCompose / GenerateLifecycle). The command
// TREE is still owned — main.go names every group constructor explicitly — so
// that one line is printed.
func wireWorkerIntoTree(cfg *config.ProjectConfig, root, name string) {
	wireComponentIntoTree(cfg, root, name, componentCmdWiring{
		group:    "workers",
		scaffold: codegen.ScaffoldWorkerCmd,
	})
}

// wireServiceIntoTree adds the freshly added service's constructor to
// cmd/<bin>/main.go, and prints the manual instruction only when it cannot.
//
// `forge generate` emits the service's cobra subcommand
// (cmd/<bin>/cmd/services/<name>.go) — but the composition root decides which
// commands this binary SHIPS. Without the constructor reference the subcommand
// is generated and unreachable: `<bin> <service>` does not exist and
// `<bin> --help` never lists it, silently.
//
// main.go stays OWNED code: this is a one-argument append that leaves the rest
// of the file alone, and an Execute call forge does not recognize is declined
// rather than guessed at (see codegen.WireComponentIntoMain for the full
// reasoning). Already-wired is a no-op, so a --resume/--force re-run stays
// quiet. Reserved names (the command tree's own server/version/db/…) emit no
// subcommand at all, so there is nothing to wire.
func wireServiceIntoTree(cfg *config.ProjectConfig, root, name string) {
	runtime, ctor, emitted := codegen.CmdServiceCommand(name)
	if !emitted {
		return
	}
	bin := binaryName(cfg, root)
	if _, err := os.Stat(filepath.Join(root, "cmd", bin, "cmd", "services", runtime+".go")); err != nil {
		return // no subcommand on disk (codegen skipped or failed) — nothing to wire
	}
	module := ""
	if cfg != nil {
		module = cfg.ModulePath
	}

	outcome, err := codegen.WireComponentIntoMain(root, bin, module, "services", ctor)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not update cmd/%s/main.go: %v\n", bin, err)
	}
	switch outcome {
	case codegen.WireAlreadyWired:
		return
	case codegen.WireApplied:
		fmt.Printf("  ✅ wired services.%s into cmd/%s/main.go — `%s %s` is live\n", ctor, bin, bin, runtime)
		return
	}

	// Unrecognized: the user has made main.go their own in a shape forge
	// cannot append to. Print exactly what to add, as before.
	fmt.Printf(`
🔧 One owned file left to wire — forge could not find the cmd.Execute(...) call to append to:

  cmd/%[1]s/main.go — add to the cmd.Execute(...) arg list (+ the services import):
      import "%[2]s/cmd/%[1]s/cmd/services"
      services.%[3]s,

  Until then `+"`%[1]s %[4]s`"+` does not exist and `+"`%[1]s --help`"+` will not list it.
  (`+"`%[1]s server`"+` still serves %[4]s — it mounts every service.)
`, bin, module, ctor, runtime)
}

// wireOperatorIntoTree is the operator-side analog of wireWorkerIntoTree.
func wireOperatorIntoTree(cfg *config.ProjectConfig, root, name string) {
	wireComponentIntoTree(cfg, root, name, componentCmdWiring{
		group:    "operators",
		scaffold: codegen.ScaffoldOperatorCmd,
	})
}

// componentCmdWiring is the per-kind half of wireComponentIntoTree: the
// cmd/<bin>/cmd/<group>/ directory the subcommand lands in and the scaffolder
// that writes it.
type componentCmdWiring struct {
	group    string
	scaffold func(targetDir, bin, name string) (bool, error)
}

func wireComponentIntoTree(cfg *config.ProjectConfig, root, name string, w componentCmdWiring) {
	bin := binaryName(cfg, root)
	field := naming.ToPascalCase(name)
	module := cfg.ModulePath

	wrote, err := w.scaffold(root, bin, name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not scaffold %s subcommand file: %v\n", w.group, err)
	} else if wrote {
		fmt.Printf("  ✅ scaffolded cmd/%s/cmd/%s/%s.go (owned)\n", bin, w.group, name)
	} else {
		fmt.Printf("  • cmd/%s/cmd/%s/%s.go already exists (left untouched)\n", bin, w.group, name)
	}

	ctor := "New" + field + "Cmd"
	outcome, werr := codegen.WireComponentIntoMain(root, bin, module, w.group, ctor)
	if werr != nil {
		fmt.Fprintf(os.Stderr, "warning: could not update cmd/%s/main.go: %v\n", bin, werr)
	}
	switch outcome {
	case codegen.WireAlreadyWired:
		// Nothing to say: a re-run over an already-wired component.
	case codegen.WireApplied:
		fmt.Printf("  ✅ wired %s.%s into cmd/%s/main.go — `%s %s` is live\n", w.group, ctor, bin, bin, name)
	default:
		fmt.Printf(`
🔧 One owned file left to wire — forge could not find the cmd.Execute(...) call to append to:

  cmd/%[1]s/main.go — add to the cmd.Execute(...) arg list (+ the %[2]s import):
      import "%[3]s/cmd/%[1]s/cmd/%[2]s"
      %[2]s.New%[4]sCmd,
`, bin, w.group, module, field)
	}

	fmt.Printf("  • internal/app/{compose,lifecycle}.go are handled by `forge generate`: it discovers\n" +
		"    the component from its package directory and reconciles the field, the\n" +
		"    construction, and the typed accessor into those owned files for you.\n")
}

// --- scaffold service ---

func newServiceCmd(f *factory.Factory) *cobra.Command {
	var (
		resume bool
		force  bool
	)

	cmd := &cobra.Command{
		Use:   "service <name>...",
		Short: "Scaffold one or more Go services",
		Long: `Scaffold one or more Go services into an existing Forge project.

This creates each service's directory structure, proto file, Dockerfile,
hot-reload config, and updates the project configuration.

Pass several names to scaffold them as ONE batch. The generate pipeline
is the dominant cost of this command and it derives everything it emits
from the tree, so a batch writes every service's sources and then
projects them once — four services in one call cost one pipeline run
instead of four.

There is no per-service port: every service in a binary mounts onto the
SAME Connect mux, and the process listens once on the AppConfig 'port'
field (env var PORT, default 8080). Change it per environment in
deploy/kcl/<env>/config.k, or change the default in
proto/config/v1/config.proto.

Flags:
  --resume   Re-run a partial scaffold. Skips every output file that
             already exists on disk. Safe to invoke repeatedly.
  --force    Re-stamp the scaffold even when files exist. Overwrites
             service.go, the test files, and the proto stub. Use after
             manually editing a scaffolded file and wanting to start over.

--resume and --force are mutually exclusive, and apply to every name in
the batch.

Example:
  forge scaffold service users
  forge scaffold service customers sales jobs billing   # one pipeline run
  forge scaffold service users --resume   # recover from a partial failure
  forge scaffold service users --force    # re-stamp every output file`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runServices(f, args, resume, force)
		},
	}

	cmd.Flags().BoolVar(&resume, "resume", false, "Resume a partial scaffold: skip files that already exist")
	cmd.Flags().BoolVar(&force, "force", false, "Force-overwrite every scaffold output file")

	return cmd
}

// runServices scaffolds a BATCH of services, paying the generate pipeline
// once for the whole set.
//
// The pipeline is the dominant cost of `forge scaffold service` — buf
// generate, the ORM/CRUD projection, mocks, wiring, `go mod tidy`, `go build`
// — and it is a pure function of the tree: it discovers services from the
// proto descriptor rather than from anything the verb records, and re-running
// it writes byte-identical output. So scaffolding N services one at a time
// pays for N projections of which the first N-1 are immediately superseded by
// the next. Measured on a four-service round, that was four full pipeline runs
// where one was correct.
//
// The batch is therefore: validate and conflict-check every name FIRST (a
// typo must not cost three scaffolds before it is caught), write every
// service's sources with the pipeline suppressed, run the pipeline once, then
// perform each name's post-pipeline tail (E2E harness, command-tree wiring,
// registry notice, workloads.k declaration) in the order the user named them.
//
// A single-name call goes through exactly this path — there is one code path,
// not a fast path and a batch path that can drift apart.
func runServices(f *factory.Factory, names []string, resume, force bool) error {
	ctxLabel := "forge scaffold service"
	if len(names) == 0 {
		return cliutil.UserErr(ctxLabel, "no service name given", "",
			"pass one or more names, e.g. `forge scaffold service customers sales`")
	}

	// Reject a repeated name before anything is written. Two identical names
	// would scaffold the same dirs twice and then collide in codegen, and the
	// only moment that is cheap to say is before the first write. Compare on
	// the CANONICAL package form, so "admin-server" and "admin_server" — which
	// scaffold into the same directory — are caught too.
	seen := map[string]string{}
	for _, name := range names {
		canonical := naming.ServicePackage(name)
		if first, dup := seen[canonical]; dup {
			return cliutil.UserErr(ctxLabel,
				fmt.Sprintf("service %q is named twice in this batch (as %q and %q)", canonical, first, name),
				"", "name each service once — they share one directory and one Go package")
		}
		seen[canonical] = name
	}

	if len(names) > 1 {
		fmt.Printf("Scaffolding %d services as one batch (the generate pipeline runs once at the end)...\n\n", len(names))
	}

	batch := &serviceBatch{total: len(names)}
	for i, name := range names {
		batch.index = i
		if err := runService(f, name, resume, force, batch); err != nil {
			return err
		}
	}
	return nil
}

// serviceBatch threads the batch's shape through the per-name scaffold so each
// run knows whether it is the one that pays for the pipeline, and so the
// deferred post-pipeline tails can be collected and replayed at the end.
type serviceBatch struct {
	// total is the number of names in the batch; index is the current one.
	total int
	index int
	// tails are the post-pipeline steps each name deferred, replayed in
	// naming order once the pipeline has run.
	tails []func() error
	// rollbacks undo each name's scaffold, for a pipeline failure that
	// invalidates the whole batch.
	rollbacks []func()
}

// isLast reports whether the current name is the one that runs the pipeline.
func (b *serviceBatch) isLast() bool { return b.index == b.total-1 }

// rollbackAll rewinds every fresh service in the batch, in reverse order.
func (b *serviceBatch) rollbackAll() {
	for i := len(b.rollbacks) - 1; i >= 0; i-- {
		b.rollbacks[i]()
	}
}

// runTails replays each name's post-pipeline tail in naming order.
//
// A tail that fails does NOT abort the rest: by this point every service is
// written and the pipeline has validated them, so a failure here is one
// service's finishing touch (an E2E harness, a wiring append) and not a reason
// to leave the other three unfinished. The first error is returned once every
// tail has had its turn.
func (b *serviceBatch) runTails() error {
	var firstErr error
	for _, tail := range b.tails {
		if err := tail(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// runService scaffolds ONE service. batch is never nil — a single-service
// invocation is a batch of one — and carries whether this name is the one that
// pays for the shared generate pipeline.
func runService(f *factory.Factory, name string, resume, force bool, batch *serviceBatch) error {
	ctxLabel := fmt.Sprintf("forge scaffold service %s", name)

	// --resume and --force are mutually exclusive: one is "preserve user
	// edits", the other is "discard them". Combining them has no coherent
	// meaning, so reject early before we touch any files.
	if resume && force {
		return cliutil.UserErr(ctxLabel,
			"--resume and --force are mutually exclusive",
			"",
			"use --resume to recover from a partial failure (skips existing files), or --force to re-stamp every output file")
	}

	// exists records whether the project already declares a service under this
	// name (the --resume/--force partial-scaffold case). Resolved once in
	// preflight from the discovered inventory and read by checkConflict and
	// postScaffold, which uses it to decide whether a failed pipeline should
	// roll the scaffold back.
	exists := false

	// Rollback tracking for a FRESH add: if the post-scaffold generate
	// pipeline (buf validate / lint / build) fails, the
	// files the scaffold just wrote (the handler dir + proto/services/<pkg>)
	// must be removed so the tree isn't left half-created. Without this a
	// failed validation strands an orphan proto dir and handler dir that
	// confuse the next `forge scaffold service`. We only remove dirs this run
	// CREATED — a pre-existing dir (resume/force, or a re-run over a manual
	// edit) is never blown away.
	servicePkg := naming.ServicePackage(name)
	var (
		handlerDirPreexisted bool
		protoDirPreexisted   bool
	)
	return scaffoldComponent(componentSpec{
		name:     name,
		ctxLabel: ctxLabel,
		// The service path owns its own workloads.k declaration, from
		// finishService — the tail that runs after the shared pipeline has
		// compiled the descriptor discovery reads. The spine's own append
		// would fire mid-batch, before the descriptor knows the service.
		deferWorkloadDecl: true,
		validate: func(name string) error {
			if err := validateServiceName(name); err != nil {
				return cliutil.WrapUserErr(ctxLabel, "invalid service name", "",
					"use a name starting with a letter, containing letters/digits/_/-; not a Go keyword or reserved (worker/scheduler/cron/job)",
					err)
			}
			return nil
		},
		preflight: func(cfg *config.ProjectConfig, inv codegen.Inventory, root string) error {
			// checkConflict runs after preflight and owns the hard-error path;
			// this resolves the same lookup once, for the conflict check and
			// for the rollback decision in postScaffold.
			_, exists = inv.Named(name)
			// Snapshot on-disk pre-existence for rollback (see rollback vars
			// above). Captured here in preflight — the only hook with root in
			// hand that runs BEFORE scaffold writes anything.
			if _, err := os.Stat(filepath.Join(root, "internal", "handlers", servicePkg)); err == nil {
				handlerDirPreexisted = true
			}
			if _, err := os.Stat(filepath.Join(root, "proto", "services", servicePkg)); err == nil {
				protoDirPreexisted = true
			}
			return nil
		},
		checkConflict: func(inv codegen.Inventory, name, ctxLabel string) error {
			// Under --resume or --force we treat a matching name as "this is
			// the partial scaffold I am recovering / re-stamping", not as a
			// hard error. `exists` was resolved in preflight.
			if exists && !resume && !force {
				return cliutil.UserErr(ctxLabel,
					fmt.Sprintf("service %q already exists in the project", name),
					"",
					"pass --resume to skip files that already exist, --force to overwrite them, or pick a different name")
			}
			return nil
		},
		announce: func(cfg *config.ProjectConfig) {
			switch {
			case resume:
				fmt.Printf("Resuming service '%s'...\n", name)
			case force:
				fmt.Printf("Force-stamping service '%s'...\n", name)
			default:
				fmt.Printf("Adding service '%s'...\n", name)
			}
		},
		scaffold: func(cfg *config.ProjectConfig, root string) error {
			// Generate service files (service.go, one rpc_<name>.go per
			// custom RPC, proto). The
			// mode drives per-file overwrite/skip behavior; progress writes
			// "✓ skipped" and "⚠ overwriting" lines to stdout as it goes.
			mode := generator.ScaffoldFail
			switch {
			case resume:
				mode = generator.ScaffoldResume
			case force:
				mode = generator.ScaffoldForce
			}
			if err := generator.GenerateServiceFilesWithMode(root, cfg.ModulePath, name, cfg.Name, mode, os.Stdout); err != nil {
				return fmt.Errorf("generate service files: %w", err)
			}
			return nil
		},
		postScaffold: func(p postScaffoldParams) error {
			// Record this name's rollback with the batch so a failure on the
			// SHARED pipeline rewinds every fresh service, not just the one
			// that happened to be running when it failed. The pipeline
			// validates the batch as a whole: if it fails, none of these
			// services is known-good, and leaving the earlier ones behind
			// would strand exactly the orphan handler/proto dirs the
			// single-service rollback exists to prevent. Only dirs THIS run
			// created are ever removed — a pre-existing dir (resume/force, or
			// a re-run over a manual edit) is preserved.
			batch.rollbacks = append(batch.rollbacks, func() {
				if !exists && !resume && !force {
					rollbackServiceScaffold(p.root, servicePkg, handlerDirPreexisted, protoDirPreexisted)
				}
			})

			// The post-pipeline tail — everything that reads what the pipeline
			// PRODUCED (the E2E harness off the compiled stub, the command-tree
			// wiring off the generated subcommand, the registry check). Held as
			// a closure so the batch replays every name's tail after the single
			// shared pipeline run, in the order the user named them.
			batch.tails = append(batch.tails, func() error {
				return finishService(f, p, name)
			})

			// Only the LAST name pays for the pipeline; earlier names have
			// written their sources and stop here. See runServices for why the
			// pipeline is batchable at all.
			if !batch.isLast() {
				fmt.Printf("  ✓ sources written — pipeline deferred to the end of the batch\n")
				return nil
			}

			// The full generation pipeline: buf generate, service stubs,
			// mocks, bootstrap.go, testing.go, go mod tidy, etc. ONCE for the
			// whole batch.
			fmt.Println("\n🔧 Running generation pipeline...")
			if err := f.Gen.RunPipeline(p.root); err != nil {
				batch.rollbackAll()
				return fmt.Errorf("generation pipeline failed for service %q: %w", name, err)
			}
			return batch.runTails()
		},
	})
}

// finishService is one service's post-pipeline tail: the steps that read what
// the generate pipeline produced. Split out of the postScaffold hook so a
// batch can defer it past the single shared pipeline run instead of
// interleaving it with the scaffolding of the next name.
func finishService(f *factory.Factory, p postScaffoldParams, name string) error {
	// Generate E2E test harness. GenerateE2ETests already skips existing
	// files unconditionally, which gives the right behavior for both
	// --resume and default. --force is not threaded through here because the
	// E2E harness is the user's tests and clobbering them is rarely what
	// someone re-stamping a scaffold actually wants.
	fmt.Println("Generating E2E test harness...")
	e2eMethods := generator.MethodsFromProtoStub(name)
	if err := generator.GenerateE2ETests(p.root, name, p.cfg.ModulePath, p.cfg.Name, e2eMethods); err != nil {
		fmt.Printf("  warning: failed to generate E2E tests: %v\n", err)
		// Non-fatal: the service was created successfully
	}

	fmt.Printf("\n✅ Service '%s' added successfully!\n", name)

	// The command tree is OWNED code — forge generated the service's
	// subcommand but only the composition root decides what this binary
	// ships. Append the constructor (no-op when already wired; prints the
	// manual line when main.go is not in a shape forge recognizes).
	wireServiceIntoTree(p.cfg, p.root, name)

	// Registration: pkg/app/services.go is user-owned — forge never edits it.
	// When the file predates this service (the usual add-flow: the registry
	// was scaffolded with the project's earlier services), the new row
	// constructor is generated but unreferenced, so the binary won't serve
	// the service until the user adds the line. Print the exact line;
	// `forge project audit` keeps the gap visible
	// (codegen.unregistered_services) until it's resolved. This is
	// deliberate: the registration line is the one decision the user (or
	// their agent) writes — forge generates the guardrails around it.
	if reg, regErr := f.Gen.LoadServiceRegistry(p.root); regErr == nil && reg.Exists() && !reg.Registered(name) {
		fmt.Println()
		fmt.Printf("⚠️  %s is user-owned — forge does not edit it.\n", f.Gen.ServiceRegistryRelPath)
		fmt.Printf("   To serve %q from this binary, add this row to RegisteredServices:\n\n", name)
		fmt.Printf("       %s(app, cfg, logger, opts...),\n\n", codegen.ServiceRowFuncName(name))
		fmt.Println("   Until then the service is generated but not served (forge project audit: codegen.unregistered_services).")
		fmt.Println("   After registering, `forge generate` also emits its cobra subcommand into cmd/services_gen.go.")
	}

	// The workloads.k declaration goes last, after the descriptor the
	// pipeline compiled — discovery is what knows the component's real shape.
	declareWorkloadInKCL(p.root, p.cfg, componentSpec{name: name, ctxLabel: "forge scaffold service " + name})
	return nil
}

// rollbackServiceScaffold removes the on-disk files a FAILED `forge scaffold
// service` scaffolded — the handler dir (internal/handlers/<pkg>) and the
// proto tree (proto/services/<pkg>) — so a validation/pipeline failure leaves
// the tree in its pre-add state instead of a half-created service. It removes
// ONLY dirs this run created: a dir that pre-existed (handlerPreexisted /
// protoPreexisted captured before scaffold) is left untouched, so a --resume /
// --force run or a re-run over manual edits never blows away real work.
// Best-effort: removal errors are surfaced as a warning, never masking the
// underlying pipeline error the caller is about to return.
func rollbackServiceScaffold(root, servicePkg string, handlerPreexisted, protoPreexisted bool) {
	fmt.Fprintf(os.Stderr, "\n↩️  Rolling back scaffold for service %q (validation failed)...\n", servicePkg)
	remove := func(dir string, preexisted bool) {
		if preexisted {
			return
		}
		if err := os.RemoveAll(dir); err != nil {
			fmt.Fprintf(os.Stderr, "  warning: could not remove %s during rollback: %v\n", dir, err)
			return
		}
		fmt.Fprintf(os.Stderr, "  ✓ removed %s\n", dir)
	}
	remove(filepath.Join(root, "internal", "handlers", servicePkg), handlerPreexisted)
	remove(filepath.Join(root, "proto", "services", servicePkg), protoPreexisted)
}

// --- scaffold package (alias for package new) ---

func newPackageCmd(f *factory.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "package <name>",
		Short: "Scaffold a new internal package (alias for 'forge package new')",
		Long: `Scaffold a new internal package under internal/<name>/.

The --type flag picks the scaffold shape:

  service     (default) classic Service/Deps/New(Deps) Service. Wired into
              the composition; callable by handlers. Also the right shape
              for a use-case orchestrator — that is a service whose Deps
              are other services' interfaces.
  adapter     Outbound boundary translator (HTTP client, queue producer,
              storage gateway). No business logic; thin translation to a
              third-party system. Marker: '// forge:outbound-io'.
              Skill: forge skill load adapter

Example:
  forge scaffold package cache
  forge scaffold package events --kind eventbus
  forge scaffold package stripe-adapter --type adapter`,
		Args: cobra.ExactArgs(1),
		RunE: f.Gen.RunPackageNew,
	}

	cmd.Flags().String("kind", "", "package kind template (e.g. eventbus, client)")
	cmd.Flags().String("type", "service", "package shape: service|adapter (see --help)")

	return cmd
}

// --- scaffold worker ---

func newWorkerCmd(f *factory.Factory) *cobra.Command {
	var kind string
	var schedule string
	var noGenerate bool

	cmd := &cobra.Command{
		Use:   "worker <name>",
		Short: "Scaffold a new background worker",
		Long: `Scaffold a new background worker into an existing Forge project.

A worker is a long-running process that doesn't serve HTTP but participates
in the single-binary lifecycle. It has Start(ctx)/Stop(ctx) methods, health
reporting, and the same Deps injection as services.

The --no-generate flag suppresses the post-scaffold ` + "`forge generate`" + ` run.
The scaffold itself (workers/<name>/*.go + forge.yaml services append) is the
only step the verb promises; the pipeline run is a convenience that becomes
hostile under parallel-agent work (see kalshi-trader migration round friction
forge-add-worker-runs-full-pipeline). Pass --no-generate when staging
scaffold-only changes in a multi-lane round and follow up with an explicit
` + "`forge generate`" + ` at a coordination point.

Example:
  forge scaffold worker email_sender
  forge scaffold worker order_processor
  forge scaffold worker cleanup --kind cron --schedule "*/5 * * * *"
  forge scaffold worker engine_shadow --kind cron --schedule "0 3 * * *" --no-generate`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWorker(f, args[0], kind, schedule, noGenerate)
		},
	}

	cmd.Flags().StringVar(&kind, "kind", "", "worker kind (use cron for scheduled workers)")
	cmd.Flags().StringVar(&schedule, "schedule", "", "cron schedule for --kind cron workers")
	cmd.Flags().BoolVar(&noGenerate, "no-generate", false, "skip the post-scaffold `forge generate` run (scaffold-only mode for parallel-agent rounds)")

	return cmd
}

func runWorker(f *factory.Factory, name, kind, schedule string, noGenerate bool) error {
	ctxLabel := fmt.Sprintf("forge scaffold worker %s", name)
	if err := validateIdentifier(name); err != nil {
		return cliutil.WrapUserErr(ctxLabel, "invalid worker name", "",
			"use a name starting with a letter, containing letters/digits/_/-", err)
	}

	kind = strings.ToLower(strings.TrimSpace(kind))
	schedule = strings.TrimSpace(schedule)
	switch kind {
	case "", "cron":
	default:
		return cliutil.UserErr(ctxLabel,
			fmt.Sprintf("invalid worker kind %q: valid kinds are cron", kind),
			"",
			"omit --kind for a long-running worker, or pass --kind=cron with --schedule for a scheduled worker")
	}
	if kind == "cron" {
		if schedule == "" {
			return cliutil.UserErr(ctxLabel, "--schedule is required when --kind cron", "",
				"pass --schedule with a 5-field cron expression (e.g. --schedule \"*/5 * * * *\")")
		}
		if _, err := cron.ParseStandard(schedule); err != nil {
			return cliutil.WrapUserErr(ctxLabel,
				fmt.Sprintf("invalid cron schedule %q", schedule), "",
				"use a 5-field cron expression (minute hour day-of-month month day-of-week), e.g. \"*/5 * * * *\"",
				err)
		}
	} else if schedule != "" {
		return cliutil.UserErr(ctxLabel, "--schedule requires --kind cron", "",
			"either drop --schedule (long-running worker) or add --kind cron")
	}

	return scaffoldComponent(componentSpec{
		name:     name,
		ctxLabel: ctxLabel,
		validate: func(string) error { return nil }, // name already validated above (cron flag rules need to run first)
		announce: func(cfg *config.ProjectConfig) {
			if kind == "cron" {
				fmt.Printf("Adding cron worker '%s' (schedule %q)...\n", name, schedule)
			} else {
				fmt.Printf("Adding worker '%s'...\n", name)
			}
		},
		scaffold: func(cfg *config.ProjectConfig, root string) error {
			// Generate worker files (worker.go, worker_test.go)
			if err := generator.GenerateWorkerFiles(root, cfg.ModulePath, name, kind, schedule); err != nil {
				return fmt.Errorf("generate worker files: %w", err)
			}
			return nil
		},
		postScaffold: func(p postScaffoldParams) error {
			if err := runPostScaffoldGenerate(f, p.root, p.name, noGenerate); err != nil {
				return err
			}
			// Scaffold the owned per-worker subcommand + print the wiring the
			// dev pastes into the owned main.go/lifecycle.go/compose.go. Generate
			// does no worker discovery, so wiring is scaffold-and-append here.
			wireWorkerIntoTree(p.cfg, p.root, p.name)
			return nil
		},
	})
}

// --- scaffold operator ---

func newOperatorCmd(f *factory.Factory) *cobra.Command {
	var (
		group              string
		version            string
		apiPackage         string
		crdType            string
		withPlaceholderCRD bool
	)

	cmd := &cobra.Command{
		Use:   "operator <name>",
		Short: "Scaffold a new Kubernetes operator",
		Long: `Scaffold a new Kubernetes operator (manager binary) into an existing Forge project.

By default this scaffolds only the operator package + manager wiring; CRDs are
added with 'forge scaffold crd <Name>' which produces a thin shim that delegates
to forge/pkg/controller.Reconciler[T].

Pass --with-placeholder-crd to keep the legacy combined types.go +
controller.go scaffold (kept for backward compatibility while users
migrate to the forge scaffold crd workflow). When --with-placeholder-crd is
set, --api-package and --crd-type tune the legacy scaffold's CRD package
and type name.

Example:
  forge scaffold operator manager
  forge scaffold operator manager --group myapp.io --version v1alpha1
  forge scaffold operator workspace --with-placeholder-crd
  forge scaffold operator workspace-controller --with-placeholder-crd --api-package workspace --crd-type Workspace --group reliant.dev --version v1alpha1`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runOperator(f, args[0], group, version, apiPackage, crdType, withPlaceholderCRD)
		},
	}

	cmd.Flags().StringVar(&group, "group", "", "API group (default: <project-name>.io)")
	cmd.Flags().StringVar(&version, "version", "v1alpha1", "API version")
	cmd.Flags().StringVar(&apiPackage, "api-package", "", "(legacy) Go package name for the placeholder CRD types")
	cmd.Flags().StringVar(&crdType, "crd-type", "", "(legacy) Placeholder CRD Go type name")
	cmd.Flags().BoolVar(&withPlaceholderCRD, "with-placeholder-crd", false, "Emit legacy types.go + controller.go scaffold (use 'forge scaffold crd' for new CRDs)")

	return cmd
}

func runOperator(f *factory.Factory, name, group, version, apiPackage, crdType string, withPlaceholderCRD bool) error {
	ctxLabel := fmt.Sprintf("forge scaffold operator %s", name)

	// Pure flag-combination guard, independent of cfg: --api-package /
	// --crd-type only mean anything with --with-placeholder-crd. Reject
	// up-front before any filesystem touch.
	if !withPlaceholderCRD && (apiPackage != "" || crdType != "") {
		return fmt.Errorf("--api-package and --crd-type only apply with --with-placeholder-crd; for the new shape use 'forge scaffold crd <Name>' after the operator is created")
	}

	return scaffoldComponent(componentSpec{
		name:     name,
		ctxLabel: ctxLabel,
		validate: func(name string) error {
			if err := validateIdentifier(name); err != nil {
				return cliutil.WrapUserErr(ctxLabel, "invalid operator name", "",
					"use a name starting with a letter, containing letters/digits/_/-", err)
			}
			return nil
		},
		preflight: func(cfg *config.ProjectConfig, _ codegen.Inventory, root string) error {
			if !cfg.Features.OperatorsEnabled() {
				return config.DisabledFeatureError(config.FeatureOperators)
			}
			// Default group from project name. Runs before announce/scaffold
			// so both see the resolved value.
			if group == "" {
				group = cfg.Name + ".io"
			}
			return nil
		},
		announce: func(cfg *config.ProjectConfig) {
			fmt.Printf("Adding operator '%s' (group=%s, version=%s)...\n", name, group, version)
		},
		scaffold: func(cfg *config.ProjectConfig, root string) error {
			if withPlaceholderCRD {
				// Legacy path: emit the combined scaffold.
				if err := generator.GenerateOperatorFilesWithAPI(root, cfg.ModulePath, name, group, version, apiPackage, crdType); err != nil {
					return fmt.Errorf("generate operator files: %w", err)
				}
			} else {
				// New default: only the operator package skeleton.
				if err := generator.GenerateOperatorBinaryOnly(root, cfg.ModulePath, name, group, version); err != nil {
					return fmt.Errorf("generate operator scaffold: %w", err)
				}
			}
			return nil
		},
		postScaffold: func(p postScaffoldParams) error {
			// Run the generation pipeline to update bootstrap.go and cmd-server.go
			fmt.Println("\n🔧 Running generation pipeline...")
			err := f.Gen.RunPipeline(p.root)
			if err != nil {
				fmt.Fprintf(os.Stderr, "warning: generation pipeline failed: %v\n", err)
				// Non-fatal: the operator files were created successfully
			}

			// Scaffold the owned per-operator subcommand + print the wiring the
			// dev pastes into the owned main.go/lifecycle.go/compose.go. Generate
			// does no operator discovery.
			wireOperatorIntoTree(p.cfg, p.root, p.name)

			fmt.Printf("\n✅ Operator '%s' added successfully!\n", name)
			if !withPlaceholderCRD {
				fmt.Printf("Next: 'forge scaffold crd <Name> --operator %s' to scaffold a CRD.\n", name)
			}

			return nil
		},
	})
}

// --- scaffold crd ---

func newCRDCmd(_ *factory.Factory) *cobra.Command {
	var (
		group    string
		version  string
		shape    string
		operator string
	)

	cmd := &cobra.Command{
		Use:   "crd <Name>",
		Short: "Scaffold a Custom Resource Definition + reconciler on an operator",
		Long: `Scaffold a Kubernetes Custom Resource Definition and its reconciler onto an
existing operator.

Generates:
  - api/<version>/<name>_types.go              CRD spec + status types
  - operators/<operator>/<name>_controller.go  thin reconciler shim
  - operators/<operator>/<name>_controller_test.go fake-client unit test

The reconciler shim embeds forge/pkg/controller.Reconciler[T] which
provides fetch / NotFound / finalizer / dispatch lifecycle automatically.
You implement ReconcileSpec (and FinalizeSpec, when finalization needs
cleanup) for the domain logic.

Shapes:
  state-machine  Spec.State drives the loop through observable phases (default).
  config         Declarative-only — Spec describes a configuration to project.
  composite      Spec owns sub-resources whose lifetime is coupled to the parent.

Example:
  forge scaffold crd Workspace
  forge scaffold crd Database --shape config
  forge scaffold crd Cluster --shape composite --operator manager`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCRD(args[0], group, version, shape, operator)
		},
	}

	cmd.Flags().StringVar(&group, "group", "", "API group (default: parent operator's group)")
	cmd.Flags().StringVar(&version, "version", "", "API version (default: parent operator's version)")
	cmd.Flags().StringVar(&shape, "shape", "state-machine", "Reconciler scaffold style: state-machine, config, composite")
	cmd.Flags().StringVar(&operator, "operator", "", "Target operator name (default: only operator in project)")

	return cmd
}

func runCRD(name, group, version, shape, operator string) error {
	ctxLabel := fmt.Sprintf("forge scaffold crd %s", name)
	if err := validateIdentifier(name); err != nil {
		return cliutil.WrapUserErr(ctxLabel, "invalid CRD name", "",
			"use a name starting with a letter, containing letters/digits/_/-", err)
	}

	root, err := projectRoot()
	if err != nil {
		return err
	}
	if err := requireServiceKind(root, "crd"); err != nil {
		return err
	}

	cfg, inv, err := readProject(root, ctxLabel)
	if err != nil {
		return err
	}

	// Resolve target operator: explicit flag > only-operator > error.
	operators := inv.OfKind(config.ComponentKindOperator)
	var op config.ComponentConfig
	if operator != "" {
		found := false
		for _, o := range operators {
			if o.Name == operator {
				op, found = o, true
				break
			}
		}
		if !found {
			return cliutil.UserErr(ctxLabel,
				fmt.Sprintf("operator %q not found in this project", operator), "",
				fmt.Sprintf("run `forge scaffold operator %s` first (operators are discovered from internal/operators/<name>/)", operator))
		}
	} else {
		switch len(operators) {
		case 0:
			return cliutil.UserErr(ctxLabel, "no operators in this project", "",
				"run `forge scaffold operator <name>` first")
		case 1:
			op = operators[0]
			operator = op.Name
		default:
			return cliutil.UserErr(ctxLabel, "multiple operators in this project", "",
				"pass --operator <name> to disambiguate")
		}
	}

	// The operator's own APIGroup/APIVersion constants are the default when
	// --group/--version are omitted (discovery reads them off the operator
	// package); the fallbacks below cover an operator package that declares
	// neither constant.
	if group == "" {
		group = op.Group
	}
	if group == "" {
		group = cfg.Name + ".io"
	}
	if version == "" {
		version = op.Version
	}
	if version == "" {
		version = "v1alpha1"
	}

	crdShape := generator.CRDShape(shape)
	if !crdShape.IsValid() {
		return cliutil.UserErr(ctxLabel,
			fmt.Sprintf("invalid --shape %q", shape), "",
			"pass --shape state-machine, config, or composite")
	}

	for _, c := range op.CRDs {
		if c.Name == name {
			return cliutil.UserErr(ctxLabel,
				fmt.Sprintf("CRD %q already exists in operator %q", name, operator), "",
				"pick a different CRD name, or remove the existing one first")
		}
	}

	fmt.Printf("Adding CRD '%s' to operator '%s' (group=%s, version=%s, shape=%s)...\n",
		name, operator, group, version, crdShape)

	if err := generator.GenerateCRDFiles(generator.CRDGenInput{
		Root:         root,
		ModulePath:   cfg.ModulePath,
		OperatorName: operator,
		TypeName:     name,
		Group:        group,
		Version:      version,
		Shape:        crdShape,
	}); err != nil {
		return fmt.Errorf("generate CRD files: %w", err)
	}

	// Nothing is written back: the emitted <lower>_controller.go IS the CRD's
	// declaration, and that is what the duplicate check above reads.
	fmt.Printf("\n✅ CRD '%s' added to operator '%s'!\n", name, operator)
	return nil
}

// --- scaffold frontend ---

func newFrontendCmd(_ *factory.Factory) *cobra.Command {
	var port int
	var kind string
	var output string
	var basePath string
	var authMode string
	var routes []string

	cmd := &cobra.Command{
		Use:   "frontend <name>",
		Short: "Scaffold a new frontend",
		Long: `Scaffold a new frontend into an existing forge project.

By default this creates a Next.js web frontend with Connect RPC client setup.
Use --kind mobile to scaffold a React Native app using Expo.
Use --kind vite-spa to scaffold a Vite + React + tanstack-router SPA.

For Next.js frontends (--kind web, the default), --output selects the
production build/runtime shape. "standalone" (the default) emits a
self-contained Node server that pairs with the generated Dockerfile and
supports the dynamic [id] CRUD routes forge generates. Opt into
"static" only when the frontend has no dynamic routes — static export
fails the build on the generated /<entity>/[id] pages. Use "server"
for full Next.js dev+prod (next start).

--routes limits which entities get generated CRUD pages. By default forge
scaffolds a list/detail/create/edit route set for EVERY entity in the
project, which is right for a project's first frontend and wrong for every
one after it — a purpose-built frontend starts by deleting most of what was
just written. Naming routes makes the set an allowlist, so entities added
later do not silently appear in this frontend. The value is persisted as
frontends[].routes and honored by every subsequent forge generate run.

--base-path mounts the frontend under a URL prefix (e.g. /admin behind a
reverse proxy that blends several apps on one host). It is persisted as
frontends[].base_path in forge.yaml and rendered into next.config.ts
(basePath + assetPrefix) and the generated src/lib/basepath_gen.ts
helper. The single runtime override is NEXT_PUBLIC_BASE_PATH.

--auth-mode picks where the user types their password. "redirect" is the
default and the only mode forge scaffolds: sign-in happens on the IdP's
own hosted pages, which is portable across every IdP, and MFA, social
sign-in and password reset come for free because the provider
implements them. A first-party form inside your app is possible — the
password goes from the browser to the IdP either way — but every
implementation of one drives a single provider's proprietary API, so
there is nothing portable to generate. Bringing the environment up
registers the new frontend with the dev IdP; nothing else to run.

Example:
  forge scaffold frontend web
  forge scaffold frontend dashboard --port 3001
  forge scaffold frontend mobile --kind mobile
  forge scaffold frontend admin --kind vite-spa
  forge scaffold frontend dashboard --output standalone
  forge scaffold frontend admin --base-path /admin
  forge scaffold frontend web --auth-mode credentials
  forge scaffold frontend ops --routes users,usage-events`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runFrontend(cmd.Context(), args[0], port, kind, output, basePath, authMode, routes)
		},
	}

	cmd.Flags().IntVar(&port, "port", 0, "Pin the frontend dev-server port. Default (unset) allocates a free port at launch — set this only when the port is externally fixed (e.g. an OAuth redirect URI registered with an IdP).")
	cmd.Flags().StringVar(&kind, "kind", "", "frontend kind (web, mobile, or vite-spa)")
	cmd.Flags().StringVar(&output, "output", "", "Next.js output shape: standalone (default), static, or server. Only applies to --kind web.")
	cmd.Flags().StringVar(&basePath, "base-path", "", `URL prefix the frontend is mounted under (e.g. "/admin"). Only applies to --kind web.`)
	cmd.Flags().StringVar(&authMode, "auth-mode", "", "Where the user signs in. Only `redirect` (the default) is scaffolded: sign-in happens on the IdP's own hosted pages. A first-party form is yours to build against your IdP's API — every such API is provider-specific.")
	cmd.Flags().StringSliceVar(&routes, "routes", nil, "Only generate CRUD pages for these entity route slugs (e.g. --routes users,usage-events). Default (unset) generates a page set for EVERY entity. Persisted as frontends[].routes and honored by every later generate run.")

	return cmd
}

// frontendFlags is the normalized, validated form of the flags
// `forge scaffold frontend` accepts. Values are lowercased and trimmed, so
// downstream comparisons can be exact.
type frontendFlags struct {
	kind     string
	output   string
	basePath string
	authMode string
}

// validateFrontendFlags normalizes and cross-checks the scaffold flags before
// anything is written.
//
// --output and --base-path are Next.js-only, so passing either with
// --kind mobile / vite-spa is refused here rather than silently ignored: the
// flag would otherwise appear accepted and have no effect on the scaffold.
func validateFrontendFlags(ctxLabel, kind, output, basePath, authMode string) (frontendFlags, error) {
	kind = strings.ToLower(strings.TrimSpace(kind))
	switch kind {
	case "", "web", "mobile", "vite-spa":
	default:
		return frontendFlags{}, cliutil.UserErr(ctxLabel,
			fmt.Sprintf("invalid frontend kind %q", kind), "",
			"pass --kind web, mobile, or vite-spa")
	}

	output = strings.ToLower(strings.TrimSpace(output))
	switch output {
	case "", "static", "standalone", "server":
	default:
		return frontendFlags{}, cliutil.UserErr(ctxLabel,
			fmt.Sprintf("invalid --output %q", output), "",
			"pass --output standalone (default), static, or server")
	}
	if output != "" && kind != "" && kind != "web" {
		return frontendFlags{}, cliutil.UserErr(ctxLabel,
			fmt.Sprintf("--output only applies to Next.js frontends (--kind web); got --kind %q", kind), "",
			"drop --output for non-web frontends, or use --kind web")
	}

	// The base-path shape contract is shared with forge.yaml validation
	// (leading "/", no trailing "/", [A-Za-z0-9._-] segments) — the value is
	// spliced verbatim into next.config.ts and generated TypeScript literals.
	basePath = strings.TrimSpace(basePath)
	if basePath != "" {
		if msg, ok := config.ValidateBasePath(basePath); !ok {
			return frontendFlags{}, cliutil.UserErr(ctxLabel,
				fmt.Sprintf("invalid --base-path %q: %s", basePath, msg), "",
				"use a leading \"/\", no trailing \"/\", and [A-Za-z0-9._-] segments")
		}
		if kind != "" && kind != "web" {
			return frontendFlags{}, cliutil.UserErr(ctxLabel,
				fmt.Sprintf("--base-path only applies to Next.js frontends (--kind web); got --kind %q", kind), "",
				"drop --base-path for non-web frontends, or use --kind web")
		}
	}

	authMode = strings.ToLower(strings.TrimSpace(authMode))
	if err := validateAuthMode(ctxLabel, authMode); err != nil {
		return frontendFlags{}, err
	}
	return frontendFlags{kind: kind, output: output, basePath: basePath, authMode: authMode}, nil
}

func runFrontend(ctx context.Context, name string, port int, kind, output, basePath, authMode string, routes []string) error {
	ctxLabel := fmt.Sprintf("forge scaffold frontend %s", name)
	if err := validateFrontendName(name); err != nil {
		return cliutil.WrapUserErr(ctxLabel, "invalid frontend name", "",
			"use a name starting with a letter, containing letters/digits/_/-", err)
	}

	flags, err := validateFrontendFlags(ctxLabel, kind, output, basePath, authMode)
	if err != nil {
		return err
	}
	kind, output, basePath, authMode = flags.kind, flags.output, flags.basePath, flags.authMode

	root, err := projectRoot()
	if err != nil {
		return err
	}
	if err := requireServiceKind(root, "frontend"); err != nil {
		return err
	}

	cfg, _, err := readProject(root, ctxLabel)
	if err != nil {
		return err
	}

	// Check for name conflict.
	//
	// Two names conflict when they CANONICALIZE the same, not only when
	// they are spelled the same. Everything forge derives from a frontend
	// name — the config proto file (proto/config/v1/<name>_config.proto),
	// the dev-IdP KCL fragment, the Go package — goes through
	// naming.GoPackage, which folds hyphens to underscores. So "foo-bar"
	// and "foo_bar" are distinct in forge.yaml but name ONE config proto.
	//
	// Silently, too: WriteFrontendConfigProto refuses to overwrite an
	// existing file (it holds an issuer and client id it cannot
	// reconstruct), so scaffolding the second frontend would succeed while
	// quietly binding it to the FIRST one's config — a frontend reading
	// another frontend's OIDC client id, discovered at runtime as a login
	// that redirects to the wrong app. Refusing at the point the name is
	// chosen is the only place this is still cheap to fix.
	canonical := naming.GoPackage(name)
	for _, frontend := range cfg.Frontends {
		if frontend.Name == name {
			return cliutil.UserErr(ctxLabel,
				fmt.Sprintf("frontend %q already exists in the project", name),
				"",
				"pick a different name, or remove the existing frontend first")
		}
		if naming.GoPackage(frontend.Name) == canonical {
			return cliutil.UserErr(ctxLabel,
				fmt.Sprintf("frontend %q collides with existing frontend %q: both canonicalize to %q",
					name, frontend.Name, canonical),
				fmt.Sprintf("forge derives generated names from the canonical form, so both frontends would "+
					"claim proto/config/v1/%s_config.proto and deploy/kcl/dev/identity_%s_gen.k",
					canonical, canonical),
				fmt.Sprintf("pick a name that canonicalizes differently (hyphens and underscores are "+
					"equivalent here — %q and %q are the same name to forge), or remove the existing frontend first",
					name, frontend.Name))
		}
	}

	// Port 0 = EPHEMERAL, and it stays that way. `forge project new` already
	// scaffolds FrontendPort: 0 for this reason (see project.go): the
	// frontend port is omitempty in forge.yaml, and `forge run` / `forge env
	// up` allocate a free OS port at launch and print it in the summary.
	//
	// This used to auto-assign 3000 (then 3001, …) instead, which put a
	// literal in forge.yaml that no longer had anything to do with what was
	// free — so on a host running several forge stacks, `forge run` failed
	// on a port another project already held and the fix was to hand-edit
	// forge.yaml. Scaffolding a frontend now teaches the same "discover the
	// dev port from forge's output, don't hardcode 3000" pattern the rest of
	// the toolchain already assumes. An explicit --port is still honored
	// verbatim, for a frontend whose port is externally fixed (an OAuth
	// redirect URI registered with an IdP is a literal string).

	frontendType := "nextjs"
	frontendKind := kind
	frontendDescription := "frontend"
	switch kind {
	case "mobile":
		frontendType = "react-native"
		frontendDescription = "mobile frontend"
	case "vite-spa":
		frontendType = "vite-spa"
		frontendDescription = "Vite SPA frontend"
	case "":
		frontendKind = ""
	}

	if port == 0 {
		fmt.Printf("Adding %s '%s' (dev port allocated at launch)...\n", frontendDescription, name)
	} else {
		fmt.Printf("Adding %s '%s' (port %d)...\n", frontendDescription, name, port)
	}

	// The dev API port baked into the new frontend's apiurl_gen.ts. Every
	// service mounts onto the binary's one Connect mux, so this is the same
	// number for every project — and it must match what `forge generate`
	// rewrites the file to (resolveDevAPIPort), or scaffolding a frontend
	// leaves connect.ts pointed somewhere generate immediately moves it.
	apiPort := config.DefaultServePort

	// Generate frontend files. When the project has opted into the
	// pnpm-workspaces layout, both the per-frontend file emitter (so
	// package.json declares workspace deps + connect.ts imports from
	// the shared package) and the workspace scaffolder (so packages/api
	// + packages/hooks + pnpm-workspace.yaml exist if this is the first
	// frontend added since flipping the flag) need to know.
	workspaces := cfg.IsFrontendWorkspacesEnabled()
	if err := generator.WriteFrontendWorkspaceFiles(root, cfg.Name, workspaces); err != nil {
		return fmt.Errorf("write frontend workspace files: %w", err)
	}
	// Declare this frontend's public runtime config, then render its files
	// against it. The (forge.v1.frontend_config) annotation in that proto
	// is what activates the typed config module, the KCL schema and the
	// per-env config.js — a frontend added without one gets none of the
	// config system and falls back to build-time env vars.
	//
	// This is append-only on an existing project BY CONSTRUCTION: it
	// writes a NEW per-frontend file, so no config content already on disk
	// is read, rewritten or reordered, and an existing file for this
	// frontend is left exactly as it is.
	if err := generator.WriteFrontendConfigProto(root, cfg.ModulePath, name, apiPort); err != nil {
		return fmt.Errorf("write frontend config proto: %w", err)
	}

	// Which typed fields the frontend's templates may read. A project that
	// already declared this frontend's config by hand is authoritative —
	// read it, so a hand-written message with a narrower field set does not
	// get templates referencing keys it lacks. Otherwise the set comes from
	// the proto just written, because `forge generate` has not run yet and
	// there is no descriptor to parse.
	typedConfig := frontendTypedConfigFor(root, name)
	if !typedConfig.Bound {
		typedConfig = generator.ScaffoldedFrontendTypedConfig()
	}
	if err := generator.GenerateFrontendFilesWithOptions(root, cfg.ModulePath, cfg.Name, name, apiPort, kind, generator.FrontendGenOptions{
		Workspaces:  workspaces,
		Output:      output,
		BasePath:    basePath,
		TypedConfig: typedConfig,
	}); err != nil {
		return fmt.Errorf("generate frontend files: %w", err)
	}
	// When the frontend just added is React Native AND workspaces are
	// on, scaffold the @<scope>/ui-native primitives package alongside.
	// The forge.yaml hasn't been written back to disk yet at this
	// point so HasReactNativeFrontend(cfg) can't see the new entry —
	// we detect via the explicit `kind == "mobile"` we already
	// branched on a few lines up.
	if workspaces && kind == "mobile" {
		layout := generator.NewFrontendWorkspaceLayout(cfg.Name)
		if err := generator.WriteUINativePackageFiles(root, layout); err != nil {
			return fmt.Errorf("write ui-native package files: %w", err)
		}
	}

	// ── Write back THREE fields, each in place. ──
	//
	// This command runs against a forge.yaml the user has been living in
	// for the life of the project, and it changes three things. It used to
	// change them by marshalling the whole config struct back over the
	// file, which is a rewrite, not an update: a Go struct models no
	// comments and no key order, and NormalizeForWrite drops every section
	// whose values happen to match what forge would derive. On a real
	// manifest that silently deleted the comment header, the ci: and lint:
	// blocks, and most of features:. The file still loaded, so nothing
	// complained. Each mutation below edits only its own bytes.
	configPath := filepath.Join(root, "forge.yaml")

	entry := buildFrontendEntry(frontendEntryInput{
		Name:         name,
		FrontendType: frontendType,
		FrontendKind: frontendKind,
		Kind:         kind,
		Output:       output,
		BasePath:     basePath,
		AuthMode:     authMode,
		Port:         port,
		Routes:       routes,
	})
	cfg.Frontends = append(cfg.Frontends, entry)
	if err := generator.AppendFrontendEntryToConfig(configPath, entry); err != nil {
		return fmt.Errorf("update project config: %w", err)
	}

	// Flip features.frontend on so subsequent `forge generate` runs
	// pick up the frontend codegen pass. Projects scaffolded with
	// `forge project new --kind service` (no --frontend) leave this field
	// explicitly false; without this flip the frontend dir + files
	// would be emitted but never regenerated. Use a stable address so
	// the *bool survives marshal round-trips.
	frontendOn := true
	cfg.Features.Frontend = &frontendOn
	if err := generator.SetProjectConfigScalarPath(configPath, []string{"features", "frontend"}, true); err != nil {
		return fmt.Errorf("update project config: %w", err)
	}

	// Bring stack.frontend.framework in sync with the frontend we just
	// added. Projects scaffolded without --frontend leave this field as
	// "none" — downstream tooling (lint config, CI, codegen branching)
	// reads the framework field directly and would misread the project
	// as having no frontend stack. Only overwrite when empty or "none"
	// so a user who set something exotic (e.g. "svelte") keeps it.
	if cfg.Stack.Frontend.Framework == "" || cfg.Stack.Frontend.Framework == "none" {
		cfg.Stack.Frontend.Framework = frontendType
		if err := generator.SetProjectConfigScalarPath(configPath,
			[]string{"stack", "frontend", "framework"}, frontendType); err != nil {
			return fmt.Errorf("update project config: %w", err)
		}
	}

	// Install the new frontend's npm dependencies so the user can run
	// the dev server (or `forge generate` post-codegen for the hooks
	// import) without an extra manual step. Failures here are non-fatal:
	// if `npm` isn't on PATH, the scaffold is still on disk and we just
	// nudge the user to install dependencies themselves.
	frontendDir := filepath.Join(root, "frontends", name)
	if err := runFrontendNpmInstall(ctx, frontendDir); err != nil {
		fmt.Printf("\n⚠️  %v\n", err)
	}

	fmt.Printf("\n✅ Frontend '%s' added successfully!\n", name)
	reportFrontendAuthNextStep(root)

	return nil
}

// reportFrontendAuthNextStep points at the dev IdP when the project has no
// OIDC issuer configured yet.
//
// Auth is fail-closed in every mode. Bringing the environment up configures
// sign-in automatically — the IdP is declared infrastructure and the
// application registration is converged during bring-up — so this says where
// to sign in rather than handing over a setup procedure. Without the line, the
// cheap-looking move is to hand-roll an AuthProvider that injects a pasted
// token, which is a credential in a repo and a file that has to be deleted
// later; the PKCE provider the scaffold already ships needs no code.
func reportFrontendAuthNextStep(root string) {
	// Only speak up when nothing is configured. A project already pointed at
	// an issuer (dev IdP or hosted) needs no advice. The DirSecrets store is
	// one file per secret, so presence of a non-empty file IS the answer —
	// no parsing, and nothing to read out of the value.
	for _, key := range []string{"JWT_JWKS_URL", "JWT_ISSUER"} {
		info, err := os.Stat(filepath.Join(root, "secrets", "dev", key))
		if err == nil && info.Size() > 0 {
			return
		}
	}
	fmt.Print(`
🔑 Auth is fail-closed: every RPC that needs a caller answers 401 until you
   sign in. Nothing to set up —

       forge run

   brings up the identity provider this project declares, registers this
   frontend with it, and prints the sign-in URL and the dev credentials.
   Details, including the traps (opaque-vs-JWT access tokens, and pointing a
   real deployment at your own issuer):

       forge skill load auth/dev-loop
`)
}

// runFrontendNpmInstall runs `npm install` in the freshly scaffolded
// frontend directory so the user can immediately run the dev server.
// A missing `npm` binary is treated as a soft warning — the scaffold
// itself succeeded and the user can install dependencies later.
//
// FORGE_SKIP_NPM_INSTALL=1 short-circuits the install. This is the
// testing seam: unit tests that exercise the forge.yaml/scaffold logic
// of `forge scaffold frontend` don't care about node_modules, and the npm
// install is ~13s apiece — three such tests dominated the entire
// internal/cli suite (~80s of an ~85s package). They set this under
// `go test -short` only (see skipNpmInstallInShortMode in
// add_frontend_test.go), so full mode still runs the real install; the
// npm-driven frontend build is additionally covered by the e2e frontend
// fixture, which needs node_modules to actually build.
func runFrontendNpmInstall(ctx context.Context, frontendDir string) error {
	if os.Getenv("FORGE_SKIP_NPM_INSTALL") != "" {
		return nil
	}
	cmd := exec.CommandContext(ctx, "npm", "install")
	cmd.Dir = frontendDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	fmt.Printf("\nRunning `npm install` in %s ...\n", frontendDir)
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return fmt.Errorf("npm not found on PATH; run `npm install` in %s manually", frontendDir)
		}
		return fmt.Errorf("npm install failed in %s: %v (run it manually to see full output)", frontendDir, err)
	}
	return nil
}

// --- scaffold webhook ---

func newWebhookCmd(f *factory.Factory) *cobra.Command {
	var serviceName string

	cmd := &cobra.Command{
		Use:   "webhook <name>",
		Short: "Scaffold a webhook endpoint on an existing service",
		Long: `Scaffold a webhook ingestion endpoint onto an existing Go service.

This scaffolds a webhook handler with signature verification and idempotency,
along with a test file. The handler is added to the service's handler directory.

Example:
  forge scaffold webhook stripe --service payments
  forge scaffold webhook github --service notifications`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWebhook(f, args[0], serviceName)
		},
	}

	cmd.Flags().StringVar(&serviceName, "service", "", "Target service name (required)")
	_ = cmd.MarkFlagRequired("service")

	return cmd
}

func runWebhook(f *factory.Factory, name, serviceName string) error {
	ctxLabel := fmt.Sprintf("forge scaffold webhook %s", name)
	if err := validateProjectName(name); err != nil {
		return cliutil.WrapUserErr(ctxLabel, "invalid webhook name", "",
			"use a name starting with a letter, containing letters/digits/_/-", err)
	}

	root, err := projectRoot()
	if err != nil {
		return err
	}
	if err := requireServiceKind(root, "webhook"); err != nil {
		return err
	}

	cfg, inv, err := readProject(root, ctxLabel)
	if err != nil {
		return err
	}

	// Find the target service.
	svc, found := inv.Named(serviceName)
	if !found {
		return cliutil.UserErr(ctxLabel,
			fmt.Sprintf("service %q not found in this project", serviceName), "",
			fmt.Sprintf("run `forge scaffold service %s` first, or pass --service with an existing service name", serviceName))
	}
	// Webhooks require a serving binary; declaring one on a service this
	// binary does not register (no serviceRow in pkg/app/services.go)
	// would fail the next `forge generate`. Reject at add time with the
	// full story. Best-effort parse: a broken registry falls open here —
	// the generate-time check is the hard gate.
	if reg, regErr := f.Gen.LoadServiceRegistry(root); regErr == nil &&
		f.Gen.IsConnectServiceConfig(svc) && !reg.Registered(serviceName) {
		return cliutil.UserErr(ctxLabel,
			fmt.Sprintf("service %q is not registered in %s — webhooks require a serving binary", serviceName, f.Gen.ServiceRegistryRelPath),
			"",
			fmt.Sprintf("add `%s(app, cfg, logger, opts...),` to RegisteredServices there first, or add the webhook to the binary that serves it", codegen.ServiceRowFuncName(serviceName)))
	}

	// Duplicate check against the REAL source: the webhook_<name>.go handler
	// file. Webhooks aren't a declared config list — the file IS the
	// declaration, so a webhook "exists" iff its handler file does.
	handlerDir := filepath.Join(root, "internal", "handlers", naming.ServicePackage(serviceName))
	if _, statErr := os.Stat(filepath.Join(handlerDir, "webhook_"+name+".go")); statErr == nil {
		return cliutil.UserErr(ctxLabel,
			fmt.Sprintf("webhook %q already exists in service %q", name, serviceName), "",
			"pick a different webhook name, or remove the existing webhook_"+name+".go first")
	}

	fmt.Printf("Adding webhook '%s' to service '%s'...\n", name, serviceName)

	// Generate the webhook handler files. These files ARE the webhook's
	// source of truth — `forge generate` discovers webhooks by scanning for
	// webhook_<name>.go, so nothing is written back to forge.yaml.
	if err := generator.GenerateWebhookFiles(root, cfg.ModulePath, serviceName, name); err != nil {
		return fmt.Errorf("generate webhook files: %w", err)
	}

	fmt.Printf("\n✅ Webhook '%s' added to service '%s'!\n", name, serviceName)

	return nil
}

// --- scaffold binary ---

// newBinaryCmd is the cobra surface for `forge scaffold binary <name>`.
//
// "Binary" is the third long-running shape forge generates, alongside
// service (Connect-RPC server) and worker (in-process goroutine under
// the canonical server). It exists for processes that need their own
// Deployment but don't fit the server / worker / operator templates —
// reverse proxies, sidecars, off-service NATS consumers, gateways.
//
// Pre-binary, every project that needed a second long-running process
// hand-wrote ~270 LOC of cobra + signal-handling + lifecycle
// boilerplate (cpnext's workspace_proxy went through this rewrite three
// times across rebuilds). The scaffold here lifts that boilerplate
// into the generator so the next equivalent is the user's business
// logic plus a thin glue layer.
func newBinaryCmd(_ *factory.Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "binary <name>",
		Short: "Scaffold a non-server long-running binary",
		Long: `Scaffold a non-server long-running binary into an existing Forge project.

A binary is a process with its own Deployment shape. Use this when:
  - You need a reverse proxy / gateway in front of pods.
  - You want an off-service NATS consumer that isn't an in-process worker.
  - You need a sidecar with its own deploy lifecycle.

For in-process background work, use 'forge scaffold worker' instead — workers
share the canonical server's lifecycle and Deps.

This creates:
  cmd/<name>.go                       Cobra subcommand (registered against the shared root)
  internal/<name>/contract.go          Deps, Service, New(deps) (*Runner, error)
  internal/<name>/<name>.go            Runner.Run(ctx) lifecycle body
  internal/<name>/<name>_test.go       Lifecycle + validateDeps tests

And an entry under 'binaries:' in forge.yaml so deploy emits a
Deployment for the binary. See the binaries skill (` + "`forge skill load binaries`" + `)
for when to choose a binary vs worker vs service.

Example:
  forge scaffold binary workspace-proxy`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBinary(args[0])
		},
	}

	return cmd
}

func runBinary(name string) error {
	ctxLabel := fmt.Sprintf("forge scaffold binary %s", name)

	return scaffoldComponent(componentSpec{
		name:     name,
		ctxLabel: ctxLabel,
		validate: func(name string) error {
			if err := validateIdentifier(name); err != nil {
				return cliutil.WrapUserErr(ctxLabel, "invalid binary name", "",
					"use a name starting with a letter, containing letters/digits/_/-", err)
			}
			return nil
		},
		checkConflict: func(inv codegen.Inventory, name, ctxLabel string) error {
			// Conflict checks. Binaries share the cmd/ directory with the
			// canonical `cmd/server.go` and any per-service shared subcommands,
			// so we check every component (server/worker/cron/operator/binary).
			if err := requireNoComponentNamed(inv, name, ctxLabel); err != nil {
				return err
			}
			// Reserved cobra subcommand names that would shadow the binary.
			switch naming.ServicePackage(name) {
			case "server", "version", "db":
				return cliutil.UserErr(ctxLabel,
					fmt.Sprintf("%q conflicts with a reserved cobra subcommand", name),
					"",
					"pick a different name (server/version/db are reserved)")
			}
			return nil
		},
		announce: func(cfg *config.ProjectConfig) {
			fmt.Printf("Adding binary '%s'...\n", name)
		},
		scaffold: func(cfg *config.ProjectConfig, root string) error {
			// Generate the four scaffold files (cmd-binary.go, contract.go,
			// binary.go, binary_test.go).
			if err := generator.GenerateBinaryFiles(root, cfg.ModulePath, name); err != nil {
				return fmt.Errorf("generate binary files: %w", err)
			}
			return nil
		},
		postScaffold: func(p postScaffoldParams) error {
			pkg := naming.ServicePackage(name)
			fmt.Printf("\n✅ Binary '%s' added successfully!\n", name)
			fmt.Printf("   - cmd/%s.go\n", pkg)
			fmt.Printf("   - internal/%s/contract.go\n", pkg)
			fmt.Printf("   - internal/%s/%s.go\n", pkg, pkg)
			fmt.Printf("   - internal/%s/%s_test.go\n", pkg, pkg)
			fmt.Printf("   - forge.yaml (binaries: entry)\n\n")
			fmt.Printf("Next steps:\n")
			fmt.Printf("  1. Edit internal/%s/%s.go to implement the runtime loop.\n", pkg, pkg)
			fmt.Printf("  2. It was declared in deploy/kcl/workloads.k as kind=\"tool\": built\n")
			fmt.Printf("     into the image, never scheduled. Refine it per env in\n")
			fmt.Printf("     deploy/kcl/<env>/main.k if it needs to run there — both files are yours.\n")
			return nil
		},
	})
}

// normalizeRouteSlugs canonicalizes --routes input: lowercased, surrounding
// slashes trimmed, blanks dropped, duplicates collapsed, order preserved.
//
// Tolerating "/users" alongside "users" matters because the value an author
// has at hand is usually a URL they copied, not the on-disk directory name.
func normalizeRouteSlugs(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, r := range in {
		slug := strings.ToLower(strings.Trim(strings.TrimSpace(r), "/"))
		if slug == "" || seen[slug] {
			continue
		}
		seen[slug] = true
		out = append(out, slug)
	}
	return out
}

// frontendEntryInput is buildFrontendEntry's parameter set. A struct rather
// than nine positional args: several are same-typed strings, so a swapped pair
// would compile cleanly and write the wrong forge.yaml.
type frontendEntryInput struct {
	Name         string
	FrontendType string
	FrontendKind string
	Kind         string
	Output       string
	BasePath     string
	AuthMode     string
	Port         int
	Routes       []string
}

// buildFrontendEntry assembles the forge.yaml entry for a newly scaffolded
// frontend.
//
// Optional fields are written ONLY when the user asked for them: leaving a
// field empty lets its scaffold default evolve in a later forge version
// without every existing forge.yaml pinning the old value. That is why each
// assignment below is conditional rather than unconditional.
func buildFrontendEntry(in frontendEntryInput) config.FrontendConfig {
	fe := config.FrontendConfig{
		Name: in.Name,
		Type: in.FrontendType,
		Kind: in.FrontendKind,
		Path: fmt.Sprintf("frontends/%s", in.Name),
		Port: in.Port,
	}
	isWeb := in.Kind == "" || in.Kind == "web"
	if in.Output != "" && isWeb {
		fe.Output = in.Output
	}
	// base_path drives the regenerated basepath_gen.ts helper's prefix.
	if in.BasePath != "" && isWeb {
		fe.BasePath = in.BasePath
	}
	// The route allowlist MUST persist: otherwise the next `forge generate`
	// re-scaffolds the whole entity set this flag was used to avoid, and the
	// frontend's route surface would depend on which command last touched it.
	if len(in.Routes) > 0 {
		fe.Routes = normalizeRouteSlugs(in.Routes)
	}
	return fe
}

// frontendTypedConfigFor reports which typed config fields the project
// declares for the named frontend, by reading the config protos and finding
// the message bound to it with (forge.v1.frontend_config).
//
// A parse failure or an absent message yields the zero value, which renders
// the frontend's previous env-var form. That degradation is safe HERE, and
// only here, because this path decides which template variant to scaffold —
// not whether a secret may ship. The sensitive-field refusal lives in
// `forge generate`, which fails loudly rather than degrading.
func frontendTypedConfigFor(root, frontendName string) generator.FrontendTypedConfig {
	messages, err := codegen.ParseConfigProtosFromDir(filepath.Join(root, "proto", "config"))
	if err != nil {
		return generator.FrontendTypedConfig{}
	}
	for _, fc := range codegen.FrontendConfigsFromMessages(messages) {
		if fc.Frontend != frontendName {
			continue
		}
		envVars := make([]string, 0, len(fc.Fields))
		for _, f := range fc.Fields {
			if f.EnvVar != "" {
				envVars = append(envVars, f.EnvVar)
			}
		}
		return generator.FrontendTypedConfigFrom(envVars)
	}
	return generator.FrontendTypedConfig{}
}

// validateAuthMode rejects an unrecognized --auth-mode.
//
// "redirect" is the only mode forge scaffolds. A first-party sign-in form is
// a legitimate thing to build, but every implementation of one is specific to
// a single provider's API, so there is nothing portable to generate.
func validateAuthMode(ctxLabel, authMode string) error {
	switch authMode {
	case "", config.AuthModeRedirect:
		return nil
	default:
		return cliutil.UserErr(ctxLabel,
			fmt.Sprintf("invalid --auth-mode %q", authMode), "",
			"pass --auth-mode redirect (the default, and the only mode forge scaffolds)")
	}
}
