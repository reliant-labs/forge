package codegen

import (
	"go/parser"
	"go/token"
	"path"
	"strconv"
	"strings"

	"github.com/reliant-labs/forge/internal/config"
)

// ProjectORMPackage is the ORM placement projectDir's forge.yaml declares
// (database.orm.package), the default internal/db when it declares none or
// cannot be read. Best-effort by design, like projectAPIRESTEnabled: an
// unreadable forge.yaml has already been reported by the load that matters.
func ProjectORMPackage(projectDir string) config.ORMConfig {
	cfg, err := config.LoadProjectDir(projectDir)
	if err != nil {
		return config.ORMConfig{}
	}
	return config.ORMPackage(cfg)
}

// ORMImportSpec is the generated ORM package as an import spec for code
// that qualifies it as `db.`: the import path, or "db <path>" when the
// package is not called db. The spec format ("[alias ]path") is the one
// crudShimImports and ensureImportLine already speak.
//
// Generated CRUD code no longer assumes `db.`: it binds the ORM with
// ChooseORMBinding, which avoids the names the file already binds.
func ORMImportSpec(modulePath string, orm config.ORMConfig) string {
	path := modulePath + "/" + orm.PackageDir()
	if orm.PackageName() == "db" {
		return path
	}
	return "db " + path
}

// ORMBinding is how one Go file imports the generated ORM package and
// qualifies its identifiers.
type ORMBinding struct {
	// Spec is the import in ensureImportLine's form: the path, or
	// "alias path" when Qual is not the package's own name.
	Spec string
	// Qual is the identifier the file's code qualifies the ORM with.
	Qual string
}

// ormQualFallback is the alias the ORM is imported under when neither its
// own name nor the legacy db is free in a file; ormstore2, ormstore3, …
// follow it.
const ormQualFallback = "ormstore"

// ormQualReserved are the identifiers the generated CRUD bodies declare as
// locals and parameters (handlers_crud_ops_gen.go.tmpl,
// handlers_crud_shim_method.go.tmpl). A package qualifier spelled like one
// would be shadowed inside the body that uses it.
var ormQualReserved = map[string]bool{
	"build": true, "c": true, "claims": true, "ctx": true, "del": true, "e": true,
	"entity": true, "err": true, "fetch": true, "fields": true, "filters": true,
	"id": true, "it": true, "items": true, "m": true, "nextPageToken": true,
	"op": true, "opts": true, "out": true, "owner": true, "persist": true,
	"persistMasked": true, "r": true, "req": true, "resp": true, "row": true,
	"rows": true, "s": true, "totalCount": true,
}

// ChooseORMBinding binds the ORM package in a file where taken reports
// the identifiers already bound: the file's other imports and the
// package's top-level declarations. The first free name of the ORM's own
// package name, the legacy db alias, then ormstore (ormstore2, …) wins, so
// a default project (package db, nothing else named db) is imported
// unaliased and qualified as db. exactly as before, and a relocated ORM
// (internal/store) is imported unaliased and qualified as store. — never as
// a db that a project's own internal/db already holds.
func ChooseORMBinding(modulePath string, orm config.ORMConfig, taken func(string) bool) ORMBinding {
	importPath := modulePath + "/" + orm.PackageDir()
	free := func(name string) bool { return !taken(name) && !ormQualReserved[name] }
	name := orm.PackageName()
	if free(name) {
		return ORMBinding{Spec: importPath, Qual: name}
	}
	alias := ""
	for _, cand := range []string{"db", ormQualFallback} {
		if free(cand) {
			alias = cand
			break
		}
	}
	for n := 2; alias == ""; n++ {
		if cand := ormQualFallback + strconv.Itoa(n); free(cand) {
			alias = cand
		}
	}
	return ORMBinding{Spec: alias + " " + importPath, Qual: alias}
}

// importSpecName is the identifier an import spec ("path" or "alias path")
// binds. An unrenamed import is assumed to bind its path's last element,
// minus a /vN major-version suffix (fileImportSpecs' rule).
func importSpecName(spec string) string {
	if alias, _, ok := strings.Cut(spec, " "); ok {
		return alias
	}
	base := path.Base(spec)
	if len(base) > 1 && base[0] == 'v' && strings.Trim(base[1:], "0123456789") == "" {
		base = path.Base(path.Dir(spec))
	}
	return base
}

// importSpecPath is the path of an import spec ("path" or "alias path").
func importSpecPath(spec string) string {
	if _, p, ok := strings.Cut(spec, " "); ok {
		return p
	}
	return spec
}

// importBinding is one name an import binds, and its path.
type importBinding struct{ name, path string }

// fileImports lists the identifiers src's imports bind, in source order.
// Only the import block is parsed, so a file whose body does not compile
// still reports its imports. Blank and dot imports bind nothing.
func fileImports(src []byte) []importBinding {
	if len(src) == 0 {
		return nil
	}
	f, err := parser.ParseFile(token.NewFileSet(), "", src, parser.ImportsOnly)
	if err != nil {
		return nil
	}
	var out []importBinding
	for _, imp := range f.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		switch {
		case imp.Name == nil:
			out = append(out, importBinding{importSpecName(p), p})
		case imp.Name.Name != "_" && imp.Name.Name != ".":
			out = append(out, importBinding{imp.Name.Name, p})
		}
	}
	return out
}

// fileImportNames reports whether src's imports bind a name.
func fileImportNames(src []byte) func(string) bool {
	bound := map[string]bool{}
	for _, b := range fileImports(src) {
		bound[b.name] = true
	}
	return func(name string) bool { return bound[name] }
}

// opsFileImportNames are the names handlers_crud_ops_gen.go.tmpl's other
// imports bind; the ORM's qualifier in that file must be none of them.
var opsFileImportNames = map[string]bool{
	"context": true, "crud": true, "fmt": true, "orm": true, "pb": true,
	"time": true, "timestamppb": true,
}

// shimORMBinding binds the ORM package in the owned handlers_crud.go that
// methods are about to be written into: existing is the file's current
// bytes (nil at its birth). A name the file already binds the ORM's path
// to is reused, so shims appended to a file scaffolded under an older
// forge (`db "<module>/internal/store"`) match the ones beside them, and
// a file that imports the store itself is never given a second import.
// Otherwise the name is chosen free of the file's imports, the imports
// the new shims add, and the package's top-level declarations. The
// file's existing lines are never rewritten.
func shimORMBinding(data CRUDTemplateData, methods []CRUDMethodTemplateData, existing []byte) ORMBinding {
	ormPath := importSpecPath(data.DBImport)
	bound := fileImports(existing)
	for _, b := range bound {
		if b.path == ormPath {
			return ORMBinding{Spec: ormPath, Qual: b.name}
		}
	}
	taken := map[string]bool{}
	for _, b := range bound {
		taken[b.name] = true
	}
	for _, imp := range crudShimImports(CRUDTemplateData{
		Module:       data.Module,
		ProtoPackage: data.ProtoPackage,
		DBImport:     data.DBImport,
		CRUDMethods:  methods,
	}) {
		if importSpecPath(imp) != ormPath {
			taken[importSpecName(imp)] = true
		}
	}
	return ChooseORMBinding(data.Module, data.orm, func(name string) bool {
		return taken[name] || data.pkgDecls[name]
	})
}
