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
  flags, auth declaration and protobuf idempotency (`IDEMPOTENCY_UNKNOWN`,
  `IDEMPOTENT`, `NO_SIDE_EFFECTS`; older descriptor fallback is `UNKNOWN`).
  Each service's `descriptor` is the existing `forge_descriptor.json` ServiceDef:
  `Schemas`, `SchemaFiles`, `Enums`, `validate` and `validate_options` are reused
  verbatim. There is no second validation/form dialect or invocation handle.
- Components expose typed method signatures and module-relative source locations.
  Consumed interfaces expose their method sets. Assignable producers and local
  concrete `implementation_candidates` are **candidates**, not proof a constructor
  selected that concrete type. `bindings.providers` lists graph accessors in that
  field's composition expression; `composition_expression` describes that evidence,
  not runtime selection. A conditional/factory can select differently at runtime.
  Expressions with no identifiable accessor remain `unresolved`, even if exactly
  one candidate happens to exist. Missing types carry diagnostics, not guesses.
- Client reach means **can-call a service**, never “called every method”. Observed
  calls still come from telemetry. No static per-method call edges are fabricated.

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
its schema and relative sources with `servicecatalog.Decode`. The catalog obeys
the existing 1 MiB per-file bound; oversize generation fails explicitly.

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

## Adoption remaining

Existing applications must pin this Forge change, regenerate, and opt their
actual outbound providers into the helper. This change does not invent or edit
Reliant/control-plane registrations, custom mounts or workload endpoint mappings.
An application still using legacy composition has no typed process catalog until
it adopts the existing graph. The descriptor alone is not a process inventory.
