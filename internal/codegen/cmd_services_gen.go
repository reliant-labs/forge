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
	// command constructor (New<FieldName>Cmd) AND the typed mount/list
	// row ((*app.Services).Mount<FieldName> / s.<FieldName>) — mirrors the
	// app.Services.<X> field spelling so the families grep together.
	FieldName string
}

// CmdServicesTemplateData feeds cmd-svc-register.go.tmpl (the services group
// anchor + collision NOTEs).
type CmdServicesTemplateData struct {
	Skipped []string // runtime names skipped for colliding with built-ins
}

// cmdServiceItemsFromNames projects raw service names (any spelling: proto
// "AdminServerService", forge.yaml "admin-server") onto group-item rows,
// filtering reserved collisions into the skipped list. The kebab/Pascal
// derivation is identical to GenerateInventory's so the subcommand Name
// always equals the inventory row Name and the typed mount method it selects.
func cmdServiceItemsFromNames(module, bin string, names []string) ([]CmdGroupItem, []string) {
	var items []CmdGroupItem
	var skipped []string
	for _, name := range names {
		runtime := naming.ToKebabCase(strings.TrimSuffix(name, "Service"))
		if runtime == "" {
			runtime = naming.ToKebabCase(name)
		}
		field := naming.ToPascalCase(strings.TrimSuffix(name, "Service"))
		if field == "" {
			field = naming.ToPascalCase(name)
		}
		if reservedSubcommandNames[runtime] {
			skipped = append(skipped, runtime)
			continue
		}
		items = append(items, CmdGroupItem{Module: module, Bin: bin, Name: runtime, FieldName: field})
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

	// services/ — one file per service + the anchor (with collision NOTEs).
	svcItems, skipped := cmdServiceItemsFromNames(modulePath, in.Bin, in.Services)
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
		item := CmdGroupItem{Module: modulePath, Bin: in.Bin, Name: w.Name, FieldName: w.FieldName}
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
		item := CmdGroupItem{Module: modulePath, Bin: in.Bin, Name: op.Name, FieldName: op.FieldName}
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
