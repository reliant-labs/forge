package codegen

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/naming"
	"github.com/reliant-labs/forge/internal/templates"
)

// reservedSubcommandNames are top-level cobra Use values the generated cli
// tree already claims (server / version / db, cobra built-ins). A service
// whose runtime name collides is SKIPPED — with a NOTE in the generated
// svc_register_gen.go — instead of emitting a subcommand that would shadow
// (or be shadowed by) the built-in. The colliding service is still
// reachable via `server <name>` (the typed MountByName convenience).
var reservedSubcommandNames = map[string]bool{
	"server":     true,
	"version":    true,
	"db":         true,
	"help":       true,
	"completion": true,
}

// CmdServiceSubcommand is one generated per-service subcommand row — one
// internal/cli/svc_<name>.go file plus a row in addServiceCmds.
type CmdServiceSubcommand struct {
	// Module is the project module path (for the import lines in each
	// generated svc_<name>.go).
	Module string

	// Name is the runtime kebab-case service name — the cobra Use value and
	// the svc_<name>.go filename stem. Identical derivation to the
	// app.Inventory row Name and the typed Mount<Svc> method.
	Name string

	// FieldName is the exported PascalCase suffix used in the generated
	// command constructor (new<FieldName>Cmd) AND the typed mount method
	// expression ((*app.Services).Mount<FieldName>) — mirrors the
	// app.Services.<X> field spelling so the families grep together.
	FieldName string
}

// CmdServicesTemplateData feeds cli-svc-register.go.tmpl (the addServiceCmds
// roster + collision NOTEs).
type CmdServicesTemplateData struct {
	Services []CmdServiceSubcommand
	Skipped  []string // runtime names skipped for colliding with built-ins
}

// CmdServiceSubcommandsFromNames projects raw service names (any spelling:
// proto "AdminServerService", forge.yaml "admin-server") onto the
// subcommand rows, filtering reserved collisions into Skipped. The
// kebab/Pascal derivation is identical to GenerateInventory's so the
// subcommand Name always equals the inventory row Name and the typed mount
// method it selects.
func CmdServiceSubcommandsFromNames(module string, names []string) CmdServicesTemplateData {
	var data CmdServicesTemplateData
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
			data.Skipped = append(data.Skipped, runtime)
			continue
		}
		data.Services = append(data.Services, CmdServiceSubcommand{Module: module, Name: runtime, FieldName: field})
	}
	return data
}

// GenerateCmdServices renders the per-service cobra subcommands into the
// internal/cli package: ONE FILE PER SERVICE (internal/cli/svc_<name>.go,
// each `new<Svc>Cmd(deps)` whose RunE calls serve() with the TYPED mount
// method expression (*app.Services).Mount<Svc>), plus the
// internal/cli/svc_register_gen.go roster (addServiceCmds + collision
// NOTEs). Selection is compile-time typed — no string positional arg, no
// string→inventory lookup. serviceNames must be the same row set the
// app.Inventory is generated from so each name lines up with a typed mount.
//
// cs is the project's checksum tracker — passing it keeps the files out of
// `forge audit`'s orphan list. A nil cs is tolerated.
func GenerateCmdServices(serviceNames []string, targetDir string, cs *checksums.FileChecksums) error {
	modulePath, err := GetModulePath(targetDir)
	if err != nil {
		return fmt.Errorf("read module path: %w", err)
	}

	data := CmdServiceSubcommandsFromNames(modulePath, serviceNames)

	// One file per service (gh / cobra-cli idiom).
	for _, svc := range data.Services {
		content, rerr := templates.ProjectTemplates().Render("cli-svc.go.tmpl", svc)
		if rerr != nil {
			return fmt.Errorf("render cli-svc.go.tmpl (%s): %w", svc.Name, rerr)
		}
		dest := filepath.Join("internal", "cli", "svc_"+svc.Name+".go")
		if werr := writeForgeOwned(targetDir, dest, content, cs); werr != nil {
			return fmt.Errorf("write %s: %w", dest, werr)
		}
	}

	// The registration roster: addServiceCmds + collision NOTEs.
	regContent, err := templates.ProjectTemplates().Render("cli-svc-register.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render cli-svc-register.go.tmpl: %w", err)
	}
	if err := writeForgeOwned(targetDir, filepath.Join("internal", "cli", "svc_register_gen.go"), regContent, cs); err != nil {
		return fmt.Errorf("write internal/cli/svc_register_gen.go: %w", err)
	}
	return nil
}
