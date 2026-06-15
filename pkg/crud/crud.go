package crud

import (
	"context"
	"errors"
	"fmt"

	"connectrpc.com/connect"

	"github.com/reliant-labs/forge/pkg/orm"
	"github.com/reliant-labs/forge/pkg/svcerr"
)

// Authorize is a hook the shim passes in to perform the per-RPC
// authorization check. The library runs it before the repository call
// and wraps its error as connect.CodePermissionDenied.
//
// Returning nil allows the call. Returning a non-nil error fails the
// call. The hook may be nil — in which case authorization is skipped
// (mirroring methods declared without auth_required: true in proto).
type Authorize func(ctx context.Context) error

// RequireTenant is a hook the shim passes in for tenant-scoped
// operations. The library invokes it after the auth check, expects a
// tenant ID string back, and forwards it into the repository call. The
// returned error is wrapped as connect.CodeUnauthenticated.
//
// Methods on tenant-aware entities pass a real closure that delegates
// to the project's middleware.RequireTenantID. Methods on global
// entities pass nil and the library treats the call as un-tenanted.
type RequireTenant func(ctx context.Context) (string, error)

// runAuth invokes the auth hook (if any) and wraps its error as
// CodePermissionDenied. nil hook means "no auth required".
func runAuth(ctx context.Context, hook Authorize) error {
	if hook == nil {
		return nil
	}
	if err := hook(ctx); err != nil {
		// If the hook already returned a connect error, pass it through;
		// otherwise wrap as PermissionDenied (the legacy default).
		if _, ok := err.(*connect.Error); ok {
			return err
		}
		return connect.NewError(connect.CodePermissionDenied, err)
	}
	return nil
}

// runTenant invokes the tenant hook (if any) and wraps its error as
// CodeUnauthenticated. nil hook means "no tenant scoping".
func runTenant(ctx context.Context, hook RequireTenant) (string, error) {
	if hook == nil {
		return "", nil
	}
	tid, err := hook(ctx)
	if err != nil {
		if _, ok := err.(*connect.Error); ok {
			return "", err
		}
		return "", connect.NewError(connect.CodeUnauthenticated, err)
	}
	return tid, nil
}

// mapRepoErr is the single repository-error -> client-error chokepoint,
// routed through pkg/svcerr (the prescribed handler convention IS the
// demonstrated one):
//
//   - a missing row (orm.ErrNoRows, or an svcerr.ErrNotFound the
//     repository already classified) maps to CodeNotFound with a clean
//     "<entity>: not found" message;
//   - EVERYTHING else maps to CodeInternal with safe text. Repository
//     errors carry SQL fragments and driver internals ("sql: no rows in
//     result set", "no such column: ..."), and a connect.Error message
//     is client-visible — raw SQL must never cross the wire. The full
//     error is still observable server-side: the generated ORM layer
//     records it on the active trace span before returning it.
func mapRepoErr(op, entity string, err error) error {
	if errors.Is(err, orm.ErrNoRows) || svcerr.IsNotFound(err) {
		return svcerr.Wrap(svcerr.NotFound(entity))
	}
	// An update_mask path that names no updatable column is the CALLER's
	// mistake, not a server fault: InvalidArgument, with the offending
	// path named and no SQL in the message (the typed error carries only
	// the field name).
	var unknownField *orm.UnknownFieldError
	if errors.As(err, &unknownField) {
		return svcerr.Wrap(svcerr.InvalidArgument(fmt.Sprintf(
			"%s %s: unknown or immutable update_mask path %q", op, entity, unknownField.Field)))
	}
	return svcerr.Wrap(svcerr.Internal(fmt.Sprintf("%s %s failed", op, entity)))
}

// CreateOp wires the per-RPC concerns of a Create handler.
//
// Create returns the constructed entity in the response. The shim
// supplies:
//
//   - Entity      — proto request -> internal entity constructor.
//   - Persist     — repository call. tenantID is empty when Tenant is nil.
//   - Pack        — internal entity -> connect.Response.
//   - EntityLower — lowercase entity name used in the error envelope.
//   - Auth/Tenant — optional hooks.
type CreateOp[Req, Resp, Ent any] struct {
	EntityLower string
	Auth        Authorize
	Tenant      RequireTenant
	Entity      func(req *Req) Ent
	Persist     func(ctx context.Context, tenantID string, entity Ent) error
	Pack        func(entity Ent) *Resp
}

// HandleCreate runs the canonical Create lifecycle:
//
//	auth -> tenant -> build entity -> persist -> pack response.
//
// All error-mapping is fixed; the shim only carries data shape.
func HandleCreate[Req, Resp, Ent any](op CreateOp[Req, Resp, Ent]) func(context.Context, *connect.Request[Req]) (*connect.Response[Resp], error) {
	return func(ctx context.Context, req *connect.Request[Req]) (*connect.Response[Resp], error) {
		if err := runAuth(ctx, op.Auth); err != nil {
			return nil, err
		}
		tid, err := runTenant(ctx, op.Tenant)
		if err != nil {
			return nil, err
		}
		entity := op.Entity(req.Msg)
		if err := op.Persist(ctx, tid, entity); err != nil {
			return nil, mapRepoErr("create", op.EntityLower, err)
		}
		return connect.NewResponse(op.Pack(entity)), nil
	}
}

// GetOp wires the per-RPC concerns of a Get handler.
type GetOp[Req, Resp, Ent any] struct {
	EntityLower string
	Auth        Authorize
	Tenant      RequireTenant
	ID          func(req *Req) string
	Fetch       func(ctx context.Context, tenantID string, id string) (Ent, error)
	Pack        func(entity Ent) *Resp
}

// HandleGet runs auth -> tenant -> fetch -> pack. Repository errors
// are mapped to CodeNotFound; the legacy generator did the same.
func HandleGet[Req, Resp, Ent any](op GetOp[Req, Resp, Ent]) func(context.Context, *connect.Request[Req]) (*connect.Response[Resp], error) {
	return func(ctx context.Context, req *connect.Request[Req]) (*connect.Response[Resp], error) {
		if err := runAuth(ctx, op.Auth); err != nil {
			return nil, err
		}
		tid, err := runTenant(ctx, op.Tenant)
		if err != nil {
			return nil, err
		}
		entity, err := op.Fetch(ctx, tid, op.ID(req.Msg))
		if err != nil {
			return nil, mapRepoErr("get", op.EntityLower, err)
		}
		return connect.NewResponse(op.Pack(entity)), nil
	}
}

// UpdateOp wires the per-RPC concerns of an Update handler. The shim
// supplies an EntityFromReq closure that returns (entity, ok). The
// library treats ok == false as "request missing entity" and returns
// CodeInvalidArgument with the same wording the legacy generator used.
type UpdateOp[Req, Resp, Ent any] struct {
	EntityLower    string
	EntityFieldLow string // lowercase form of the proto field that holds the entity, e.g. "user"
	Auth           Authorize
	Tenant         RequireTenant
	Entity         func(req *Req) (entity Ent, ok bool)
	Persist        func(ctx context.Context, tenantID string, entity Ent) error
	Pack           func(entity Ent) *Resp

	// Mask extracts the AIP-134 update_mask paths from the request
	// (req.GetUpdateMask().GetPaths()). nil when the proto's update
	// request has no update_mask field — HandleUpdate then behaves
	// exactly as before this field existed (full replace via Persist).
	Mask func(req *Req) []string

	// PersistMasked writes ONLY the named fields (proto field names ==
	// column names, snake_case). The generator wires it whenever it
	// wires Mask. If Mask is set but PersistMasked is nil and a request
	// arrives with concrete paths, HandleUpdate fails CodeInternal —
	// silently widening a masked write to a full replace is the
	// data-loss bug this hook exists to prevent.
	PersistMasked func(ctx context.Context, tenantID string, entity Ent, fields []string) error
}

// HandleUpdate runs auth -> tenant -> validate-required -> persist -> pack.
//
// AIP-134 update_mask semantics (when op.Mask is wired):
//
//   - mask absent/empty, or containing "*"  → full-object replace via
//     op.Persist. AIP-134 permits full replacement when the behavior is
//     documented — this is that documentation. Callers that want a
//     partial update MUST send a mask.
//   - mask with concrete paths → op.PersistMasked writes only those
//     fields. Paths are proto field names (snake_case, == column names).
//   - unknown or immutable path → CodeInvalidArgument naming the path
//     (mapped from orm.UnknownFieldError by mapRepoErr).
//
// After a masked write the response echoes the request entity: masked
// fields hold their new values, unmasked fields hold whatever the caller
// sent (NOT necessarily the stored values). Re-read with Get for the
// authoritative row.
func HandleUpdate[Req, Resp, Ent any](op UpdateOp[Req, Resp, Ent]) func(context.Context, *connect.Request[Req]) (*connect.Response[Resp], error) {
	return func(ctx context.Context, req *connect.Request[Req]) (*connect.Response[Resp], error) {
		if err := runAuth(ctx, op.Auth); err != nil {
			return nil, err
		}
		tid, err := runTenant(ctx, op.Tenant)
		if err != nil {
			return nil, err
		}
		entity, ok := op.Entity(req.Msg)
		if !ok {
			return nil, connect.NewError(
				connect.CodeInvalidArgument,
				fmt.Errorf("update %s: %s is required", op.EntityLower, op.EntityFieldLow),
			)
		}
		if op.Mask != nil {
			if paths, full := maskPaths(op.Mask(req.Msg)); !full {
				if op.PersistMasked == nil {
					// Wiring bug, not caller error: the generator emits Mask
					// and PersistMasked together. Fail loudly rather than
					// silently rewriting every column.
					return nil, connect.NewError(
						connect.CodeInternal,
						fmt.Errorf("update %s: update_mask received but masked persistence is not wired", op.EntityLower),
					)
				}
				if err := op.PersistMasked(ctx, tid, entity, paths); err != nil {
					return nil, mapRepoErr("update", op.EntityLower, err)
				}
				return connect.NewResponse(op.Pack(entity)), nil
			}
		}
		if err := op.Persist(ctx, tid, entity); err != nil {
			return nil, mapRepoErr("update", op.EntityLower, err)
		}
		return connect.NewResponse(op.Pack(entity)), nil
	}
}

// maskPaths normalizes update_mask paths: blank entries are dropped, and
// full reports whether the mask requests a full replace (no concrete
// paths, or any "*" entry).
func maskPaths(raw []string) (paths []string, full bool) {
	for _, p := range raw {
		if p == "" {
			continue
		}
		if p == "*" {
			return nil, true
		}
		paths = append(paths, p)
	}
	return paths, len(paths) == 0
}

// DeleteOp wires the per-RPC concerns of a Delete handler.
type DeleteOp[Req, Resp any] struct {
	EntityLower string
	Auth        Authorize
	Tenant      RequireTenant
	ID          func(req *Req) string
	Persist     func(ctx context.Context, tenantID string, id string) error
	// Pack is optional. When nil, HandleDelete returns the proto's
	// zero-value response (matching the legacy DeleteResponse{} shape).
	Pack func() *Resp
}

// HandleDelete runs auth -> tenant -> persist -> empty response.
func HandleDelete[Req, Resp any](op DeleteOp[Req, Resp]) func(context.Context, *connect.Request[Req]) (*connect.Response[Resp], error) {
	return func(ctx context.Context, req *connect.Request[Req]) (*connect.Response[Resp], error) {
		if err := runAuth(ctx, op.Auth); err != nil {
			return nil, err
		}
		tid, err := runTenant(ctx, op.Tenant)
		if err != nil {
			return nil, err
		}
		if err := op.Persist(ctx, tid, op.ID(req.Msg)); err != nil {
			return nil, mapRepoErr("delete", op.EntityLower, err)
		}
		var resp *Resp
		if op.Pack != nil {
			resp = op.Pack()
		} else {
			var zero Resp
			resp = &zero
		}
		return connect.NewResponse(resp), nil
	}
}

// ListOp wires the per-RPC concerns of a List handler. Pagination,
// filter, and order-by bookkeeping live in the library; the per-RPC
// shim still provides the per-field filter -> orm.QueryOption mapping
// (this is data, not lifecycle, and reflection-free Go can't generalize
// it without a code-gen table).
type ListOp[Req, Resp, Ent any] struct {
	EntityLower  string
	Auth         Authorize
	Tenant       RequireTenant
	PkColumnName string // empty disables PK-cursor pagination

	// Columns is the entity's declared column allowlist (the generated
	// db.<Entity>Columns var). User-supplied order_by columns are
	// validated against it — identifier-shape validation alone lets an
	// undeclared column reach the database, where some engines silently
	// treat it as a constant (an ordering no-op).
	Columns []string

	// Pagination knobs. The library applies the same defaults the legacy
	// template did when these are zero.
	HasPagination   bool
	DefaultPageSize int // 0 -> 50
	MaxPageSize     int // 0 -> 100

	// HasOrderBy enables req.Msg.OrderBy / req.Msg.Descending handling
	// via the OrderBy/Descending closures.
	HasOrderBy bool
	OrderBy    func(req *Req) (clause string, descending bool)

	// Filters returns extra orm.QueryOption values built from per-field
	// filter logic. The shim implements this as a static sequence of
	// "if req.Msg.X != nil { opts = append(opts, orm.WhereILike(...)) }"
	// statements — same as the legacy template, just lifted into a
	// closure.
	Filters func(req *Req) []orm.QueryOption

	// PageToken / PageSize accessors. PageSize is clamped by the
	// library; PageToken is decoded by the library.
	PageToken func(req *Req) string
	PageSize  func(req *Req) int

	// Query runs the repository call. tenantID is "" when Tenant is nil.
	// Returns slice + error; the library handles the +1 fetch and
	// trim-to-pageSize.
	Query func(ctx context.Context, tenantID string, opts []orm.QueryOption) ([]Ent, error)

	// EntityID extracts the cursor key from the last-of-page entity.
	// Required when HasPagination is true.
	EntityID func(entity Ent) string

	// Pack receives the trimmed item slice and the next page token (empty
	// when no further page). Shim assembles the response with the right
	// repeated-field name.
	Pack func(items []Ent, nextPageToken string) *Resp
}

// HandleList runs auth -> tenant -> page/order/filter assembly ->
// repository call -> trim -> pack.
func HandleList[Req, Resp, Ent any](op ListOp[Req, Resp, Ent]) func(context.Context, *connect.Request[Req]) (*connect.Response[Resp], error) {
	return func(ctx context.Context, req *connect.Request[Req]) (*connect.Response[Resp], error) {
		if err := runAuth(ctx, op.Auth); err != nil {
			return nil, err
		}
		tid, err := runTenant(ctx, op.Tenant)
		if err != nil {
			return nil, err
		}

		var opts []orm.QueryOption

		// Pagination clamp + fetch+1 + cursor decode.
		pageSize := 0
		if op.HasPagination {
			pageSize = op.PageSize(req.Msg)
			defSize := op.DefaultPageSize
			if defSize <= 0 {
				defSize = 50
			}
			maxSize := op.MaxPageSize
			if maxSize <= 0 {
				maxSize = 100
			}
			if pageSize <= 0 {
				pageSize = defSize
			}
			if pageSize > maxSize {
				pageSize = maxSize
			}
			opts = append(opts, orm.WithLimit(pageSize+1))

			if tok := op.PageToken(req.Msg); tok != "" {
				cursor, derr := orm.DecodeCursor(tok)
				if derr != nil {
					// Preserve the legacy "invalid page token" wording exactly.
					return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("invalid page token"))
				}
				opts = append(opts, orm.WithWhere(op.PkColumnName, orm.GreaterThan, cursor))
			}
		}

		// Filters (per-RPC, supplied by shim).
		if op.Filters != nil {
			opts = append(opts, op.Filters(req.Msg)...)
		}

		// Order-by handling. Identical semantics to the legacy template:
		// validate the user-supplied clause, default to PK ASC when empty
		// (and pagination is on), pick Asc/Desc from req.Msg.Descending.
		appliedOrder := false
		if op.HasOrderBy && op.OrderBy != nil {
			clause, desc := op.OrderBy(req.Msg)
			if clause != "" {
				if err := orm.ValidateOrderBy(clause, op.Columns); err != nil {
					return nil, connect.NewError(connect.CodeInvalidArgument, err)
				}
				ord := orm.Asc
				if desc {
					ord = orm.Desc
				}
				opts = append(opts, orm.WithOrderBy(clause, ord))
				appliedOrder = true
			}
		}
		if op.HasPagination && !appliedOrder && op.PkColumnName != "" {
			opts = append(opts, orm.WithOrderBy(op.PkColumnName, orm.Asc))
		}

		results, err := op.Query(ctx, tid, opts)
		if err != nil {
			return nil, mapRepoErr("list", op.EntityLower, err)
		}

		var nextPageToken string
		if op.HasPagination && len(results) > pageSize {
			results = results[:pageSize]
			if op.EntityID != nil {
				nextPageToken = orm.EncodeCursor(op.EntityID(results[pageSize-1]))
			}
		}

		return connect.NewResponse(op.Pack(results, nextPageToken)), nil
	}
}
