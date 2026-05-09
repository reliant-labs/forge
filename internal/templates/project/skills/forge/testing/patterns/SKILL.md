---
name: patterns
description: "Copy-paste-ready test patterns — table-driven via the pkg/tdd library for RPC handlers, contract methods, and contract mocks; pure validators stay hand-rolled."
---

# Test Patterns

Four ready-to-copy templates covering the test shapes that appear in every Forge project. The first two delegate the table boilerplate to the `github.com/reliant-labs/forge/pkg/tdd` library; the last two stay hand-rolled because the table shape doesn't fit the library API. Copy, rename placeholders, fill in cases. Do not invent new shapes for these scenarios — use these.

All patterns are table-driven by default: one slice of cases, one iteration helper. Add cases by appending an entry, never by writing new test functions.

## Pattern 1: RPC handler test (use `tdd.TableRPC`)

One-line: in-process handler call via the generated `NewTest<Service>` helper from `pkg/app/testing.go`. Hermetic; no server, no network. The library carries the iteration + error-code assertion so the test file is just the case table.

```go
package myservice_test

import (
	"testing"

	"connectrpc.com/connect"

	"github.com/reliant-labs/forge/pkg/tdd"

	apiv1 "github.com/example/proj/gen/api/v1"
	"github.com/example/proj/pkg/app"
)

func TestCreateUser(t *testing.T) {
	t.Parallel()
	svc := app.NewTestUserService(t)

	tdd.TableRPC(t, []tdd.Case[apiv1.CreateUserRequest, apiv1.CreateUserResponse]{
		{
			Name: "happy path",
			Req:  connect.NewRequest(&apiv1.CreateUserRequest{Name: "Alice"}),
			Check: func(t *testing.T, resp *connect.Response[apiv1.CreateUserResponse]) {
				if resp.Msg.User.GetName() != "Alice" {
					t.Fatalf("name = %q", resp.Msg.User.GetName())
				}
			},
		},
		{
			Name:    "missing name",
			Req:     connect.NewRequest(&apiv1.CreateUserRequest{}),
			WantErr: connect.CodeInvalidArgument,
		},
	}, svc.CreateUser)
}
```

`tdd.Case[Req, Resp]` rows can also set a `Setup` hook (per-row mock wiring) and an `Ctx` (override the default `context.Background()`; pair with `tdd.WithTimeout` for deadlined cases).

When to use: validating a single handler's request/response/error contract. This is the default unit-test shape for any RPC. The integration scaffold (`//go:build integration`) uses the same shape with `app.NewTest<Service>Server` and `client.<Method>`.

## Pattern 2: Contract test (use `tdd.TableContract`)

One-line: exercise an `internal/<pkg>/contract.go`-defined Service interface implementation. Each row's `Call` closure invokes one method and the helper handles equality / error / custom-check assertions.

```go
package cache_test

import (
	"context"
	"testing"

	"github.com/reliant-labs/forge/pkg/tdd"

	"github.com/example/proj/internal/cache"
)

func TestContract(t *testing.T) {
	t.Parallel()
	svc := cache.New(cache.Deps{})
	ctx := context.Background()

	tdd.TableContract(t, svc, []tdd.ContractCase{
		{
			Name: "Set then Get round-trips",
			Setup: func(t *testing.T) { _ = svc.Set(ctx, "k", "v") },
			Call: func() (any, error) { return svc.Get(ctx, "k") },
			Want: "v",
		},
		{
			Name:    "Get on missing key returns ErrNotFound",
			Call:    func() (any, error) { return svc.Get(ctx, "missing") },
			WantErr: cache.ErrNotFound,
		},
	})
}
```

When to use: testing a `contract.go`-defined Service interface implementation. Forge's `forge package new` and `forge generate` scaffold a starter `contract_test.go` once; user owns it after.

## Pattern 3: Cobra runner test (validator extraction)

One-line: extract the validator BEFORE writing the test; tests touch only the pure helper, never the runner. No library helper here — the validator's signature is project-specific.

```go
// BAD — tests the full runner. Slow, may hang, drags in subprocess + I/O.
// func TestRunCreate(t *testing.T) { runCreate(cmd, []string{"alice"}) }

// GOOD — tests the pure validator extracted from the runner.
func TestValidateCreateArgs(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		input   string
		wantErr string // substring match; empty == expect no error
	}{
		{"valid", "alice", ""},
		{"empty rejected", "", "name required"},
		{"too long rejected", strings.Repeat("a", 256), "name too long"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateCreateArgs(tc.input)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("got %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
}
```

When to use: any Cobra `runX(cmd, args) error`. If `runX` does I/O, FS work, or subprocess calls, extract a `validateXArgs` first (see `testing` skill — Discipline section). Real end-to-end behavior of the runner belongs in an `e2e` test.

## Pattern 4: Pure validator / transformer test

One-line: pure data manipulation methods. No mocks, no library — a map-of-cases is fine because a pure function has no side-effect ordering.

```go
package naming_test

import (
	"testing"

	"github.com/example/proj/internal/naming"
)

func TestNamingPascalCase(t *testing.T) {
	t.Parallel()
	n := naming.New(naming.Deps{})

	cases := map[string]string{
		"user_id": "UserID",
		"api-key": "APIKey",
		"":        "",
		"already": "Already",
	}
	for in, want := range cases {
		in, want := in, want
		t.Run(in, func(t *testing.T) {
			t.Parallel()
			got := n.ToPascalCase(in)
			if got != want {
				t.Fatalf("ToPascalCase(%q) = %q, want %q", in, got, want)
			}
		})
	}
}
```

When to use: pure functions, formatters, parsers, naming helpers. Anything that takes data in and returns data out with no side effects.

## Library helpers cheat sheet

The `pkg/tdd` library exports:

| Helper | Use for |
|--------|---------|
| `tdd.Case[Req, Resp]` + `tdd.TableRPC` | unit / integration / E2E tests of unary Connect RPCs |
| `tdd.ContractCase` + `tdd.TableContract` | tests of `internal/<pkg>/contract.go` Service implementations |
| `tdd.E2EClient(t, srv, factory)` | wrap an httptest.Server in a typed Connect client (cleanup registered) |
| `tdd.NewMock(opts...)` + `tdd.MockOption[T]` | terse construction of Forge `MockService` (Func-field) mocks |
| `tdd.AssertConnectError(t, err, code)` | one-line Connect error code assertion |
| `tdd.WithTimeout(d)` | deadlined `context.Context` for `Case.Ctx` |
| `tdd.SetupMockDB(t)` | in-memory SQLite `*sql.DB` (driver must be blank-imported) |

Forge's scaffolders emit Pattern 1 (unit + integration) and Pattern 2 (contract) automatically. Hand-write Pattern 3 / Pattern 4 — those don't fit a generic helper.

## Rules

- Always table-driven. New cases = new struct entry, never a new test function.
- `t.Parallel()` at the function level by default. Per-row parallelism: skip it inside library helpers — the library iterates serially so per-row Setup hooks remain correct.
- Name cases by the SCENARIO ("missing name", "duplicate email"), not by the inputs ("test_1", "case_a").
- For RPC handlers, assert on `connect.CodeOf(err)`, not on the error string. `tdd.AssertConnectError` and `Case.WantErr` already do this.
- For maps in test fixtures, sort keys before formatting/comparing (see `testing` skill — Determinism rule).
