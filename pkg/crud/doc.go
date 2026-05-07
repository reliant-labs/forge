// Package crud provides runtime helpers used by the per-service
// handlers_crud_gen.go file forge generates from a service's CRUD RPCs.
//
// # Pattern
//
// Forge's CRUD generator emits a thin per-RPC shim that delegates to one
// of [HandleCreate], [HandleGet], [HandleList], [HandleUpdate], or
// [HandleDelete]. The shim carries the only things forge can know that
// this library cannot:
//
//   - The RPC signature (req/resp connect types).
//   - The proto -> entity field copy for Create.
//   - The repository call site (db.<Name> function).
//   - The response packing (proto field that holds the entity).
//
// Everything else — auth check, tenant check, error mapping, cursor
// encoding/decoding, pagination clamping, list bookkeeping — moves into
// this package.
//
// # Auth and tenant
//
// The library is decoupled from the per-project middleware package.
// Auth and tenant checks are expressed as closures the shim passes in.
// This avoids a hard import dependency on the user project's middleware
// surface (which is itself generated and project-specific).
//
// In a generated shim, the auth closure looks like this:
//
//	func(ctx context.Context) error {
//	    claims, err := middleware.GetUser(ctx)
//	    if err != nil { return err }
//	    return s.deps.Authorizer.Can(ctx, claims, middleware.ActionCreate, "user")
//	}
//
// The library invokes the closure (if non-nil) and returns its error
// wrapped as connect.CodePermissionDenied — preserving the previous
// generated behaviour.
//
// # Behavioural fingerprint
//
// The pre-existing per-method generator wrote three observable strings:
//
//   - "<op> <entity_lower>: <wrapped error>" for Create/Get/List/Update/Delete.
//   - "invalid page token" for an undecodable PageToken.
//   - "<op> <entity_lower>: <field> is required" when an Update request
//     has a nil entity field.
//
// All three are preserved verbatim by this package and locked by tests
// in [crud_test.go].
package crud
