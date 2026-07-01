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

// AuthzMethodData holds per-method authorization metadata for the authorizer template.
type AuthzMethodData struct {
	Procedure     string   // full RPC procedure path, e.g. "/services.users.v1.UserService/CreateUser"
	RequiredRoles []string // roles that grant access (empty = any authenticated user)
	AuthRequired  bool     // whether auth is required for this method
	// AuthzCustom marks a method whose authorization is delegated to a
	// hand-written authorizer ((forge.v1.method).authz_custom). It carries no
	// role allow-list, so the template must NOT emit it with empty roles (that
	// reads as an any-authenticated grant). Instead the template emits it
	// FAIL-CLOSED — a sentinel "custom — see interceptor" role that no caller
	// holds — so the generated table can't be misread as a grant. The real
	// decision is enforced by the descriptor-driven RoleInterceptor + the
	// service's authorizer.go, never this table.
	AuthzCustom bool
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

// BuildAuthzMethods converts a service's RPC methods into the
// authorizer-table entries the template emits. It returns BOTH key
// spellings of the policy universe:
//
//   - one entry per RPC keyed by the Connect procedure path (checked by
//     CanAccess from the auth middleware), and
//   - one alias entry per CRUD-matched RPC keyed by
//     "<action>:<resource>" (e.g. "create:patient") — the exact keys the
//     generated CRUD handler bodies pass to Can. The alias carries the
//     same required-roles/auth-required flags as its underlying RPC.
//
// The aliases are derived from MatchCRUDMethods — the SAME extraction
// crud_gen uses to emit the Can() call sites — so every generated Can
// key exists in the generated table by construction. VerifyCanKeyUniverse
// re-checks that invariant independently at generate time.
func BuildAuthzMethods(svc ServiceDef, entities []EntityDef) []AuthzMethodData {
	var methods []AuthzMethodData
	seen := make(map[string]bool, len(svc.Methods))
	for _, m := range svc.Methods {
		methods = append(methods, AuthzMethodData{
			Procedure:     fmt.Sprintf("/%s.%s/%s", svc.Package, svc.Name, m.Name),
			RequiredRoles: m.RequiredRoles,
			AuthRequired:  m.AuthRequired,
			AuthzCustom:   m.AuthzCustom,
			Errors:        m.Errors,
		})
	}
	for _, cm := range MatchCRUDMethods(svc, entities) {
		key := operationToAuthAction(cm.Operation) + ":" + strings.ToLower(cm.Entity.Name)
		if seen[key] {
			continue
		}
		seen[key] = true
		// Look the source RPC back up to carry its roles/auth flags onto
		// the alias — MatchCRUDMethods returns a reduced shape without
		// RequiredRoles.
		for _, m := range svc.Methods {
			if m.Name != cm.Method.Name {
				continue
			}
			methods = append(methods, AuthzMethodData{
				Procedure:     key,
				RequiredRoles: m.RequiredRoles,
				AuthRequired:  m.AuthRequired,
				AuthzCustom:   m.AuthzCustom,
			})
			break
		}
	}
	return methods
}

// VerifyCanKeyUniverse asserts that every "<action>:<resource>" key a
// generated CRUD handler will pass to Authorizer.Can exists in the
// emitted authorizer table. The check recomputes the Can-key set from
// MatchCRUDMethods (the source of the generated call sites) and compares
// against the table entries, so any future drift between the two
// extractions fails `forge generate` loudly instead of shipping an
// authorizer that warns-and-denies every CRUD request forever.
func VerifyCanKeyUniverse(svc ServiceDef, entities []EntityDef, methods []AuthzMethodData) error {
	emitted := make(map[string]bool, len(methods))
	for _, m := range methods {
		emitted[m.Procedure] = true
	}
	var missing []string
	for _, cm := range MatchCRUDMethods(svc, entities) {
		key := operationToAuthAction(cm.Operation) + ":" + strings.ToLower(cm.Entity.Name)
		if !emitted[key] {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("authorizer table for %s is missing Can() keys used by generated CRUD handlers: %v (key-universe drift between crud_gen and authz_gen — this is a forge bug)", svc.Name, missing)
	}
	return nil
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
	// Entities feed the CRUD alias keys ("create:patient") that the
	// generated CRUD handler bodies pass to Can. A missing/unreadable
	// descriptor means crud_gen emits no CRUD bodies either, so an empty
	// entity set keeps the two key universes consistent.
	entities, err := ParseEntityProtos(targetDir)
	if err != nil {
		entities = nil
	}

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

		methods := BuildAuthzMethods(svc, entities)
		// Generate-time invariant: every Can() key the CRUD handlers use
		// must exist in the emitted table. Drift here means steady-state
		// per-request denials for every CRUD RPC — fail the generate.
		if verr := VerifyCanKeyUniverse(svc, entities, methods); verr != nil {
			return verr
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
		relPath := filepath.Join("internal", "handlers", filepath.FromSlash(res.ImportLeaf), "authorizer_gen.go")
		if err := writeForgeOwned(targetDir, relPath, content, cs); err != nil {
			return fmt.Errorf("write authorizer_gen.go for %s: %w", svc.Name, err)
		}
		generatedDirs[res.ImportLeaf] = true
	}

	// Also generate authorizer_gen.go for service directories that exist but
	// have no corresponding ServiceDef (e.g., scaffold created the handler
	// dir before any RPCs were defined in the proto). This ensures
	// authorizer.go can always reference GeneratedAuthorizer.
	handlersDir := filepath.Join(targetDir, "internal", "handlers")
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

		relPath := filepath.Join("internal", "handlers", dirName, "authorizer_gen.go")
		if err := writeForgeOwned(targetDir, relPath, content, cs); err != nil {
			return fmt.Errorf("write authorizer_gen.go for %s: %w", pkg, err)
		}
	}

	return nil
}
