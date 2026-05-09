package crud

import (
	"context"
	"errors"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/reliant-labs/forge/pkg/orm"
)

// Test fixture types: a tiny "User" entity and per-RPC req/resp shapes
// that mirror what the proto gen would produce.

type user struct {
	ID    string
	Name  string
	Email string
}

type createReq struct {
	Name  string
	Email string
}
type createResp struct{ User *user }

type getReq struct{ ID string }
type getResp struct{ User *user }

type updateReq struct{ User *user }
type updateResp struct{ User *user }

type deleteReq struct{ ID string }
type deleteResp struct{ ID string }

type listReq struct {
	PageSize   int
	PageToken  string
	Search     *string
	OrderBy    string
	Descending bool
}
type listResp struct {
	Users         []*user
	NextPageToken string
}

// fakeRepo is the test stand-in for the per-project db.* helpers.
type fakeRepo struct {
	store      map[string]*user
	createErr  error
	getErr     error
	listErr    error
	updateErr  error
	deleteErr  error
	listResult []*user
	tenantSeen string
	queryOpts  []orm.QueryOption
}

func newRepo() *fakeRepo {
	return &fakeRepo{store: map[string]*user{}}
}

// --- HandleCreate -----------------------------------------------------

func TestHandleCreate_HappyPath(t *testing.T) {
	repo := newRepo()
	h := HandleCreate(CreateOp[createReq, createResp, *user]{
		EntityLower: "user",
		Entity: func(r *createReq) *user {
			return &user{Name: r.Name, Email: r.Email}
		},
		Persist: func(ctx context.Context, _ string, e *user) error {
			if repo.createErr != nil {
				return repo.createErr
			}
			e.ID = "u1"
			repo.store[e.ID] = e
			return nil
		},
		Pack: func(e *user) *createResp { return &createResp{User: e} },
	})
	resp, err := h(context.Background(), connect.NewRequest(&createReq{Name: "Ada", Email: "a@x"}))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if resp.Msg.User.Name != "Ada" || resp.Msg.User.ID != "u1" {
		t.Fatalf("bad resp: %+v", resp.Msg.User)
	}
}

func TestHandleCreate_AuthFailure(t *testing.T) {
	denied := errors.New("nope")
	h := HandleCreate(CreateOp[createReq, createResp, *user]{
		EntityLower: "user",
		Auth:        func(ctx context.Context) error { return denied },
		Entity:      func(r *createReq) *user { return &user{} },
		Persist:     func(context.Context, string, *user) error { return nil },
		Pack:        func(*user) *createResp { return &createResp{} },
	})
	_, err := h(context.Background(), connect.NewRequest(&createReq{}))
	cerr := new(connect.Error)
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodePermissionDenied {
		t.Fatalf("want PermissionDenied, got %v", err)
	}
}

func TestHandleCreate_TenantFailure(t *testing.T) {
	h := HandleCreate(CreateOp[createReq, createResp, *user]{
		EntityLower: "user",
		Tenant:      func(ctx context.Context) (string, error) { return "", errors.New("no tenant") },
		Entity:      func(r *createReq) *user { return &user{} },
		Persist:     func(context.Context, string, *user) error { return nil },
		Pack:        func(*user) *createResp { return &createResp{} },
	})
	_, err := h(context.Background(), connect.NewRequest(&createReq{}))
	cerr := new(connect.Error)
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeUnauthenticated {
		t.Fatalf("want Unauthenticated, got %v", err)
	}
}

func TestHandleCreate_RepoError_WrappedAsInternal(t *testing.T) {
	repo := newRepo()
	repo.createErr = errors.New("db down")
	h := HandleCreate(CreateOp[createReq, createResp, *user]{
		EntityLower: "user",
		Entity:      func(r *createReq) *user { return &user{} },
		Persist:     func(context.Context, string, *user) error { return repo.createErr },
		Pack:        func(*user) *createResp { return &createResp{} },
	})
	_, err := h(context.Background(), connect.NewRequest(&createReq{}))
	cerr := new(connect.Error)
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeInternal {
		t.Fatalf("want Internal, got %v", err)
	}
	if !strings.Contains(cerr.Message(), "create user:") {
		t.Fatalf("error envelope wording changed: %q", cerr.Message())
	}
}

// --- HandleGet --------------------------------------------------------

func TestHandleGet_HappyPath(t *testing.T) {
	want := &user{ID: "u1", Name: "Ada"}
	h := HandleGet(GetOp[getReq, getResp, *user]{
		EntityLower: "user",
		ID:          func(r *getReq) string { return r.ID },
		Fetch:       func(context.Context, string, string) (*user, error) { return want, nil },
		Pack:        func(u *user) *getResp { return &getResp{User: u} },
	})
	resp, err := h(context.Background(), connect.NewRequest(&getReq{ID: "u1"}))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if resp.Msg.User != want {
		t.Fatal("unexpected entity returned")
	}
}

func TestHandleGet_NotFound(t *testing.T) {
	h := HandleGet(GetOp[getReq, getResp, *user]{
		EntityLower: "user",
		ID:          func(r *getReq) string { return r.ID },
		Fetch: func(context.Context, string, string) (*user, error) {
			return nil, errors.New("no rows")
		},
		Pack: func(u *user) *getResp { return &getResp{User: u} },
	})
	_, err := h(context.Background(), connect.NewRequest(&getReq{ID: "nope"}))
	cerr := new(connect.Error)
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeNotFound {
		t.Fatalf("want NotFound, got %v", err)
	}
	if !strings.Contains(cerr.Message(), "get user:") {
		t.Fatalf("error envelope wording changed: %q", cerr.Message())
	}
}

// --- HandleUpdate -----------------------------------------------------

func TestHandleUpdate_HappyPath(t *testing.T) {
	h := HandleUpdate(UpdateOp[updateReq, updateResp, *user]{
		EntityLower:    "user",
		EntityFieldLow: "user",
		Entity: func(r *updateReq) (*user, bool) {
			if r.User == nil {
				return nil, false
			}
			return r.User, true
		},
		Persist: func(context.Context, string, *user) error { return nil },
		Pack:    func(u *user) *updateResp { return &updateResp{User: u} },
	})
	resp, err := h(context.Background(), connect.NewRequest(&updateReq{User: &user{ID: "u1", Name: "B"}}))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if resp.Msg.User.Name != "B" {
		t.Fatal("entity not propagated through Pack")
	}
}

func TestHandleUpdate_NilEntity_InvalidArgument(t *testing.T) {
	h := HandleUpdate(UpdateOp[updateReq, updateResp, *user]{
		EntityLower:    "user",
		EntityFieldLow: "user",
		Entity:         func(r *updateReq) (*user, bool) { return r.User, r.User != nil },
		Persist:        func(context.Context, string, *user) error { return nil },
		Pack:           func(*user) *updateResp { return &updateResp{} },
	})
	_, err := h(context.Background(), connect.NewRequest(&updateReq{}))
	cerr := new(connect.Error)
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeInvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
	// Locked behavioural fingerprint: same wording as legacy generator.
	want := "update user: user is required"
	if !strings.Contains(cerr.Message(), want) {
		t.Fatalf("legacy message format changed.\nwant substring: %q\ngot: %q", want, cerr.Message())
	}
}

// --- HandleDelete -----------------------------------------------------

func TestHandleDelete_DefaultZeroResp(t *testing.T) {
	h := HandleDelete(DeleteOp[deleteReq, deleteResp]{
		EntityLower: "user",
		ID:          func(r *deleteReq) string { return r.ID },
		Persist:     func(context.Context, string, string) error { return nil },
	})
	resp, err := h(context.Background(), connect.NewRequest(&deleteReq{ID: "u1"}))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if resp.Msg == nil || resp.Msg.ID != "" {
		t.Fatalf("want zero-valued resp, got %+v", resp.Msg)
	}
}

func TestHandleDelete_PackOverride(t *testing.T) {
	h := HandleDelete(DeleteOp[deleteReq, deleteResp]{
		EntityLower: "user",
		ID:          func(r *deleteReq) string { return r.ID },
		Persist:     func(context.Context, string, string) error { return nil },
		Pack:        func() *deleteResp { return &deleteResp{ID: "echoed"} },
	})
	resp, _ := h(context.Background(), connect.NewRequest(&deleteReq{ID: "u1"}))
	if resp.Msg.ID != "echoed" {
		t.Fatalf("Pack override not used")
	}
}

func TestHandleDelete_RepoError_WrappedInternal(t *testing.T) {
	h := HandleDelete(DeleteOp[deleteReq, deleteResp]{
		EntityLower: "user",
		ID:          func(r *deleteReq) string { return r.ID },
		Persist:     func(context.Context, string, string) error { return errors.New("fk violation") },
	})
	_, err := h(context.Background(), connect.NewRequest(&deleteReq{ID: "u1"}))
	cerr := new(connect.Error)
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeInternal {
		t.Fatalf("want Internal, got %v", err)
	}
	if !strings.Contains(cerr.Message(), "delete user:") {
		t.Fatalf("envelope wording changed: %q", cerr.Message())
	}
}

// --- HandleList -------------------------------------------------------

func TestHandleList_PaginationDefaultsAndTrim(t *testing.T) {
	// Repo returns 51 results; default page size is 50 -> trim, NextPageToken set.
	all := make([]*user, 51)
	for i := range all {
		all[i] = &user{ID: "u" + string(rune('A'+i%26))}
	}
	h := HandleList(ListOp[listReq, listResp, *user]{
		EntityLower:   "user",
		PkColumnName:  "id",
		HasPagination: true,
		PageToken:     func(r *listReq) string { return r.PageToken },
		PageSize:      func(r *listReq) int { return r.PageSize },
		Query: func(ctx context.Context, _ string, _ []orm.QueryOption) ([]*user, error) {
			return all, nil
		},
		EntityID: func(u *user) string { return u.ID },
		Pack: func(items []*user, tok string) *listResp {
			return &listResp{Users: items, NextPageToken: tok}
		},
	})
	resp, err := h(context.Background(), connect.NewRequest(&listReq{}))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if len(resp.Msg.Users) != 50 {
		t.Fatalf("expected 50 results after trim, got %d", len(resp.Msg.Users))
	}
	if resp.Msg.NextPageToken == "" {
		t.Fatal("expected non-empty NextPageToken")
	}
	// Cursor round-trip: decode should give back the 50th entity ID.
	got, derr := orm.DecodeCursor(resp.Msg.NextPageToken)
	if derr != nil {
		t.Fatalf("cursor decode: %v", derr)
	}
	if got != all[49].ID {
		t.Fatalf("cursor mismatch: %q vs %q", got, all[49].ID)
	}
}

// TestHandleList_PageSizeClampAndCustomMax checks the page-size clamp by
// examining how many results end up in the response after trim. We seed
// the repo with `wantLimit-1` rows (i.e. the actual page size, with no
// next page) to assert the clamp applied.
func TestHandleList_PageSizeClampAndCustomMax(t *testing.T) {
	cases := []struct {
		name       string
		maxSize    int
		req        int
		wantPageSz int
	}{
		{"zero -> default 50", 0, 0, 50},
		{"over default-max 100 clamped", 0, 999, 100},
		{"custom max applied", 25, 999, 25},
		{"under default min", 0, 1, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Seed enough rows to overflow any page-size we may compute.
			seed := make([]*user, 200)
			for i := range seed {
				seed[i] = &user{ID: "u" + string(rune('A'+i%26))}
			}
			h := HandleList(ListOp[listReq, listResp, *user]{
				EntityLower:   "user",
				PkColumnName:  "id",
				HasPagination: true,
				MaxPageSize:   tc.maxSize,
				PageToken:     func(r *listReq) string { return "" },
				PageSize:      func(r *listReq) int { return r.PageSize },
				Query: func(ctx context.Context, _ string, _ []orm.QueryOption) ([]*user, error) {
					return seed, nil
				},
				EntityID: func(u *user) string { return u.ID },
				Pack: func(items []*user, tok string) *listResp {
					return &listResp{Users: items, NextPageToken: tok}
				},
			})
			resp, err := h(context.Background(), connect.NewRequest(&listReq{PageSize: tc.req}))
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if len(resp.Msg.Users) != tc.wantPageSz {
				t.Fatalf("page size = %d, want %d", len(resp.Msg.Users), tc.wantPageSz)
			}
		})
	}
}

func TestHandleList_BadCursor_InvalidArgument(t *testing.T) {
	h := HandleList(ListOp[listReq, listResp, *user]{
		EntityLower:   "user",
		PkColumnName:  "id",
		HasPagination: true,
		PageToken:     func(r *listReq) string { return r.PageToken },
		PageSize:      func(r *listReq) int { return r.PageSize },
		Query: func(ctx context.Context, _ string, _ []orm.QueryOption) ([]*user, error) {
			return nil, nil
		},
		EntityID: func(u *user) string { return u.ID },
		Pack:     func(items []*user, tok string) *listResp { return &listResp{} },
	})
	_, err := h(context.Background(), connect.NewRequest(&listReq{PageToken: "%%not-base64"}))
	cerr := new(connect.Error)
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeInvalidArgument {
		t.Fatalf("want InvalidArgument, got %v", err)
	}
	// Legacy fingerprint preserved.
	if !strings.Contains(cerr.Message(), "invalid page token") {
		t.Fatalf("legacy 'invalid page token' wording lost: %q", cerr.Message())
	}
}

func TestHandleList_NoPagination_NoLimitNoCursor(t *testing.T) {
	var sawOpts int
	h := HandleList(ListOp[listReq, listResp, *user]{
		EntityLower: "user",
		Query: func(ctx context.Context, _ string, opts []orm.QueryOption) ([]*user, error) {
			sawOpts = len(opts)
			return []*user{{ID: "x"}}, nil
		},
		Pack: func(items []*user, tok string) *listResp {
			return &listResp{Users: items, NextPageToken: tok}
		},
	})
	resp, err := h(context.Background(), connect.NewRequest(&listReq{}))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if sawOpts != 0 {
		t.Fatalf("HasPagination off => no opts emitted, got %d", sawOpts)
	}
	if resp.Msg.NextPageToken != "" {
		t.Fatalf("NextPageToken should be empty when pagination is off")
	}
}

func TestHandleList_OrderByValidation(t *testing.T) {
	h := HandleList(ListOp[listReq, listResp, *user]{
		EntityLower:   "user",
		PkColumnName:  "id",
		HasPagination: true,
		HasOrderBy:    true,
		PageToken:     func(r *listReq) string { return r.PageToken },
		PageSize:      func(r *listReq) int { return r.PageSize },
		OrderBy:       func(r *listReq) (string, bool) { return r.OrderBy, r.Descending },
		Query: func(ctx context.Context, _ string, _ []orm.QueryOption) ([]*user, error) {
			return nil, nil
		},
		EntityID: func(u *user) string { return u.ID },
		Pack:     func(items []*user, tok string) *listResp { return &listResp{} },
	})
	// "; DROP TABLE" should fail validation.
	_, err := h(context.Background(), connect.NewRequest(&listReq{OrderBy: "name; DROP TABLE x"}))
	cerr := new(connect.Error)
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeInvalidArgument {
		t.Fatalf("want InvalidArgument from order-by validation, got %v", err)
	}
}

func TestHandleList_TenantPropagated(t *testing.T) {
	var seenTID string
	h := HandleList(ListOp[listReq, listResp, *user]{
		EntityLower:   "user",
		PkColumnName:  "id",
		HasPagination: true,
		Tenant:        func(ctx context.Context) (string, error) { return "tenant-7", nil },
		PageToken:     func(r *listReq) string { return r.PageToken },
		PageSize:      func(r *listReq) int { return r.PageSize },
		Query: func(ctx context.Context, tid string, _ []orm.QueryOption) ([]*user, error) {
			seenTID = tid
			return nil, nil
		},
		EntityID: func(u *user) string { return u.ID },
		Pack:     func(items []*user, tok string) *listResp { return &listResp{} },
	})
	_, err := h(context.Background(), connect.NewRequest(&listReq{}))
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if seenTID != "tenant-7" {
		t.Fatalf("tenant not propagated: %q", seenTID)
	}
}

// Connect errors from hooks should pass through with their code intact
// (the library only wraps non-connect errors).
func TestRunAuth_ConnectErrorPassesThrough(t *testing.T) {
	want := connect.NewError(connect.CodeUnauthenticated, errors.New("missing token"))
	h := HandleGet(GetOp[getReq, getResp, *user]{
		EntityLower: "user",
		Auth:        func(ctx context.Context) error { return want },
		ID:          func(r *getReq) string { return r.ID },
		Fetch:       func(context.Context, string, string) (*user, error) { return nil, nil },
		Pack:        func(*user) *getResp { return &getResp{} },
	})
	_, err := h(context.Background(), connect.NewRequest(&getReq{}))
	cerr := new(connect.Error)
	if !errors.As(err, &cerr) || cerr.Code() != connect.CodeUnauthenticated {
		t.Fatalf("want passthrough Unauthenticated, got %v", err)
	}
}
