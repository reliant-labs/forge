# Forge

Forge is a production infrastructure generator for Go/Connect RPC applications. It generates middleware, mocks, observability, ORM code, test harnesses, CI/CD, and wiring from proto definitions — so you focus on business logic.

## Architecture

Proto is the canonical input format. Two lifecycles drive everything:

```
Proto IR (canonical input format)
├── proto/.plan/ (ephemeral, gitignored, deleted after scaffold)
│   └── entities.proto with forge.v1.entity + forge.v1.field annotations
│       → protoc-gen-forge --mode=scaffold (future)
│       → DB migration, internal/db/, handlers, pages, long-lived API proto
│
├── proto/services/ (long-lived, committed)
│   └── service.proto with forge.v1.service + forge.v1.method annotations
│       → protoc-gen-forge --mode=descriptor → forge_descriptor.json
│       → protoc-gen-forge --mode=orm → ORM code
│       → protoc-gen-go + protoc-gen-connect-go → stubs
│
└── internal/*/contract.go (Go interfaces, LLM-owned)
    → forge generate → mock_gen.go, middleware_gen.go, tracing_gen.go, metrics_gen.go
```

**Scaffold** (runs once per entity): plan proto → full skeleton → plan proto deleted.
**Generate** (runs always): service protos → stubs, hooks, mocks (idempotent, safe to re-run).

## Key Directories

| Directory | Purpose |
|-----------|---------|
| `proto/forge/v1/` | Forge annotation definitions (entity, field, service, method, config) |
| `internal/cli/` | CLI commands + protoc plugin (`protoc_gen_orm.go`, `orm_entity.go`, `orm_codegen.go`, `forge_descriptor.go`) |
| `internal/codegen/` | Canonical types (`ServiceDef`, `EntityDef`, `FieldKind`), CRUD generation, frontend codegen, template rendering |
| `internal/templates/` | Go templates for all generated code (project scaffolds, ORM, middleware, handlers) |
| `internal/generator/` | Project scaffolding, migration generation |
| `internal/linter/` | Contract linter (`RequireContractAnalyzer`, `ExportedVarsAnalyzer`), DB linter |
| `internal/config/` | `forge.yaml` types (`ProjectConfig`, `ContractsConfig`, etc.) |
| `gen/` | Generated Go types from forge's own protos (forge.v1) |
| `pkg/orm/` | Runtime ORM library (repository, query builder, dialect, cursor pagination, migrations) |

## Proto Annotations

All annotations live in `proto/forge/v1/forge.proto`. Import with:
```proto
import "forge/v1/forge.proto";
```

### Entity (on messages)
```proto
message Patient {
  option (forge.v1.entity) = {
    table: "patients"
    soft_delete: true
    timestamps: true
    middleware: ["tracing", "metrics"]
    indexes: [{fields: ["org_id", "email"], unique: true}]
  };
  // fields...
}
```

### Field (on fields)
```proto
string id = 1 [(forge.v1.field) = {pk: true}];
string org_id = 2 [(forge.v1.field) = {tenant: true, index: true}];
string email = 3 [(forge.v1.field) = {unique: true, validate: {required: true, format: "email"}}];
string doctor_id = 4 [(forge.v1.field) = {ref: "doctors.id"}];
```

Field storage for complex types:
```proto
map<string, string> metadata = 5 [(forge.v1.field) = {store: STORE_AS_JSONB}];
```

### Service (on services)
```proto
service PatientService {
  option (forge.v1.service) = {
    name: "PatientService"
    version: "v1"
    auth: {auth_required: true, auth_provider: "jwt"}
  };
}
```

### Method (on RPCs)
```proto
rpc CreatePatient(CreatePatientRequest) returns (CreatePatientResponse) {
  option (forge.v1.method) = {
    auth_required: true
    idempotency_key: true
    timeout: {seconds: 30}
  };
}
```

### Config (on fields)
```proto
message AppConfig {
  string database_url = 1 [(forge.v1.config) = {
    env_var: "DATABASE_URL"
    flag: "database-url"
    required: true
    description: "PostgreSQL connection string"
  }];
}
```

## protoc-gen-forge Plugin

The unified protoc plugin (`protoc-gen-forge`) replaces the old `protoc-gen-forge-orm`. It has three modes:

| Mode | Flag | Output | Purpose |
|------|------|--------|---------|
| `orm` (default) | `--mode=orm` | `*.pb.orm.go` | ORM repository code with tracing, tenant scoping, soft delete |
| `descriptor` | `--mode=descriptor` | `forge_descriptor.json` | JSON descriptor bridging proto definitions to the generate pipeline |
| `scaffold` | `--mode=scaffold` | _(future)_ | Full project scaffold from plan protos |

The plugin reads annotations via `proto.GetExtension()` with typed access to `forgev1.EntityOptions`, `forgev1.FieldOptions`, etc.

## FieldKind Type System

Every proto field is classified into a `FieldKind` for template branching:

| FieldKind | Example Proto Type | Go Type |
|-----------|--------------------|---------|
| `scalar` | `string`, `int64`, `bool` | `string`, `int64`, `bool` |
| `enum` | `Status` | `int32` (enum value) |
| `message` | `Address` | `*AddressPb` |
| `map` | `map<string, string>` | `map[string]string` |
| `repeated_scalar` | `repeated string` | `[]string` |
| `repeated_message` | `repeated Address` | `[]*AddressPb` |
| `wrapper` | `google.protobuf.StringValue` | `*string` |
| `timestamp` | `google.protobuf.Timestamp` | `*timestamppb.Timestamp` |

Templates branch on `Kind` to emit correct serialization, scanning, and comparison code.

## Contract Pattern

Internal packages (`internal/<name>/`) use Go interface contracts. The linter enforces:

1. **RequireContractAnalyzer**: Every `internal/` package with exported methods must have a `contract.go` file defining the interface.
2. **ExportedVarsAnalyzer**: No exported package-level variables (prevents global state).

Configuration in `forge.yaml`:
```yaml
contracts:
  strict: true              # require contract.go (default: true)
  allow_exported_vars: false # no exported vars (default: false)
  allow_exported_funcs: true # allow exported funcs (default: true)
  exclude:                   # packages that opt out
    - "internal/buildinfo"
```

`forge generate` reads `contract.go` interfaces and produces `mock_gen.go`, `middleware_gen.go`, `tracing_gen.go`, `metrics_gen.go`.

## Dev Workflow

```bash
# Build
go build ./...

# Test
go test ./...
go test ./internal/codegen/... ./internal/cli/... ./internal/linter/...

# Generate forge's own proto types
buf generate

# Lint
go vet ./...
```

## Testing

- **Unit tests**: Co-located with source files (`*_test.go`).
- **E2E scaffold tests**: `internal/cli/scaffold_*_e2e_test.go` — verify full project scaffolding produces valid, buildable output.
- **Golden file tests**: `internal/templates/testdata/golden/` — verify generated templates match expected output.
- **Linter tests**: `internal/linter/contract/testdata/` — test fixtures for contract analyzer.

Run all tests: `go test ./...`
Run specific: `go test ./internal/cli/... -run TestScaffold`

## Key Source Files

| File | What it does |
|------|-------------|
| `internal/cli/protoc_gen_orm.go` | Plugin entry point, mode dispatch |
| `internal/cli/orm_entity.go` | Entity/field parsing from proto descriptors |
| `internal/cli/orm_codegen.go` | ORM Go code emission (CRUD, tracing, tenant scoping) |
| `internal/cli/forge_descriptor.go` | Descriptor JSON extraction for generate pipeline |
| `internal/codegen/types.go` | Canonical types: `ServiceDef`, `EntityDef`, `FieldKind`, `ConfigField` |
| `internal/codegen/crud_gen.go` | CRUD template data building |
| `internal/config/config.go` | `forge.yaml` config types including `ContractsConfig` |
| `proto/forge/v1/forge.proto` | All forge annotation definitions |

## Rules

- Never hand-edit `gen/`. Run `buf generate` to regenerate.
- Proto annotations use `forge.v1` (not `forge.options.v1`).
- The protoc plugin is `protoc-gen-forge` (not `protoc-gen-forge-orm`).
- `forge_descriptor.json` bridges the protoc plugin to the generate pipeline — don't construct it manually.
- Contract enforcement is strict by default. Every `internal/` package needs `contract.go`.
- Templates branch on `FieldKind` — when adding a new type, update `DetermineFieldKind()` in `types.go`.
