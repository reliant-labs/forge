package codegen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/naming"
	"github.com/reliant-labs/forge/internal/templates"
)

// reservedSubcommandNames are top-level cobra Use values the generated
// cmd/ tree already claims (cmd/server.go, cmd/version.go, cmd/db.go,
// cobra built-ins). A registered service whose runtime name collides is
// skipped — with a NOTE in the generated file — instead of emitting a
// subcommand that would shadow (or be shadowed by) the built-in.
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
	// AND the appkit row name runServer's filter matches against. Same
	// derivation as the bootstrap row Name (GenerateBootstrap).
	Name string

	// FieldName is the exported PascalCase suffix used in the generated
	// var name (serviceCmd<FieldName>) — mirrors the serviceRow<X> and
	// app.Services.<X> spelling so the three families grep together.
	FieldName string
}

// CmdServicesTemplateData feeds cmd-services-gen.go.tmpl.
type CmdServicesTemplateData struct {
	Services []CmdServiceSubcommand
	Skipped  []string // runtime names skipped for colliding with built-ins
}

// CmdServiceSubcommandsFromNames projects raw service names (any
// spelling: proto "AdminServerService", forge.yaml "admin-server") onto
// the subcommand rows, filtering reserved collisions into Skipped.
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

// GenerateCmdServices renders cmd/services_gen.go: one cobra subcommand
// per REGISTERED service — the cmd-side projection of the same
// registration table (pkg/app/services.go rows) that bootstrap
// consumes. Tier-1: regenerated every `forge generate`, so adding or
// tombstoning a row updates the binary's subcommand surface on the next
// run.
//
// services must already be the REGISTERED set (the caller filters via
// the services.go registry parse); names are svc.Name spellings.
//
// cs is the project's checksum tracker — passing it keeps the file out
// of `forge audit`'s orphan list. A nil cs is tolerated.
func GenerateCmdServices(serviceNames []string, targetDir string, cs *checksums.FileChecksums) error {
	data := CmdServiceSubcommandsFromNames(serviceNames)

	content, err := templates.ProjectTemplates().Render("cmd-services-gen.go.tmpl", data)
	if err != nil {
		return fmt.Errorf("render cmd-services-gen.go.tmpl: %w", err)
	}

	if _, err := checksums.WriteGeneratedFile(targetDir, filepath.Join("cmd", "services_gen.go"), content, cs, true); err != nil {
		return fmt.Errorf("write cmd/services_gen.go: %w", err)
	}
	return nil
}

// GenerateCmdCommands scaffolds cmd/commands.go — the user-owned cobra
// extension point the generated cmd/main.go consumes (userCommands()).
// Written ONCE; never overwritten (Tier-2: the user owns the file the
// moment it exists). Second binaries register here as code with opt-in
// serverkit pieces instead of a parallel hand-rolled main().
func GenerateCmdCommands(targetDir string) error {
	cmdDir := filepath.Join(targetDir, "cmd")
	dest := filepath.Join(cmdDir, "commands.go")

	// Never overwrite — this is user-owned code.
	if _, err := os.Stat(dest); err == nil {
		return nil
	}

	if err := os.MkdirAll(cmdDir, 0755); err != nil {
		return err
	}

	content, err := templates.ProjectTemplates().Render("cmd-commands.go.tmpl", struct{}{})
	if err != nil {
		return fmt.Errorf("render cmd-commands.go.tmpl: %w", err)
	}

	return os.WriteFile(dest, content, 0644)
}
