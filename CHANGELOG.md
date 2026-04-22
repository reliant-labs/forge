# Changelog

All notable changes to Forge are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

### Added

- **CRUD codegen: pagination, filtering, multi-tenant, soft delete** — Generated CRUD handlers now support cursor-based pagination, field-level filtering, tenant-scoped queries via `tenant_key` annotation, and soft delete with `deleted_at` column management.
- **RBAC policy codegen from proto annotations** — `protoc-gen-forge-orm` reads `auth_required` and role annotations from proto method options and generates a per-method RBAC policy map, eliminating hand-written authorization boilerplate.
- **TypeScript React Query hooks auto-generation** — New codegen target produces typed React Query hooks (`useQuery`, `useMutation`) from Connect RPC service definitions, giving frontend teams type-safe data fetching out of the box.
- **Seed data generator from entity definitions** — `forge generate` can now produce seed data SQL or Go fixtures from proto entity definitions, pre-populating development databases with realistic test data.
- **API key lifecycle pack** — New `api-key` pack with SHA-256 hashed key storage, prefix-based lookup, create/revoke/rotate/list operations, scoped permissions, and auth middleware integration. Includes database migration.
- **Audit log pack with DB persistence** — New `audit-log` pack with a Connect interceptor that records every RPC call (caller, procedure, duration, status, trace ID) to both slog and a PostgreSQL `audit_log` table via async writes.
- **Structured logging with typed events** — Logging middleware now emits structured slog attributes with consistent field names (`procedure`, `duration`, `user_id`, `status`, `trace_id`), enabling reliable log parsing and alerting.
- **Grafana dashboard provisioning** — `forge generate` can produce a Grafana JSON dashboard with panels for RPC latency, error rates, and throughput, ready to import or provision via Grafana's API.

### Fixed

- **protoc-gen-forge-orm: os.Args handling** — Fixed the protoc plugin to correctly read from stdin as a protoc plugin rather than parsing `os.Args`, which caused failures when invoked by the protoc toolchain.
- **protoc-gen-forge-orm: duplicate shared headers** — Resolved an issue where shared file headers (imports, package declarations) were emitted multiple times when processing proto packages with multiple files.
- **protoc-gen-forge-orm: multi-file package support** — Fixed code generation to correctly handle proto packages split across multiple `.proto` files, merging generated output per Go package rather than per proto file.
- **CRUD handler idempotent regeneration** — `forge generate` no longer duplicates handler registrations or overwrites user-edited handler files on repeated runs. Generated files use `_gen.go` suffix and are safe to regenerate.
- **Test file naming convention** — Generated test files now consistently use the `_test.go` suffix required by the Go toolchain, fixing cases where tests were silently excluded from `go test`.
