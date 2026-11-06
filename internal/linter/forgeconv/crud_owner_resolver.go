// File: internal/linter/forgeconv/crud_owner_resolver.go
//
// forgeconv-crud-owner-resolver — an owner-scoping wrapper whose owner
// comes from somewhere its siblings' does not.
//
// # The defect
//
// growth replaced every handlers_crud.go placeholder (`owner :=
// claims.UserID`) with `owner, err := tenancy.TenantID(ctx)`. Later forge
// appended two List wrappers still on the placeholder. They compiled and
// ran, and scoped every query by a user id against a tenant_id column:
// nothing matched, and nothing in forge said so. A wrapper on the wrong
// principal is an access-control bug whichever way it fails — empty here,
// another principal's rows when two id spaces overlap.
//
// codegen now appends with the siblings' resolver (crud_owner_source.go);
// this rule finds the files it was too late for, and the hand edits that
// re-create the mismatch.
//
// # What it checks
//
// Per handler file, per owner column, using codegen.ScanCRUDOwnerSources,
// against the wrappers of the same entity (or, when the entity has no
// other, every entity's on that column — except `id`, which names a
// different principal on every table): a wrapper is flagged when
//
//   - it is on forge's placeholder (`middleware.GetUser(ctx).UserID`) while
//     a sibling on the same column resolves the owner some other way — the
//     sibling records the decision the placeholder was waiting for; or
//   - its source differs from the one source every sibling of its own class
//     agrees on. Reads are compared with reads and writes with writes, so a
//     write that needs a stricter resolver (tenancy.RequireAdmin beside a
//     file of tenancy.TenantID reads) is not a finding, while a read on
//     tenancy.UserID among tenancy.TenantID reads is. "Agrees" takes two
//     siblings: one against one is a disagreement with no side to take.
//
// An owner forge cannot trace (read off the request, computed) is not
// judged either way. A deliberate exception takes
// `// forge:lint-disable-next-line forgeconv-crud-owner-resolver: <why>`
// above the method.
package forgeconv

import (
	"fmt"
	"go/ast"
	"go/token"
	"sort"
	"strings"

	"github.com/reliant-labs/forge/internal/codegen"
	"github.com/reliant-labs/forge/internal/naming"
)

const crudOwnerResolverRule = "forgeconv-crud-owner-resolver"

// lintCRUDOwnerResolverFile reports the wrappers in one parsed handler file
// whose owner source disagrees with their siblings'.
func lintCRUDOwnerResolverFile(relPath string, fset *token.FileSet, file *ast.File) []Finding {
	if ast.IsGenerated(file) {
		return nil
	}
	uses := codegen.ScanCRUDOwnerSources(fset, file)
	if len(uses) == 0 {
		return nil
	}

	// A create's stamp names the entity FIELD (`e.TenantID = owner`), a
	// predicate the column; fold fields onto the columns the file names.
	fieldColumn := map[string]string{}
	for _, u := range uses {
		if u.Column != "" {
			fieldColumn[naming.ColumnGoName(u.Column)] = u.Column
		}
	}
	type methodSource struct {
		method string
		entity string
		read   bool
		line   int
		src    codegen.CRUDOwnerSource
	}
	byColumn := map[string][]methodSource{}
	seen := map[string]bool{}
	for _, u := range uses {
		if u.Entity == "" {
			continue // not a CRUD-shaped rpc: no siblings to compare it with
		}
		col := u.Column
		if col == "" {
			if c, ok := fieldColumn[u.Field]; ok {
				col = c
			} else {
				col = u.Field
			}
		}
		key := col + "\x00" + u.Method + "\x00" + u.Source.String()
		if seen[key] {
			continue
		}
		seen[key] = true
		byColumn[col] = append(byColumn[col], methodSource{u.Method, u.Entity, u.Read, u.Line, u.Source})
	}

	columns := make([]string, 0, len(byColumn))
	for c := range byColumn {
		columns = append(columns, c)
	}
	sort.Strings(columns)

	var findings []Finding
	for _, col := range columns {
		ms := byColumn[col]
		for _, m := range ms {
			if m.src.Kind == codegen.CRUDOwnerUnknown {
				continue
			}
			// The decided sources of the OTHER wrappers on this column:
			// the same entity's when it has any, else (for a column that
			// is not the conventional key, whose meaning differs per
			// table) every entity's; same class first, every class when
			// the class has none.
			collect := func(sameEntity bool) (sameClass, anyClass map[string]int) {
				sameClass, anyClass = map[string]int{}, map[string]int{}
				for _, o := range ms {
					if o.method == m.method || (o.entity == m.entity) != sameEntity ||
						o.src.Kind == codegen.CRUDOwnerUnknown || o.src.IsPlaceholder() {
						continue
					}
					anyClass[o.src.String()]++
					if o.read == m.read {
						sameClass[o.src.String()]++
					}
				}
				return sameClass, anyClass
			}
			scope := m.entity + "." + col
			sameClass, anyClass := collect(true)
			if len(anyClass) == 0 && col != "id" {
				sameClass, anyClass = collect(false)
				scope = col
			}
			agreed := sameClass
			if len(agreed) == 0 {
				agreed = anyClass
			}
			if len(agreed) == 0 || agreed[m.src.String()] > 0 {
				continue
			}
			if !m.src.IsPlaceholder() && (len(sameClass) != 1 || sameClass[sortedKeys(sameClass)[0]] < 2) {
				// A real resolver among siblings that do not agree with one
				// another, or one sibling against one: no settled policy to
				// measure it against.
				continue
			}
			others := sortedKeys(agreed)
			class := "write"
			if m.read {
				class = "read"
			}
			msg := fmt.Sprintf("%s scopes %s by %s, but this file's other %s wrappers on %s use %s",
				m.method, col, m.src, class, scope, strings.Join(others, " / "))
			if len(sameClass) == 0 {
				msg = fmt.Sprintf("%s scopes %s by %s, but this file's other wrappers on %s use %s",
					m.method, col, m.src, scope, strings.Join(others, " / "))
			}
			if m.src.IsPlaceholder() {
				msg += " — forge's FORGE_SCAFFOLD placeholder, not a decision: the query matches rows by the wrong principal"
			}
			remediation := fmt.Sprintf("resolve the owner as its siblings do (%s) unless this RPC really needs a different principal; "+
				"then say so with `// forge:lint-disable-next-line %s: <why>` above the method. See skill: auth/authorization.",
				others[0], crudOwnerResolverRule)
			findings = append(findings, Finding{
				Rule:        crudOwnerResolverRule,
				Severity:    SeverityError,
				File:        relPath,
				Line:        m.line,
				Message:     msg,
				Remediation: remediation,
			})
		}
	}
	return findings
}

func sortedKeys(m map[string]int) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
