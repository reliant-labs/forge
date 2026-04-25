---
name: forge-dev
description: Development guide for working ON forge itself — plugin internals, template system, testing.
---
# Working on Forge

## Architecture

- **protoc-gen-forge**: Unified protoc plugin with `mode=orm` and `mode=descriptor`. Entry point: `internal/cli/protoc_gen_orm.go`.
- **Proto descriptors**: Typed extension access via `proto.GetExtension()` with `forgev1.E_Entity`, `forgev1.E_Field`, `forgev1.E_Service`, `forgev1.E_Method`, `forgev1.E_Config`.
- **FieldKind type system**: Classifies every field as `scalar`, `enum`, `message`, `map`, `repeated_scalar`, `repeated_message`, `wrapper`, or `timestamp`. Templates branch on Kind.
- **forge_descriptor.json**: Bridges protoc plugin output to the `generate.go` pipeline. Contains `ServiceDef`, `EntityDef`, and `ConfigMessage` arrays.
- **Templates**: Go text/template files in `internal/templates/` with FuncMap helpers. Rendered by `internal/codegen/templates.go`.

## Key Files

| File | Purpose |
|------|---------|
| `internal/cli/protoc_gen_orm.go` | Plugin entry point, mode dispatch (orm/descriptor/scaffold) |
| `internal/cli/orm_entity.go` | Entity/field parsing from proto descriptors (`parseEntity`, `parseField`) |
| `internal/cli/orm_codegen.go` | ORM Go code emission (CRUD, tracing, tenant scoping, soft delete) |
| `internal/cli/forge_descriptor.go` | Descriptor extraction → `forge_descriptor.json` |
| `internal/codegen/types.go` | Canonical types: `ServiceDef`, `EntityDef`, `FieldKind`, `ConfigField` |
| `internal/codegen/crud_gen.go` | CRUD template data building |
| `internal/codegen/entity_convert.go` | Entity conversion utilities |
| `internal/config/config.go` | `forge.yaml` types (`ProjectConfig`, `ContractsConfig`, etc.) |
| `proto/forge/v1/forge.proto` | All annotation definitions (entity, field, service, method, config) |
| `gen/forge/v1/forge.pb.go` | Generated Go types from forge.proto |
| `internal/linter/contract/require_contract.go` | RequireContractAnalyzer |
| `internal/linter/contract/exported_vars.go` | ExportedVarsAnalyzer |

## Adding a New Annotation

1. Add the field to the appropriate message in `proto/forge/v1/forge.proto` (e.g., add to `EntityOptions`, `FieldOptions`, etc.)
2. Run `buf generate` to update `gen/forge/v1/forge.pb.go`
3. Read the new field in `orm_entity.go` (`parseEntity` or `parseField`) and/or `forge_descriptor.go` (extraction functions)
4. Add the field to the relevant struct in `internal/codegen/types.go` if it needs to flow through to templates
5. Pass it through to templates via the template data structs in `crud_gen.go` or similar
6. Update templates in `internal/templates/` to use the new field
7. Add tests in `internal/cli/orm_entity_test.go` and/or `internal/codegen/` test files

## Adding a New FieldKind

1. Add the constant to `FieldKind` in `internal/codegen/types.go`
2. Update `DetermineFieldKind()` to classify the new kind
3. Update ORM codegen in `internal/cli/orm_codegen.go` — the `generateEntityCode` function branches on field properties
4. Update templates that branch on Kind (search for `.Kind` in `internal/templates/`)
5. Add test cases in `internal/codegen/types_test.go`

## Adding a New Plugin Mode

1. Add the constant to `forgePluginMode` in `protoc_gen_orm.go`
2. Add the case to the `opts.Run` switch in `newProtocGenForgeCmd`
3. Create a `generate<Mode>` function (see `generateDescriptor` as a template)
4. Wire it in `buf.gen.yaml` for the target project with `opt: mode=<name>`

## Contract Linter

The contract linter (`cmd/contractlint/main.go`) runs two analyzers:

- **RequireContractAnalyzer**: `internal/` packages with exported methods must have `contract.go`
- **ExportedVarsAnalyzer**: No exported package-level variables

Config via `forge.yaml`:
```yaml
contracts:
  strict: true
  allow_exported_vars: false
  exclude: ["internal/buildinfo"]
```

Test fixtures: `internal/linter/contract/testdata/`

## Testing

```bash
# All tests
go test ./...

# Plugin and codegen
go test ./internal/codegen/... ./internal/cli/...

# Contract linter
go test ./internal/linter/...

# E2E scaffold tests (slower — scaffold and build full projects)
go test ./internal/cli/... -run TestScaffold

# ORM runtime library
go test ./pkg/orm/...
```

## Build

```bash
go build ./...     # verify everything compiles
buf generate       # regenerate forge's own proto types
buf lint           # lint proto files
```

## Project Structure

```
proto/forge/v1/forge.proto      # Annotation definitions (THE source of truth)
gen/forge/v1/                   # Generated Go types (from buf generate)
internal/cli/                   # CLI commands + protoc plugin
internal/codegen/               # Types, CRUD generation, frontend codegen
internal/templates/             # Go templates for all generated code
internal/generator/             # Project scaffolding, migration generation
internal/linter/                # Contract and DB linters
internal/config/                # forge.yaml types
pkg/orm/                        # Runtime ORM library
cmd/forge/                      # Main CLI binary
cmd/contractlint/               # Standalone contract linter binary
```
