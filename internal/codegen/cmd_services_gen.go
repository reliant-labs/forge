package codegen

import (
	"fmt"
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
	// row Name and the typed Mount<Svc> / WorkerList / OperatorList rows.
	Name string

	// FieldName is the exported PascalCase suffix used in the generated
	// command constructor (New<FieldName>Cmd) and self-registration. It is
	// the PLAIN per-role name (ToPascalCase of the trimmed service name) —
	// the constructor is a local symbol in the group package, so it is not
	// subject to the cross-role collision rename. Workers/operators set
	// FieldName == MountFieldName (no Services-field mount, no collision).
	FieldName string

	// MountFieldName is the exported suffix of the typed mount METHOD the
	// service command calls: (*app.Services).Mount<MountFieldName>. For
	// services it MUST equal the name inventory_gen emitted — which is
	// collision-aware (ResolveCollisionNaming), so a service whose handler
	// package collides cross-role with an internal package mounts as
	// MountSvc<Pkg>, not Mount<Pkg>. Keeping this SEPARATE from FieldName lets
	// the constructor stay New<Plain>Cmd while the mount call names the
	// collision-renamed Services method — matching inventory_gen exactly and
	// killing the MountBilling/MountSvcBilling mismatch (BUG 1). For
	// workers/operators it is set equal to FieldName (their template doesn't
	// reference a Services mount method).
	MountFieldName string
}

// CmdServicesTemplateData feeds cmd-svc-register.go.tmpl (the services group
// anchor + collision NOTEs).
type CmdServicesTemplateData struct {
	Skipped []string // runtime names skipped for colliding with built-ins
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
// cmd-group command's typed mount call ((*app.Services).Mount<MountFieldName>)
// MUST match the method inventory_gen emitted, so when a collision exists the
// caller passes the resolved name here. The constructor name (FieldName) is
// always the PLAIN name — it's a local group-package symbol, not a Services
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
// the service / worker / operator rows (the SAME rows the app composition
// layer is generated from, so every subcommand lines up with a typed mount /
// WorkerList / OperatorList entry).
type CmdServiceGroupInput struct {
	Bin       string
	Services  []string                // raw service-name spellings
	Packages  []BootstrapPackageData  // internal packages — for cross-role collision counts
	Workers   []BootstrapWorkerData   // Name + FieldName
	Operators []BootstrapOperatorData // Name + FieldName
}

// GenerateCmdGroups renders the dir-nested command groups under
// cmd/<bin>/cmd (devspace idiom): ONE FILE PER ITEM in the services/,
// workers/, and operators/ SUBPACKAGES, each `New<X>Cmd(cmd.Deps)` whose
// RunE calls cmd.Serve() with a TYPED selection (the (*app.Services).Mount<Svc>
// method expression for services; cmd.MountNone + a named supervised subset
// for workers/operators). Each group also gets a register_gen.go anchor so
// the package compiles (and main.go's blank import resolves) with zero items;
// the services anchor additionally carries the built-in collision NOTEs.
// Selection is compile-time typed — no string positional arg, no
// string→inventory lookup. service rows must be the same set the app
// Inventory is generated from so each name lines up with a typed mount.
//
// cs is the project's checksum tracker — passing it keeps the files out of
// `forge audit`'s orphan list. A nil cs is tolerated.
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
	// ((*app.Services).Mount<MountFieldName>) always names the EXACT method
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
	for _, w := range in.Workers {
		item := CmdGroupItem{Module: modulePath, Bin: in.Bin, Name: w.Name, FieldName: w.FieldName, MountFieldName: w.FieldName}
		content, rerr := templates.ProjectTemplates().Render("cmd-worker-group.go.tmpl", item)
		if rerr != nil {
			return fmt.Errorf("render cmd-worker-group.go.tmpl (%s): %w", item.Name, rerr)
		}
		dest := filepath.Join(groupDir("workers"), item.Name+".go")
		if werr := writeForgeOwned(targetDir, dest, content, cs); werr != nil {
			return fmt.Errorf("write %s: %w", dest, werr)
		}
	}
	workerAnchor, err := templates.ProjectTemplates().Render("cmd-worker-register.go.tmpl", struct{}{})
	if err != nil {
		return fmt.Errorf("render cmd-worker-register.go.tmpl: %w", err)
	}
	if err := writeForgeOwned(targetDir, filepath.Join(groupDir("workers"), "register_gen.go"), workerAnchor, cs); err != nil {
		return fmt.Errorf("write workers/register_gen.go: %w", err)
	}

	// operators/ — one file per operator + the anchor.
	for _, op := range in.Operators {
		item := CmdGroupItem{Module: modulePath, Bin: in.Bin, Name: op.Name, FieldName: op.FieldName, MountFieldName: op.FieldName}
		content, rerr := templates.ProjectTemplates().Render("cmd-operator-group.go.tmpl", item)
		if rerr != nil {
			return fmt.Errorf("render cmd-operator-group.go.tmpl (%s): %w", item.Name, rerr)
		}
		dest := filepath.Join(groupDir("operators"), item.Name+".go")
		if werr := writeForgeOwned(targetDir, dest, content, cs); werr != nil {
			return fmt.Errorf("write %s: %w", dest, werr)
		}
	}
	opAnchor, err := templates.ProjectTemplates().Render("cmd-operator-register.go.tmpl", struct{}{})
	if err != nil {
		return fmt.Errorf("render cmd-operator-register.go.tmpl: %w", err)
	}
	if err := writeForgeOwned(targetDir, filepath.Join(groupDir("operators"), "register_gen.go"), opAnchor, cs); err != nil {
		return fmt.Errorf("write operators/register_gen.go: %w", err)
	}

	return nil
}

// cmdServiceMountOverrides returns, keyed by raw service-name spelling, the
// collision-aware exported suffix each service's typed mount METHOD carries in
// inventory_gen.go. It is the SINGLE shared derivation point between the
// cmd-group generator and inventory_gen: both feed the resolved handler
// package clause + the cross-role CollisionCounts into ResolveCollisionNaming,
// so the cmd-group's `(*app.Services).Mount<MountFieldName>` call can never
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

	counts := CollisionCounts(svcComponents, in.Packages, in.Workers, in.Operators)

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
