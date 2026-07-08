package codegen

import (
	"fmt"
	"go/format"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/naming"
	"github.com/reliant-labs/forge/internal/templates"
)

// reservedSubcommandNames are top-level cobra Use values the generated cmd
// tree already claims (server / version / db, cobra built-ins). A service
// whose runtime name collides is SKIPPED — with a NOTE in the generated
// services/register_gen.go — instead of emitting a subcommand that would
// shadow (or be shadowed by) the built-in. The colliding service is still
// reachable via `server <name>` (the typed MountByName convenience).
var reservedSubcommandNames = map[string]bool{
	"server":     true,
	"version":    true,
	"db":         true,
	"help":       true,
	"completion": true,
}

// CmdGroupItem is one generated command-group entry — one
// cmd/<bin>/cmd/<group>/<name>.go file that self-registers via init().
// It serves services, workers, and operators alike.
type CmdGroupItem struct {
	// Module is the project module path (for the import lines).
	Module string

	// Bin is the primary binary name — the cmd/<bin>/cmd import path segment
	// the group file imports for the shared Deps/Serve helpers.
	Bin string

	// Name is the runtime kebab-case component name — the cobra Use value and
	// the <name>.go filename stem. Identical derivation to the app inventory
	// row Name and the typed Mount<Svc> / Worker<X>() / Operator<X>() accessors.
	Name string

	// FieldName is the exported PascalCase suffix used in the generated
	// command constructor (New<FieldName>Cmd) and self-registration. It is
	// the PLAIN per-role name (ToPascalCase of the trimmed service name) —
	// the constructor is a local symbol in the group package, so it is not
	// subject to the cross-role collision rename. Workers/operators set
	// FieldName == MountFieldName (no Components-field mount, no collision).
	FieldName string

	// MountFieldName is the exported suffix of the typed mount METHOD the
	// service command calls: (*app.Components).Mount<MountFieldName>. For
	// services it MUST equal the name inventory_gen emitted — which is
	// collision-aware (ResolveCollisionNaming), so a service whose handler
	// package collides cross-role with an internal package mounts as
	// MountSvc<Pkg>, not Mount<Pkg>. Keeping this SEPARATE from FieldName lets
	// the constructor stay New<Plain>Cmd while the mount call names the
	// collision-renamed Components method — matching inventory_gen exactly and
	// killing the MountBilling/MountSvcBilling mismatch (BUG 1). For
	// workers/operators it is set equal to FieldName (their template doesn't
	// reference a Components mount method).
	MountFieldName string
}

// CmdServicesTemplateData feeds cmd-svc-register.go.tmpl (the services group
// anchor + collision NOTEs).
type CmdServicesTemplateData struct {
	Skipped []string // runtime names skipped for colliding with built-ins
}

// CmdCtorRef is one group-command constructor reference for the composition
// root — the exported New<X>Cmd func name the main.go template qualifies with
// its group package (services./workers./operators.).
type CmdCtorRef struct {
	Ctor string // e.g. "NewAuditLogCmd"
}

// CmdMainTemplateData feeds cmd-main.go.tmpl — the composition root. main.go is
// no longer a thin blank-import + cmd.Execute(); it names EVERY group
// constructor explicitly and passes them to cmd.Execute. That makes main.go
// inventory-dependent (like the per-component group files), so it is rendered
// here from the SAME service/worker/operator rows the group files are — not
// from the project-level scaffold data — which is why GenerateCmdGroups owns it
// and the upgrade managed-file list does not.
type CmdMainTemplateData struct {
	Module    string
	Bin       string
	Services  []CmdCtorRef
	Workers   []CmdCtorRef
	Operators []CmdCtorRef
}

// cmdSvcFieldName derives the no-collision exported FieldName for a raw
// service-name spelling — the same ToPascalCase(TrimSuffix("Service"))
// derivation GenerateInventory's fallbackField uses BEFORE
// ResolveCollisionNaming. Shared so the two halves can't drift.
func cmdSvcFieldName(name string) string {
	field := naming.ToPascalCase(strings.TrimSuffix(name, "Service"))
	if field == "" {
		field = naming.ToPascalCase(name)
	}
	return field
}

// cmdServiceItemsFromNames projects raw service names (any spelling: proto
// "AdminServerService", forge.yaml "admin-server") onto group-item rows,
// filtering reserved collisions into the skipped list. The kebab/Pascal
// derivation is identical to GenerateInventory's so the subcommand Name
// always equals the inventory row Name and the typed mount method it selects.
//
// mountOverride, when non-nil, supplies the collision-aware MOUNT method name
// for a given raw service name — the EXACT name inventory_gen's
// ResolveCollisionNaming produced for that service (the Svc-prefixed name when
// its handler package collides cross-role with an internal package). The
// cmd-group command's typed mount call ((*app.Components).Mount<MountFieldName>)
// MUST match the method inventory_gen emitted, so when a collision exists the
// caller passes the resolved name here. The constructor name (FieldName) is
// always the PLAIN name — it's a local group-package symbol, not a Components
// method, so it isn't collision-renamed. When mountOverride is nil or lacks an
// entry for a name, MountFieldName == FieldName (the no-collision common case
// and disk-less unit fixtures).
func cmdServiceItemsFromNames(module, bin string, names []string, mountOverride map[string]string) ([]CmdGroupItem, []string) {
	var items []CmdGroupItem
	var skipped []string
	for _, name := range names {
		runtime := naming.ToKebabCase(strings.TrimSuffix(name, "Service"))
		if runtime == "" {
			runtime = naming.ToKebabCase(name)
		}
		field := cmdSvcFieldName(name)
		mountField := field
		if mountOverride != nil {
			if override, ok := mountOverride[name]; ok {
				mountField = override
			}
		}
		if reservedSubcommandNames[runtime] {
			skipped = append(skipped, runtime)
			continue
		}
		items = append(items, CmdGroupItem{Module: module, Bin: bin, Name: runtime, FieldName: field, MountFieldName: mountField})
	}
	return items, skipped
}

// CmdServiceGroupInput drives GenerateCmdGroups: the primary binary name plus
// the SERVICE rows (the SAME proto-derived rows the app mount surface is
// generated from, so every service subcommand lines up with a typed
// (*app.Components).Mount<Svc>). Workers/operators are deliberately absent —
// generate performs no worker/operator discovery; their subcommands are
// scaffold-once OWNED code written by `forge add worker/operator`.
type CmdServiceGroupInput struct {
	Bin      string
	Services []string               // raw service-name spellings
	Packages []BootstrapPackageData // internal packages — for cross-role collision counts
}

// GenerateCmdGroups renders the dir-nested command groups under
// cmd/<bin>/cmd (devspace idiom): ONE FILE PER SERVICE in the services/
// SUBPACKAGE, each `New<X>Cmd(cmd.Deps)` whose RunE calls cmd.Serve() with a
// TYPED (*app.Components).Mount<Svc> selection. Each of the services/, workers/,
// and operators/ groups gets a register_gen.go anchor so the package compiles
// with zero items; the services anchor additionally carries the built-in
// collision NOTEs. Selection is compile-time typed — no string positional arg,
// no string→inventory lookup.
//
// It ALSO scaffolds cmd/<bin>/main.go — the composition root — ONCE
// (write-if-absent) from the service rows. main.go is OWNED code thereafter:
// `forge add worker/operator` appends the constructor ref by hand. The
// per-worker/operator subcommand files are NOT emitted here — they are
// scaffold-once OWNED code (ScaffoldWorkerCmd / ScaffoldOperatorCmd).
//
// cs is the project's checksum tracker — passing it keeps the (proto-derived)
// service files out of `forge audit`'s orphan list. A nil cs is tolerated.
func GenerateCmdGroups(in CmdServiceGroupInput, targetDir string, cs *checksums.FileChecksums) error {
	modulePath, err := GetModulePath(targetDir)
	if err != nil {
		return fmt.Errorf("read module path: %w", err)
	}

	groupDir := func(group string) string {
		return filepath.Join("cmd", in.Bin, "cmd", group)
	}

	// Derive the collision-aware MOUNT method name for each service from the
	// SAME single source GenerateInventory uses (ResolveCollisionNaming over
	// CollisionCounts), so the cmd-group command's typed mount call
	// ((*app.Components).Mount<MountFieldName>) always names the EXACT method
	// inventory_gen emitted. A service whose handler package collides
	// cross-role with an internal package (control-plane: handler
	// internal/handlers/billing `package billing` vs domain internal/billing
	// `package billing`) renames to MountSvcBilling in inventory_gen; without
	// this the cmd-group would emit MountBilling and fail to compile.
	//
	// Disk-resolve each service's handler package clause (same as inventory),
	// then count cross-role collisions over services + packages + workers +
	// operators. Resolution failures are non-fatal here: fall back to the
	// plain name (covers disk-less fixtures); a genuinely missing handler dir
	// surfaces as a build error downstream, not a silent miscompile.
	mountOverride := cmdServiceMountOverrides(targetDir, in)

	// services/ — one file per service + the anchor (with collision NOTEs).
	svcItems, skipped := cmdServiceItemsFromNames(modulePath, in.Bin, in.Services, mountOverride)
	for _, item := range svcItems {
		content, rerr := templates.ProjectTemplates().Render("cmd-svc-group.go.tmpl", item)
		if rerr != nil {
			return fmt.Errorf("render cmd-svc-group.go.tmpl (%s): %w", item.Name, rerr)
		}
		dest := filepath.Join(groupDir("services"), item.Name+".go")
		if werr := writeForgeOwned(targetDir, dest, content, cs); werr != nil {
			return fmt.Errorf("write %s: %w", dest, werr)
		}
	}
	svcAnchor, err := templates.ProjectTemplates().Render("cmd-svc-register.go.tmpl", CmdServicesTemplateData{Skipped: skipped})
	if err != nil {
		return fmt.Errorf("render cmd-svc-register.go.tmpl: %w", err)
	}
	if err := writeForgeOwned(targetDir, filepath.Join(groupDir("services"), "register_gen.go"), svcAnchor, cs); err != nil {
		return fmt.Errorf("write services/register_gen.go: %w", err)
	}

	// workers/ — one file per worker + the anchor.
	// NOTE: the per-worker subcommand files (workers/<name>.go) are NOT emitted
	// here. Each is scaffold-once OWNED code the `forge add worker` scaffold
	// writes exactly once (ScaffoldWorkerCmd) and then hand-wires into the owned
	// main.go / lifecycle.go. `forge generate` performs ZERO worker discovery, so
	// this pass only (re)writes the anchor that keeps the workers/ subpackage
	// compilable (and main.go's blank import resolvable) with zero workers.
	workerAnchor, err := templates.ProjectTemplates().Render("cmd-worker-register.go.tmpl", struct{}{})
	if err != nil {
		return fmt.Errorf("render cmd-worker-register.go.tmpl: %w", err)
	}
	if err := writeForgeOwned(targetDir, filepath.Join(groupDir("workers"), "register_gen.go"), workerAnchor, cs); err != nil {
		return fmt.Errorf("write workers/register_gen.go: %w", err)
	}

	// operators/ — anchor only (see the workers/ note above). Per-operator
	// subcommand files are scaffold-once OWNED code written by `forge add
	// operator` (ScaffoldOperatorCmd), never by `forge generate`.
	opAnchor, err := templates.ProjectTemplates().Render("cmd-operator-register.go.tmpl", struct{}{})
	if err != nil {
		return fmt.Errorf("render cmd-operator-register.go.tmpl: %w", err)
	}
	if err := writeForgeOwned(targetDir, filepath.Join(groupDir("operators"), "register_gen.go"), opAnchor, cs); err != nil {
		return fmt.Errorf("write operators/register_gen.go: %w", err)
	}

	// cmd/<bin>/main.go — the COMPOSITION ROOT, SCAFFOLD-ONCE owned code. It names
	// every group constructor explicitly and passes them to cmd.Execute (no
	// init() self-registration, no dynamic registry). Forge emits it ONCE with
	// the services known at scaffold time; thereafter it is hand-maintained
	// (`forge add worker/operator` appends the constructor ref). It carries NO
	// worker/operator refs on the initial emit — generate does no worker/operator
	// discovery — and an existing main.go is left untouched.
	if err := generateCmdMain(targetDir, modulePath, in.Bin, svcItems); err != nil {
		return err
	}

	return nil
}

// ScaffoldWorkerCmd writes the scaffold-once per-worker subcommand file
// cmd/<bin>/cmd/workers/<name>.go for a SINGLE worker, write-if-absent. This is
// the `forge add worker` counterpart to the retired generate-time worker loop:
// one new component, known by name, scaffolded once as OWNED code (the dev then
// hand-wires it into main.go / lifecycle.go / compose.go). Returns true when it
// wrote a fresh file (false when one already existed).
func ScaffoldWorkerCmd(targetDir, bin, name string) (bool, error) {
	return scaffoldComponentCmd(targetDir, bin, name, "workers", "cmd-worker-group.go.tmpl")
}

// ScaffoldOperatorCmd is the operator-side analog of ScaffoldWorkerCmd — writes
// cmd/<bin>/cmd/operators/<name>.go once for a single operator.
func ScaffoldOperatorCmd(targetDir, bin, name string) (bool, error) {
	return scaffoldComponentCmd(targetDir, bin, name, "operators", "cmd-operator-group.go.tmpl")
}

func scaffoldComponentCmd(targetDir, bin, name, group, tmpl string) (bool, error) {
	modulePath, err := GetModulePath(targetDir)
	if err != nil {
		return false, fmt.Errorf("read module path: %w", err)
	}
	fieldName := naming.ToPascalCase(name)
	item := CmdGroupItem{Module: modulePath, Bin: bin, Name: name, FieldName: fieldName, MountFieldName: fieldName}
	content, err := templates.ProjectTemplates().Render(tmpl, item)
	if err != nil {
		return false, fmt.Errorf("render %s (%s): %w", tmpl, name, err)
	}
	dest := filepath.Join("cmd", bin, "cmd", group, name+".go")
	return writeForgeScaffoldOnce(targetDir, dest, content)
}

// GenerateCmdMainRoot renders ONLY cmd/<bin>/main.go — the composition root —
// as a bare cmd.Execute() with no component constructors. The generate pipeline
// emits main.go via GenerateCmdGroups (which knows the full inventory); this
// exported entry lets the scaffold drop a composition root for a service
// project that has no proto pipeline (features.codegen=false), preserving the
// pre-refactor contract that main.go is always scaffolded for service kind.
func GenerateCmdMainRoot(targetDir, bin string, cs *checksums.FileChecksums) error {
	modulePath, err := GetModulePath(targetDir)
	if err != nil {
		return fmt.Errorf("read module path: %w", err)
	}
	return generateCmdMain(targetDir, modulePath, bin, nil)
}

// generateCmdMain renders cmd/<bin>/main.go (the SCAFFOLD-ONCE composition root)
// from the resolved service rows. It carries NO worker/operator refs — generate
// performs zero worker/operator discovery; those are hand-wired by `forge add`.
// The output is gofmt-canonicalized so the arg list collapses cleanly (an empty
// component set yields a bare `cmd.Execute()`). Written write-if-absent: forge
// emits it once, then never overwrites the owned file.
func generateCmdMain(targetDir, modulePath, bin string, svcItems []CmdGroupItem) error {
	data := CmdMainTemplateData{Module: modulePath, Bin: bin}
	for _, item := range svcItems {
		data.Services = append(data.Services, CmdCtorRef{Ctor: "New" + item.FieldName + "Cmd"})
	}

	rendered, err := templates.ProjectTemplates().Render("cmd-main.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render cmd-main.go.tmpl: %w", err)
	}
	formatted, err := format.Source(rendered)
	if err != nil {
		return fmt.Errorf("gofmt cmd/%s/main.go: %w\n%s", bin, err, rendered)
	}
	if _, err := writeForgeScaffoldOnce(targetDir, filepath.Join("cmd", bin, "main.go"), formatted); err != nil {
		return fmt.Errorf("write cmd/%s/main.go: %w", bin, err)
	}
	return nil
}

// cmdServiceMountOverrides returns, keyed by raw service-name spelling, the
// collision-aware exported suffix each service's typed mount METHOD carries in
// inventory_gen.go. It is the SINGLE shared derivation point between the
// cmd-group generator and inventory_gen: both feed the resolved handler
// package clause + the cross-role CollisionCounts into ResolveCollisionNaming,
// so the cmd-group's `(*app.Components).Mount<MountFieldName>` call can never
// name a method inventory_gen didn't emit.
//
// Only services whose name maps to a NON-plain field (i.e. a real collision)
// get an entry; the no-collision majority fall through to the plain
// ToPascalCase derivation in cmdServiceItemsFromNames (MountFieldName ==
// FieldName), keeping output byte-identical for collision-free projects.
// Disk-resolution failures are skipped (no entry) rather than fatal: disk-less
// unit fixtures and transient half-scaffolded trees fall back to the plain
// name, and a truly missing handler dir surfaces as a downstream build error.
func cmdServiceMountOverrides(targetDir string, in CmdServiceGroupInput) map[string]string {
	type resolvedSvc struct {
		name string // raw service-name spelling (map key)
		pkg  string // resolved handler package clause
	}
	resolved := make([]resolvedSvc, 0, len(in.Services))
	svcComponents := make([]BootstrapServiceData, 0, len(in.Services))
	for _, name := range in.Services {
		res, err := ResolveServiceComponent(targetDir, name)
		if err != nil {
			// Can't read the package clause — skip (plain-name fallback).
			continue
		}
		resolved = append(resolved, resolvedSvc{name: name, pkg: res.PackageName})
		svcComponents = append(svcComponents, BootstrapServiceData{Package: res.PackageName})
	}

	// Workers/operators are not counted: they carry no Mount<Svc> method, so
	// they never collide with a service's mount name — and GenerateInventory
	// (the source of the actual Mount methods) likewise no longer counts them,
	// so both stay consistent.
	counts := CollisionCounts(svcComponents, in.Packages, nil, nil)

	overrides := map[string]string{}
	for _, rs := range resolved {
		_, fieldName := ResolveCollisionNaming(rs.pkg, cmdSvcFieldName(rs.name), "svc", counts)
		// Record only genuine collision renames; the plain case is the
		// cmdServiceItemsFromNames default.
		if counts[rs.pkg] > 1 {
			overrides[rs.name] = fieldName
		}
	}
	if len(overrides) == 0 {
		return nil
	}
	return overrides
}
