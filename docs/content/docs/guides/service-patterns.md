---
title: "Service Patterns"
description: "Common service implementation patterns"
weight: 80
---

# Service Patterns

Common patterns for implementing Forge services.

## CRUD Service Pattern

```go
type Deps struct {
    DB *pgxpool.Pool
}

type Service struct {
    deps Deps
}

func New(deps Deps) *Service {
    return &Service{deps: deps}
}

func (s *Service) CreateUser(
    ctx context.Context,
    req *connect.Request[usersv1.CreateUserRequest],
) (*connect.Response[usersv1.CreateUserResponse], error) {
    // Create
}

func (s *Service) GetUser(
    ctx context.Context,
    req *connect.Request[usersv1.GetUserRequest],
) (*connect.Response[usersv1.GetUserResponse], error) {
    // Read
}

func (s *Service) UpdateUser(
    ctx context.Context,
    req *connect.Request[usersv1.UpdateUserRequest],
) (*connect.Response[usersv1.UpdateUserResponse], error) {
    // Update
}

func (s *Service) DeleteUser(
    ctx context.Context,
    req *connect.Request[usersv1.DeleteUserRequest],
) (*connect.Response[usersv1.DeleteUserResponse], error) {
    // Delete
}

func (s *Service) ListUsers(
    ctx context.Context,
    req *connect.Request[usersv1.ListUsersRequest],
) (*connect.Response[usersv1.ListUsersResponse], error) {
    // List with pagination
}
```

## Service with Dependencies

```go
type Deps struct {
    DB            *pgxpool.Pool
    Logger        *slog.Logger
    UsersClient   usersv1connect.UsersServiceClient
    PaymentClient paymentv1connect.PaymentServiceClient
}

type Service struct {
    deps Deps
}

func New(deps Deps) *Service {
    return &Service{deps: deps}
}
```

## Validation Pattern

```go
func (s *Service) CreateUser(
    ctx context.Context,
    req *connect.Request[usersv1.CreateUserRequest],
) (*connect.Response[usersv1.CreateUserResponse], error) {
    if err := validateCreateUserRequest(req.Msg); err != nil {
        return nil, connect.NewError(connect.CodeInvalidArgument, err)
    }

    // Process...
}

func validateCreateUserRequest(req *usersv1.CreateUserRequest) error {
    if req.Email == "" {
        return fmt.Errorf("email is required")
    }
    if !isValidEmail(req.Email) {
        return fmt.Errorf("invalid email format")
    }
    return nil
}
```

## Error Handling Pattern

```go
func (s *Service) GetUser(
    ctx context.Context,
    req *connect.Request[usersv1.GetUserRequest],
) (*connect.Response[usersv1.GetUserResponse], error) {
    user, err := db.GetUserByID(ctx, s.deps.DB, req.Msg.Id)
    if err == sql.ErrNoRows {
        return nil, connect.NewError(connect.CodeNotFound, fmt.Errorf("user not found"))
    }
    if err != nil {
        s.deps.Logger.Error("database error", "error", err)
        return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("internal server error"))
    }

    return connect.NewResponse(&usersv1.GetUserResponse{User: toProto(user)}), nil
}
```

## Transaction Pattern

```go
func (s *Service) TransferFunds(
    ctx context.Context,
    req *connect.Request[transferv1.TransferRequest],
) (*connect.Response[transferv1.TransferResponse], error) {
    err := orm.RunInTx(ctx, s.deps.DB, func(tx pgx.Tx) error {
        if err := db.UpdateBalance(ctx, tx, req.Msg.FromAccount, -req.Msg.Amount); err != nil {
            return err
        }
        if err := db.UpdateBalance(ctx, tx, req.Msg.ToAccount, req.Msg.Amount); err != nil {
            return err
        }
        return nil
    })
    if err != nil {
        return nil, connect.NewError(connect.CodeInternal, err)
    }

    return connect.NewResponse(&transferv1.TransferResponse{Success: true}), nil
}
```

## Pagination Pattern

```go
func (s *Service) ListUsers(
    ctx context.Context,
    req *connect.Request[usersv1.ListUsersRequest],
) (*connect.Response[usersv1.ListUsersResponse], error) {
    pageSize := req.Msg.PageSize
    if pageSize == 0 {
        pageSize = 10
    }
    if pageSize > 100 {
        pageSize = 100
    }

    offset, _ := decodePageToken(req.Msg.PageToken)

    users, err := db.ListUsers(ctx, s.deps.DB,
        orm.WithLimit(int(pageSize+1)),
        orm.WithOffset(offset),
        orm.WithOrderBy("created_at", orm.Desc),
    )
    if err != nil {
        return nil, connect.NewError(connect.CodeInternal, fmt.Errorf("query failed"))
    }

    var nextPageToken string
    if len(users) > int(pageSize) {
        users = users[:pageSize]
        nextPageToken = encodePageToken(offset + int(pageSize))
    }

    return connect.NewResponse(&usersv1.ListUsersResponse{
        Users:         toProtoUsers(users),
        NextPageToken: nextPageToken,
    }), nil
}
```

## See Also

- [Creating Services]({{< ref "creating-services" >}})
- [Service Communication]({{< ref "service-communication" >}})
