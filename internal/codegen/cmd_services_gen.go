package codegen

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/naming"
	"github.com/reliant-labs/forge/internal/templates"
)

// reservedSubcommandNames are top-level cobra Use values the generated
// cmd/ tree already claims (cmd/server.go, cmd/version.go, cmd/db.go,
// cobra built-ins). A service whose runtime name collides is skipped —
// with a NOTE in the generated file — instead of emitting a subcommand
// that would shadow (or be shadowed by) the built-in. The colliding
// service is still reachable via `server <name>`.
var reservedSubcommandNames = map[string]bool{
	"server":     true,
	"version":    true,
	"db":         true,
	"help":       true,
	"completion": true,
}

// CmdServiceSubcommand is one generated per-service subcommand row in
// cmd/services_gen.go.
type CmdServiceSubcommand struct {
	// Name is the runtime kebab-case service name — the cobra Use value
	// AND the literal selection key baked into the subcommand's RunE
	// (passed to runServer as a single-element slice). Identical
	// derivation to the app.Inventory row Name, so the subcommand selects
	// exactly that inventory row at mount time.
	Name string

	// FieldName is the exported PascalCase suffix used in the generated
	// var name (serviceCmd<FieldName>) — mirrors the app.Services.<X>
	// field spelling so the two families grep together.
	FieldName string
}

// CmdServicesTemplateData feeds cmd-services-gen.go.tmpl.
type CmdServicesTemplateData struct {
	Services []CmdServiceSubcommand
	Skipped  []string // runtime names skipped for colliding with built-ins
}

// CmdServiceSubcommandsFromNames projects raw service names (any
// spelling: proto "AdminServerService", forge.yaml "admin-server") onto
// the subcommand rows, filtering reserved collisions into Skipped. The
// kebab/Pascal derivation is identical to GenerateInventory's so the
// subcommand Name always equals the inventory row Name it selects.
func CmdServiceSubcommandsFromNames(names []string) CmdServicesTemplateData {
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
		data.Services = append(data.Services, CmdServiceSubcommand{Name: runtime, FieldName: field})
	}
	return data
}

// GenerateCmdServices renders cmd/services_gen.go: one REAL cobra
// subcommand per service — `./<bin> <service>` is its own command with
// its own `-h`, Short/Long help, and identity. Each subcommand boots the
// canonical server pipeline (cmd/server.go runServer) with its own name
// pre-filled as the single mount-selection key, so it mounts ONLY that
// service over the data-only app.Inventory. The multi-service form
// `./<bin> server [services...]` keeps working unchanged.
//
// Selection is by the subcommand's OWN identity — the kebab name is a
// compile-time constant baked into the generated RunE, never a runtime
// positional arg. serviceNames must be the row set the app.Inventory is
// generated from, so each subcommand name lines up with an inventory row.
//
// cs is the project's checksum tracker — passing it keeps the file out
// of `forge audit`'s orphan list. A nil cs is tolerated.
func GenerateCmdServices(serviceNames []string, targetDir string, cs *checksums.FileChecksums) error {
	data := CmdServiceSubcommandsFromNames(serviceNames)

	content, err := templates.ProjectTemplates().Render("cmd-services-gen.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render cmd-services-gen.go.tmpl: %w", err)
	}

	if err := writeForgeOwned(targetDir, filepath.Join("cmd", "services_gen.go"), content, cs); err != nil {
		return fmt.Errorf("write cmd/services_gen.go: %w", err)
	}
	return nil
}
