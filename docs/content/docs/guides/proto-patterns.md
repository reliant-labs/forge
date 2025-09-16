---
title: "Proto Patterns"
description: "Common protobuf patterns for Forge services"
weight: 70
---

# Proto Patterns

Common patterns and best practices for protocol buffer definitions in Forge.

## Request/Response Pattern

```protobuf
message CreateUserRequest {
  string email = 1;
  string name = 2;
}

message CreateUserResponse {
  User user = 1;
}
```

## Pagination

```protobuf
message ListUsersRequest {
  int32 page_size = 1;
  string page_token = 2;
}

message ListUsersResponse {
  repeated User users = 1;
  string next_page_token = 2;
  int32 total_count = 3;
}
```

## Filtering and Sorting

```protobuf
message ListUsersRequest {
  string filter = 1;    // e.g., "status:active AND role:admin"
  string order_by = 2;  // e.g., "created_at DESC"
  int32 page_size = 3;
  string page_token = 4;
}
```

## Field Masks

```protobuf
import "google/protobuf/field_mask.proto";

message UpdateUserRequest {
  User user = 1;
  google.protobuf.FieldMask update_mask = 2;
}
```

## Timestamps

```protobuf
import "google/protobuf/timestamp.proto";

message User {
  google.protobuf.Timestamp created_at = 10;
  google.protobuf.Timestamp updated_at = 11;
  google.protobuf.Timestamp deleted_at = 12;  // Soft delete
}
```

## Enums

```protobuf
enum UserStatus {
  USER_STATUS_UNSPECIFIED = 0;
  USER_STATUS_ACTIVE = 1;
  USER_STATUS_SUSPENDED = 2;
  USER_STATUS_DELETED = 3;
}

message User {
  UserStatus status = 5;
}
```

## Nested Messages

```protobuf
message User {
  message Address {
    string street = 1;
    string city = 2;
    string postal_code = 3;
  }

  Address billing_address = 10;
  Address shipping_address = 11;
}
```

## One-of Fields

```protobuf
message SearchRequest {
  oneof query {
    string text_query = 1;
    int64 id_query = 2;
    EmailQuery email_query = 3;
  }
}
```

## Validation

```protobuf
import "buf/validate/validate.proto";

message CreateUserRequest {
  string email = 1 [(buf.validate.field).string = {
    email: true
    min_len: 3
    max_len: 255
  }];

  string name = 2 [(buf.validate.field).string = {
    min_len: 1
    max_len: 100
  }];
}
```

## See Also

- [Proto Conventions]({{< ref "../architecture/proto-conventions" >}})
- [Creating Services]({{< ref "creating-services" >}})
