# Browse-only RPC and code catalog

`forge generate` now writes `gen/forge_service_catalog.json` when it generates
an application's typed composition graph. The public contract is
`pkg/servicecatalog.Catalog`, schema `forge.dev/service-catalog/v1`; a small
example is in `pkg/servicecatalog/testdata/example.json`. No server, live LSP,
RPC execution endpoint, endpoint registry or new database is involved.

## What it says

- Processes are **module + composition root**, with the root's command path.
  One executable's `server api` and `server worker` remain separate. Reach comes
  from the existing typed graph and reconciled `compose.go`, not filename guesses.
- `serves` lists reachable generated mountkit route sets and their fully qualified
  proto services/audiences. This is a declared route-set capability, **not proof
  a live listener mounted it**. Custom/dynamic mounting is not inferred.
- Service methods retain fully qualified input/output names, both streaming
  flags, auth declaration and protobuf idempotency as the closed set `UNKNOWN`,
  `IDEMPOTENT`, `NO_SIDE_EFFECTS` (protobuf's `IDEMPOTENCY_UNKNOWN` is `UNKNOWN`).
  Each service's `descriptor` is the existing `forge_descriptor.json` ServiceDef:
  `Schemas`, `SchemaFiles`, `Enums`, `validate` and `validate_options` are reused
  verbatim. There is no second validation/form dialect or invocation handle.
- Components expose typed method signatures and module-relative source locations
  (see "Source locations" below).
  Consumed interfaces expose their method sets. Assignable producers and local
  concrete `implementation_candidates` are **candidates**, not proof a constructor
  selected that concrete type. `bindings.providers` lists graph accessors in that
  field's composition expression; `composition_expression` describes that evidence,
  not runtime selection. A conditional/factory can select differently at runtime.
  Expressions with no identifiable accessor remain `unresolved`, even if exactly
  one candidate happens to exist. Missing types carry diagnostics, not guesses.
- Client reach means **can-call a service**, never “called every method”. Observed
  calls still come from telemetry. No static per-method call edges are fabricated.

## Source locations

The committed `gen/` file names each declaration by **path and symbol**, never by
line: `{"path": "internal/handlers/orders/service.go", "symbol": "Service.Get"}`.
A symbol is `Name` (a func or type) or `Type.Name` (a method of a concrete type,
or of the named interface that declares it). A line number moves whenever code
above it does, so a committed catalog that recorded one went stale on every
merge ahead of yours that touched a handler file, and failed `verify-generated`
in the merge group. Moving, reordering or editing around declarations now
leaves the file byte-identical; only a change to what is declared (an rpc, a
method, a signature, a binding) changes it.

The bundle copy (`forge/service-catalog.json`) carries the line its readers
expect, `{"path": ..., "line": N}`: the build resolves each symbol against the
source being built (`servicecatalog.Locate`) and drops the symbol. A symbol the
build cannot find becomes the unknown source `{"path": "", "line": 0}`. The
bundle's layer gate (`servicecatalog.DecodeLocated`) refuses a symbolic source.
A client registration is located at the provider func that makes it, and its
id is `<provider>:<func>#<n>`, the n-th registration in that func.

Generation is deterministic for the same source/build inputs. External-module
source locations are omitted; no machine paths, URL values, config defaults,
headers, credentials, source bodies or runtime observations are copied into it.
Generated catalogs must be regenerated after code/proto changes, just like the
existing descriptor and composition outputs. They are not a live reflection API.

## Opt an existing client in

Keep the application's current HTTP client, timeout, Connect options, signing,
authentication and tracing interceptors. Only wrap the constructor call in the
**owned provider**; the return value stays the generated Connect interface:

```go
func NewOrders(d OrdersDeps) ordersconnect.OrdersServiceClient {
    return servicecatalog.NewClient(
        servicecatalog.ClientBinding{
            Service: ordersconnect.OrdersServiceName,
            ConfigField: "shop.config.v1.API.orders_url",
        },
        ordersconnect.NewOrdersServiceClient,
        d.HTTPClient, d.Config.GetOrdersUrl(),
        d.Options...,
    )
}
```

`ConfigField` names the **existing** fully qualified protobuf config field. It
must resolve in the descriptor to a non-sensitive string field. Regenerate an
older descriptor to populate ConfigMessage.FullName. The generated Connect
service-name constant and constructor must agree. Nonliteral/dynamic binding
metadata, missing config fields, sensitive fields and mismatched constructors
produce an unresolved diagnostic and do not export their values. Do not use a
URL as metadata. The helper itself delegates directly, preserves option order,
and does not install interceptors or alter construction/failure behavior.

The resolved endpoint still comes from existing config loading. If that field is
authored as `forge.WorkloadURL`, the existing rendered deployment inventory owns
its workload reference and URL lowering. Catalog consumers can join the **field
reference** to that inventory; this catalog does not resolve URLs, infer targets
by service name, enable private/cross-environment discovery or supersede routing.
Literal configured URLs remain external/unresolved targets. Registration is
metadata, not authorization to impersonate that provider.

## Existing bundle artifact path

Build/cut collects the generated catalog into **the existing manifest layer** at
`forge/service-catalog.json` (`release.BundleServiceCatalogPath`). It is a sibling
of `forge/frontend-declarations.json`, outside every reconciler-applied
`manifests/` tree. It affects the existing OCI bundle digest, not config digest,
shape, workload images or version assignments. The ingest layer gate validates
its schema and relative, located sources with `servicecatalog.DecodeLocated`. The
catalog has no per-file bound of its own; the layer total
(`release.ManifestsLayerMaxBytes`) bounds it.

### The bundle copy is reduced

The bundle does not carry `gen/`'s catalog whole. It carries what a hosted reader
serves to a browser (`servicecatalog.ForBundle`), so its size follows the RPC
surface rather than the code graph:

- each service's `descriptor` keeps only `Schemas`, the request/response shapes
  the RPC explorer renders;
- the code method sets of components, implementation candidates and bindings
  are empty, and the `bundle_method_sets_omitted` diagnostic says so: an empty
  list there means "not carried", not "no methods". Every component, binding,
  candidate, provider, client and process stays;
- `idempotency` is the closed set `UNKNOWN`, `IDEMPOTENT`, `NO_SIDE_EFFECTS`.

It is still schema v1. For reliant this is 2.1 MB → 0.38 MB compact. The full
code graph (signatures, method ids, scopes) stays in `gen/forge_service_catalog.json`.

A hosted consumer verifies the existing bundle's digest/layer using existing
artifact admission, authorizes project/environment access, and then reads that
exact path and calls `servicecatalog.Decode`. Absence means metadata unavailable,
not an empty live graph. No new assignment store is required. This binds metadata
to those bundle bytes; it does **not** attest a process's loaded image/config or
provide a deployed-version lens. Mixed/scoped promotions must not be treated as
proof that every declared process runs these bytes.

## Code telemetry join

Generated contract decorators attach a bounded `forge.method.id` span attribute
using `servicecatalog.MethodID(scope, interfaceName, methodName)`. It is stable
across line/body/signature changes and contains no build/request IDs. Generated
IDs decorate the existing chain rather than replacing owned middleware. Span
names, tracer scopes and metric `method` dimensions stay unchanged. When IDs are
absent (older/disowned/custom instrumentation), use the catalog's `scope` and
`legacy_name` as fallback evidence; ambiguity remains unknown. Concrete candidate
methods have their own receiver IDs and are not silently equated to interfaces.
The method sets that carry these ids are in `gen/`'s catalog; the bundle copy
omits them (see "The bundle copy is reduced"), so a hosted join that needs them
must ship them in their own artifact rather than re-grow this one.

## Adoption remaining

Existing applications must pin this Forge change, regenerate, and opt their
actual outbound providers into the helper. This change does not invent or edit
Reliant/control-plane registrations, custom mounts or workload endpoint mappings.
An application still using legacy composition has no typed process catalog until
it adopts the existing graph. The descriptor alone is not a process inventory.
