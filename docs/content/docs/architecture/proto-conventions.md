---
title: "Proto Conventions"
description: "Forge's protocol buffer conventions and best practices"
weight: 40
---

# Proto Conventions

Forge enforces specific proto conventions to ensure consistency and optimal code generation. Proto is used for **external boundaries** (service RPCs and config). Internal package boundaries use Go interfaces instead.

## File Organization

```
proto/
├── config/v1/             # Config — instantiation contract
│   └── config.proto
├── services/              # Service RPC definitions
│   ├── users/
│   │   └── v1/
│   │       └── users.proto
│   └── orders/
│       └── v1/
│           └── orders.proto
└── db/                    # Database entities (deprecated, ORM-owned)
    └── v1/
        └── entities.proto
```

## Package Naming

```protobuf
// Format: services.{name}.{version}
package services.users.v1;

option go_package = "github.com/myorg/myapp/gen/services/users/v1;usersv1";
```

## Service Definitions

### Standard RPC Pattern

```protobuf
service UsersService {
  rpc CreateUser(CreateUserRequest) returns (CreateUserResponse);
  rpc GetUser(GetUserRequest) returns (GetUserResponse);
  rpc ListUsers(ListUsersRequest) returns (ListUsersResponse);
  rpc UpdateUser(UpdateUserRequest) returns (UpdateUserResponse);
  rpc DeleteUser(DeleteUserRequest) returns (DeleteUserResponse);
}
```

### Request/Response Naming

```protobuf
// Pattern: {MethodName}Request / {MethodName}Response
message CreateUserRequest {
  string name = 1;
  string description = 2;
}

message CreateUserResponse {
  User user = 1;
}
```

## Field Conventions

### Timestamps

```protobuf
import "google/protobuf/timestamp.proto";

message Event {
  google.protobuf.Timestamp created_at = 1;
  google.protobuf.Timestamp updated_at = 2;
}
```

### Enums

```protobuf
enum Status {
  STATUS_UNSPECIFIED = 0;  // Always include zero value
  STATUS_PENDING = 1;
  STATUS_ACTIVE = 2;
  STATUS_ARCHIVED = 3;
}
```

## Database Entity Annotations

Proto DB entities use `forge.options.v1` annotations. Note: these annotations are **deprecated in favor of the migration-first ORM workflow** where SQL migrations are the source of truth. They remain available for generating initial ORM code:

```protobuf
import "forge/options/v1/entity.proto";
import "forge/options/v1/field.proto";

message User {
  option (forge.options.v1.entity_options) = {
    table_name: "users"
    timestamps: true
  };

  string id = 1 [(forge.options.v1.field_options) = {
    primary_key: true
    not_null: true
  }];

  string email = 2 [(forge.options.v1.field_options) = {
    not_null: true
  }];
}
```

## Comments and Documentation

```protobuf
// UsersService manages user accounts.
service UsersService {
  // CreateUser creates a new user account.
  rpc CreateUser(CreateUserRequest) returns (CreateUserResponse);
}
```

## Versioning

```protobuf
// V1 — initial version
package services.users.v1;

// V2 — breaking changes
package services.users.v2;
```

## Linting

```bash
# Check proto conventions
forge lint

# Contract enforcement
forge lint --contract

# Proto-specific linting via buf
buf lint
```

## Best Practices

1. **Always version packages** — Use `/v1`, `/v2` suffixes
2. **Zero values** — First enum value must be `_UNSPECIFIED = 0`
3. **Comments** — Document all services and methods
4. **Breaking changes** — Check with `buf breaking`
5. **Use proto for external APIs only** — Internal packages use Go interfaces in `contract.go`
6. **Commit generated code** — Makes builds reproducible

## See Also

- [Code Generation]({{< ref "code-generation" >}})
- [Creating Services]({{< ref "../guides/creating-services" >}})
