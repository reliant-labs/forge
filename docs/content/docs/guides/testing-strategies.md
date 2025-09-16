---
title: "Testing Strategies"
description: "Testing patterns for Forge services"
weight: 50
---

# Testing Strategies

Forge provides a generated test harness and multiple testing strategies for comprehensive test coverage.

## Unit Testing with Test Harness

Use the generated test helpers in `pkg/app/testing.go`:

```go
func TestCreateUser(t *testing.T) {
    // NewTestUsersService wires up the service with mock dependencies
    svc, mocks := app.NewTestUsersService(t)

    // Configure mock behavior
    mocks.Email.On("Send", mock.Anything, "test@example.com", mock.Anything, mock.Anything).Return(nil)

    // Call the service method
    resp, err := svc.CreateUser(context.Background(), connect.NewRequest(&usersv1.CreateUserRequest{
        Email: "test@example.com",
        Name:  "Test User",
    }))

    require.NoError(t, err)
    assert.NotEmpty(t, resp.Msg.Id)
    assert.Equal(t, "test@example.com", resp.Msg.Email)

    // Verify mock expectations
    mocks.Email.AssertExpectations(t)
}
```

## Integration Testing with Test Server

Use `NewTestXxxServer` for HTTP-level integration tests:

```go
func TestUsersServiceE2E(t *testing.T) {
    // Starts a real HTTP server with the service
    client, cleanup := app.NewTestUsersServiceServer(t)
    defer cleanup()

    // Test via the Connect RPC client
    createResp, err := client.CreateUser(context.Background(), connect.NewRequest(&usersv1.CreateUserRequest{
        Email: "test@example.com",
        Name:  "Test User",
    }))
    require.NoError(t, err)

    // Verify via another call
    getResp, err := client.GetUser(context.Background(), connect.NewRequest(&usersv1.GetUserRequest{
        Id: createResp.Msg.Id,
    }))
    require.NoError(t, err)
    assert.Equal(t, "test@example.com", getResp.Msg.Email)
}
```

## Testing with Real Dependencies

For integration tests with real databases:

```go
func TestIntegrationCreateUser(t *testing.T) {
    if testing.Short() {
        t.Skip("skipping integration test")
    }

    db := setupTestDB(t)

    svc := usersservice.New(usersservice.Deps{
        DB:     db,
        Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
        Email:  email.NewMockContract(t),
    })

    resp, err := svc.CreateUser(context.Background(), connect.NewRequest(&usersv1.CreateUserRequest{
        Email: "test@example.com",
        Name:  "Test User",
    }))
    require.NoError(t, err)

    // Verify in database
    getResp, err := svc.GetUser(context.Background(), connect.NewRequest(&usersv1.GetUserRequest{
        Id: resp.Msg.Id,
    }))
    require.NoError(t, err)
    assert.Equal(t, "test@example.com", getResp.Msg.Email)
}
```

## Test Fixtures

Use proto messages as fixtures:

```go
var testUser = &usersv1.CreateUserRequest{
    Email: "test@example.com",
    Name:  "Test User",
}

func TestWithFixture(t *testing.T) {
    svc, _ := app.NewTestUsersService(t)

    resp, err := svc.CreateUser(context.Background(), connect.NewRequest(testUser))
    require.NoError(t, err)
    // ...
}
```

## Table-Driven Tests

```go
func TestValidation(t *testing.T) {
    svc, _ := app.NewTestUsersService(t)

    tests := []struct {
        name    string
        request *usersv1.CreateUserRequest
        wantErr bool
        errCode connect.Code
    }{
        {
            name: "valid request",
            request: &usersv1.CreateUserRequest{
                Email: "test@example.com",
                Name:  "Test User",
            },
            wantErr: false,
        },
        {
            name: "missing email",
            request: &usersv1.CreateUserRequest{
                Name: "Test User",
            },
            wantErr: true,
            errCode: connect.CodeInvalidArgument,
        },
        {
            name: "invalid email",
            request: &usersv1.CreateUserRequest{
                Email: "not-an-email",
                Name:  "Test User",
            },
            wantErr: true,
            errCode: connect.CodeInvalidArgument,
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            _, err := svc.CreateUser(context.Background(), connect.NewRequest(tt.request))

            if tt.wantErr {
                require.Error(t, err)
                assert.Equal(t, tt.errCode, connect.CodeOf(err))
            } else {
                require.NoError(t, err)
            }
        })
    }
}
```

## Testing Internal Packages

Internal packages with `contract.go` interfaces are easily testable:

```go
func TestEmailService(t *testing.T) {
    svc := email.New(email.Deps{
        Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
        SMTPUrl: "smtp://localhost:1025",
    })

    err := svc.Send(context.Background(), "user@test.com", "Test Subject", "Test Body")
    require.NoError(t, err)
}
```

Use the generated mock in other tests:

```go
mockEmail := email.NewMockContract(t)
mockEmail.On("Send", mock.Anything, "user@test.com", mock.Anything, mock.Anything).Return(nil)

// Pass to service that depends on email
svc := usersservice.New(usersservice.Deps{
    Email: mockEmail,
})
```

## Running Tests

```bash
# All tests
forge test

# Unit tests only
forge test unit

# Integration tests
forge test integration

# With coverage
forge test --coverage

# Specific package
go test ./services/users/...
```

## Best Practices

1. **Use the test harness** — `app.NewTestXxx` for consistent mock setup
2. **Table-driven tests** — For validation logic
3. **Test via interfaces** — Use `contract.go` interfaces for internal packages
4. **Integration tests** — For critical paths with real databases
5. **Clean up** — Use `defer cleanup()` for test servers
6. **Context** — Always use context in tests
7. **Parallel tests** — Use `t.Parallel()` when safe

## See Also

- [Creating Services]({{< ref "creating-services" >}})
- [Service Communication]({{< ref "service-communication" >}})
