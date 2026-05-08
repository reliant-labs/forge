package crud

import (
	"context"
	"fmt"

	"connectrpc.com/connect"

	"github.com/reliant-labs/forge/pkg/orm"
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

// wrapInternal wraps a repository error in the canonical
// "<op> <entity>: %w" envelope at CodeInternal. This format matches
// what the previous generated handler emitted, byte for byte.
func wrapInternal(op, entity string, err error) error {
	return connect.NewError(connect.CodeInternal, fmt.Errorf("%s %s: %w", op, entity, err))
}

// wrapNotFound is the same envelope at CodeNotFound (used by Get).
func wrapNotFound(op, entity string, err error) error {
	return connect.NewError(connect.CodeNotFound, fmt.Errorf("%s %s: %w", op, entity, err))
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
			return nil, wrapInternal("create", op.EntityLower, err)
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
			return nil, wrapNotFound("get", op.EntityLower, err)
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
}

// HandleUpdate runs auth -> tenant -> validate-required -> persist -> pack.
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
		if err := op.Persist(ctx, tid, entity); err != nil {
			return nil, wrapInternal("update", op.EntityLower, err)
		}
		return connect.NewResponse(op.Pack(entity)), nil
	}
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
			return nil, wrapInternal("delete", op.EntityLower, err)
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
				if err := orm.ValidateOrderBy(clause); err != nil {
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
			return nil, wrapInternal("list", op.EntityLower, err)
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
