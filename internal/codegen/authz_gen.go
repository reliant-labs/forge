package codegen

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/reliant-labs/forge/internal/checksums"
	"github.com/reliant-labs/forge/internal/naming"
	"github.com/reliant-labs/forge/internal/templates"
)

// AuthzMethodData holds per-method authorization metadata for the authorizer template.
type AuthzMethodData struct {
	Procedure     string   // full RPC procedure path, e.g. "/services.users.v1.UserService/CreateUser"
	RequiredRoles []string // roles that grant access (empty = any authenticated user)
	AuthRequired  bool     // whether auth is required for this method
	// Errors records the Connect/gRPC error codes the method may return,
	// derived from (forge.v1.method).errors. The template emits a
	// per-method entry in `methodErrors` so handler readers (including
	// LLMs) see the typed error contract alongside the role table.
	// Methods with no declared errors are omitted from the map.
	Errors []string
}

// AuthzTemplateData holds the data shape expected by authorizer_gen.go.tmpl.
type AuthzTemplateData struct {
	Package     string            // Go package name, e.g. "users"
	ServiceName string            // proto service name, e.g. "UserService"
	Module      string            // Go module path
	Methods     []AuthzMethodData // per-method authorization data
}

// GenerateAuthorizer generates authorizer_gen.go for each service whose
// handler directory exists. The generated file contains a methodRoles map
// and a role-checking CanAccess/Can implementation. It is always generated
// (even with zero annotated methods) so that the companion authorizer.go
// can unconditionally reference GeneratedAuthorizer without compilation
// errors.
//
// cs is the project's checksum tracker — passing it ensures every emitted
// authorizer_gen.go is recorded so `forge audit` doesn't flag it as an
// orphan. A nil cs is tolerated.
//
// skipDirs lists handlers/<dir> leaves the directory sweep below must NOT
// touch — the dirs of tombstoned (types-only) services: services that
// pkg/app/services.go deliberately does not register (the row was
// deleted, a comment names the serving binary). Their services never
// appear in the (already row-filtered) services slice, so without the
// skip the sweep would misread a retired handler dir as an orphaned
// scaffold and re-emit authorizer_gen.go into it, re-adding the path to
// WrittenThisRun and hiding it from the stale-cleanup sweep. Keys are
// the snake package form (naming.ServicePackage); nil means no
// types-only services.
func GenerateAuthorizer(services []ServiceDef, modulePath string, targetDir string, skipDirs map[string]bool, cs *checksums.FileChecksums) error {
	// generatedDirs records the handlers/<dir> leaves covered by the
	// ServiceDef pass so the directory sweep below doesn't double-emit.
	// Keyed by the ON-DISK directory name (not the synthesized package)
	// because the sweep iterates real directory entries.
	generatedDirs := make(map[string]bool, len(services))
	for _, svc := range services {
		// Disk-first: authorizer_gen.go lands INSIDE the existing handler
		// dir and must carry that dir's REAL package clause — synthesizing
		// either from the proto name would (a) skip generation entirely
		// when the dir uses a snake_case spelling the synthesis misses, or
		// (b) stamp a conflicting `package x` clause into a dir that
		// declares something else. See disk_resolver.go.
		res, err := ResolveServiceComponent(targetDir, svc.Name)
		if err != nil {
			return err
		}
		// Only generate if the service directory exists (was scaffolded).
		if !res.FromDisk {
			continue
		}
		pkg := res.PackageName

		var methods []AuthzMethodData
		for _, m := range svc.Methods {
			methods = append(methods, AuthzMethodData{
				Procedure:     fmt.Sprintf("/%s.%s/%s", svc.Package, svc.Name, m.Name),
				RequiredRoles: m.RequiredRoles,
				AuthRequired:  m.AuthRequired,
				Errors:        m.Errors,
			})
		}

		data := AuthzTemplateData{
			Package:     pkg,
			ServiceName: svc.Name,
			Module:      modulePath,
			Methods:     methods,
		}

		content, err := templates.ServiceTemplates().Render("authorizer_gen.go.tmpl", data)
		if err != nil {
			return fmt.Errorf("render authorizer_gen.go.tmpl for %s: %w", svc.Name, err)
		}

		// Land the file in the dir that actually exists (res.ImportLeaf),
		// not the synthesized package name — writing to a synthesized path
		// here is how the historical handlers/adminserver-vs-admin_server
		// duplicate-dir bug was born.
		relPath := filepath.Join("handlers", filepath.FromSlash(res.ImportLeaf), "authorizer_gen.go")
		if _, err := checksums.WriteGeneratedFile(targetDir, relPath, content, cs, true); err != nil {
			return fmt.Errorf("write authorizer_gen.go for %s: %w", svc.Name, err)
		}
		generatedDirs[res.ImportLeaf] = true
	}

	// Also generate authorizer_gen.go for service directories that exist but
	// have no corresponding ServiceDef (e.g., scaffold created the handler
	// dir before any RPCs were defined in the proto). This ensures
	// authorizer.go can always reference GeneratedAuthorizer.
	handlersDir := filepath.Join(targetDir, "handlers")
	entries, err := os.ReadDir(handlersDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read handlers dir: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dirName := entry.Name()
		if generatedDirs[dirName] {
			continue
		}
		// Types-only (tombstoned in pkg/app/services.go) services: leave
		// their retired handler dirs alone so the stale-cleanup sweep can
		// flag the tracked files.
		if skipDirs[dirName] || skipDirs[naming.ServicePackage(dirName)] {
			continue
		}
		// Canonical handler dirs are snake_case Go identifiers (matching
		// `naming.GoPackage` output). A name that doesn't round-trip
		// through GoPackage is a legacy compact/PascalCase dir left
		// behind by a pre-2026-06-08 scaffold (e.g. handlers/adminserver
		// from the brief compact-form interlude) — skip it so cleanup /
		// dangling-check can surface the stale dir for removal instead
		// of emitting an authorizer with a mismatched package.
		if dirName != naming.GoPackage(dirName) {
			continue
		}
		// Only generate if authorizer.go exists (confirming this is a service dir)
		if _, err := os.Stat(filepath.Join(handlersDir, dirName, "authorizer.go")); os.IsNotExist(err) {
			continue
		}

		// Disk-first: the emitted file must declare the SAME package as
		// the rest of the directory (the dir name and the package clause
		// may legally differ — e.g. handlers/admin_server declaring
		// `package adminserver`). authorizer.go exists here, so a clause
		// must be parseable; a conflicting/broken clause is a real user
		// error worth failing on.
		pkg, perr := ParsePackageClause(filepath.Join(handlersDir, dirName))
		if perr != nil {
			return fmt.Errorf("generating authorizer_gen.go for handlers/%s: %w", dirName, perr)
		}

		data := AuthzTemplateData{
			Package:     pkg,
			ServiceName: naming.ToPascalCase(dirName) + "Service",
			Module:      modulePath,
			Methods:     nil,
		}

		content, err := templates.ServiceTemplates().Render("authorizer_gen.go.tmpl", data)
		if err != nil {
			return fmt.Errorf("render authorizer_gen.go.tmpl for %s: %w", pkg, err)
		}

		relPath := filepath.Join("handlers", dirName, "authorizer_gen.go")
		if _, err := checksums.WriteGeneratedFile(targetDir, relPath, content, cs, true); err != nil {
			return fmt.Errorf("write authorizer_gen.go for %s: %w", pkg, err)
		}
	}

	return nil
}
