# Proposal: connect-go v2 support, side by side with v1

**Status:** design for the implementing PR. The forge owner approved the move (v1 and v2 supported side by side). The decisions in section 7 are still open.
**Date:** 2026-11-19
**Scope:** forge (`pkg/`, templates, codegen, CLI, analyzers). Sections 5 and 6 describe how projects and the two consumers (reliant, control-plane) migrate afterwards; that work is not part of the forge PR.

## 1. Summary and why

forge generates and runs Connect RPC services on `connectrpc.com/connect` v1. connect-go v2 (`connectrpc.com/connect/v2`, currently v2.0.2) is a redesign of the Go API on the same wire protocol. forge's own `go.mod` already requires both majors (`connect v1.21.1`, `connect/v2 v2.0.2`, `otelconnect v0.12.0`, `vanguard v0.5.0`); `pkg/serverkit/rest.go` already uses v2 as the vanguard bridge. What is missing is everything else: generated code, `forge/pkg`, tool installation, analyzers.

What v2 gives a forge project:

- **Simple-style handlers.** A handler is `func(ctx, *pb.Req) (*pb.Resp, error)`. No `connect.Request[T]` / `connect.Response[T]` wrappers, no `.Msg`, no `connect.NewResponse`. Most of forge's generated handler code (CRUD shims, scaffolded stubs, tdd rows) gets shorter.
- **Uniform function interceptors.** `connect.ServerInterceptor` and `connect.ClientInterceptor` are `func(next) next` over every RPC kind. forge's ~45 interceptor constructors stop having to implement three methods (unary, streaming client, streaming handler) each.
- **`CallInfo` metadata.** Headers, trailers and peer address are read from a `*connect.CallInfo` on the context instead of from request/response wrappers.
- **Bounded read limits by default.** 4 MiB per message on servers and clients; v1 was unbounded, so any caller could make a server buffer an arbitrary message.
- **Plain errors never leak.** An error returned from a handler that is not a `*connect.Error` is not serialized in v2. In v1 its `Error()` text reached the client as `CodeUnknown`.
- **In-process transport.** `connectinprocess` connects a client to a `*connect.Server` without a listener, which makes forge's generated component tests faster and less flaky.
- **vanguard v0.5 is native.** vanguard v0.5 is v2-only and mounts REST routes straight from a `*connect.Server`; a v2 project needs no v1-to-v2 bridge.

connect-go v1 remains supported upstream (fixes and security updates on the v1 branch). Nothing here forces a project off v1. This proposal makes v2 available, keeps v1 fully working, and gets the two consumers onto v2 on their own schedule.

## 2. Constraints

1. **forge is semver, major locked at 1.** A forge release must not break a project that is still on connect v1. Every v1 symbol in `forge/pkg` keeps its signature and behaviour; every v1 template keeps rendering for a v1 project. v1 tests pass unmodified.
2. **The wire must not change.** reliant and control-plane serve web (connect-es), Electron, the Go SDK and old daemon builds that are already deployed. Header names, content types, envelope flags, error JSON, gRPC trailers and codecs are identical in v1 and v2 (`wire-compat.md`). Only defaults differ; section 4(e) pins each.
3. **One major per project.** The v1 and v2 `protoc-gen-connect-go` plugins emit the same package name (`<pkg>v1connect`) and the same file name, so one project generates one major. Mid-migration, a module can import both runtimes (v2 says so), but its generated packages are one or the other.
4. **No `Deprecated:` marker yet.** consumers run staticcheck SA1019 on non-test code, so a machine-readable marker on v1 `forge/pkg` APIs would turn every v1 consumer's lint red when it pins the release. v1 APIs get a prose note in this release (section 4(f)).
5. **forge's own lint rollout rule applies.** A new lint rule or a tightened one needs a row in `internal/cli/lint/lint_rollout_table.go` and warns before it gates.

## 3. Breaking changes, v1 to v2, and how forge handles each

Source: the v2 migration guide plus the inventories. "Pkg" means `forge/pkg` packages; "Codegen" means `internal/templates`, `internal/codegen`, `internal/cli`.

| # | Change (v1 to v2) | forge area affected | How forge handles it |
|---|---|---|---|
| 1 | Module path `connectrpc.com/connect` becomes `connectrpc.com/connect/v2`; `grpchealth` and `grpcreflect` also move to `/v2`; `validate` (v0.8+) and `otelconnect` (v0.11+) keep their paths and gain `NewServerInterceptor`/`NewClientInterceptor`; `authn` stays on v1 | Pkg (14 packages expose v1 types); Codegen (`go.mod.tmpl`, `gen-go.mod.tmpl`, `tools.go.tmpl`); linters keyed on the import path | Pkg: V2 symbols beside v1 ones using `connectv2`/`connecthttp`/`connectv2proto` aliases (4(b)). Codegen: import path chosen by `connectmajor` (4(a)). `validate` and `otelconnect` v0.12 already support both; forge's `pkg/validate` adopts `validate.NewServerInterceptor`. forge's `pkg/authn` is forge's own package (not connectrpc `authn`) and gets a V2 twin |
| 2 | Runtime split into `connect/v2`, `connecthttp`, `connectproto`, `connectgzip`, `connectinprocess`; core no longer imports `net/http` | Pkg; templates | v2 templates import `connect/v2` in generated code and `connecthttp` in wiring. Tests use `connectinprocess` where no HTTP behaviour is under test |
| 3 | `buf.gen.yaml`: remote plugin pin moves to `connectrpc/go:v2.x`, `gosimple` plugin replaced, `simple=true` is rejected; local `protoc-gen-connect-go` entry unchanged but must be reinstalled from the v2 module; `go tool` form needs the v2 tool dependency | Codegen: `internal/cli/tools.go` (`requiredProtoTools`), `generate_toolpins.go`, `generate_cache.go`, `generator/dx_files.go:315` (`@latest` v1 install), `templates/project/tools.go.tmpl`, `generate_buf_preflight.go` (`goToolPackageHint`) | forge's `buf.gen.yaml` (`local: protoc-gen-connect-go`, `paths=source_relative`) is already major-agnostic and is not touched. Tool install is chosen per project: v2 uses module `connectrpc.com/connect/v2/cmd/protoc-gen-connect-go` with version module `connectrpc.com/connect/v2`, and the module path goes into the cache key (4(a)). Preflight warns when `buf.gen.yaml` pins a remote v1 plugin for a v2 project |
| 4 | Generated code is always simple-style; handler interfaces take and return bare messages | Codegen: `service/handlers.go.tmpl`, `handlers_methods.go.tmpl`, `handlers_crud_shim_method.go.tmpl`, `unit_test.go.tmpl`, `handlers_crud_test.go.tmpl`; `internal/codegen/generator.go:422-451`, `service_stub_gen.go`, `crud_gen.go`, `scaffold/rpc.go`; golden `handlers_methods.go.golden` | Every template and Go emitter that writes a handler signature branches on the detected major (4(g)). v1 output stays byte-identical, pinned by the existing golden |
| 5 | `connect.Request[T]` / `connect.Response[T]` / `NewRequest` / `NewResponse` removed | Pkg: `crud.Handle*` (35 template uses), `tdd.Case`/`HandlerFunc`/`RunRPCCases`, `testkit`; Codegen: `crud_owner_source.go:286` (helper detection by `connect.Request` selector), `project_migrate_tdd.go` | Connect-neutral simple-style cores in `crud` and `tdd` that the v1 symbols wrap (4(b)). `crud_owner_source.go` learns the bare `*pb.T` helper shape. `project_migrate_tdd.go` skips (does not crash on) v2 code |
| 6 | Server construction: `NewXHandler(impl, opts) (path, http.Handler)` becomes `connect.NewServer(interceptors...)` + `RegisterXHandler(server, impl)` + `connecthttp.Mount(mux, server, opts...)` | Pkg: `mountkit` (`Registrar`, `WithHandlerOptions`, `Resolve`, `RegisterService`), `serverkit` (`Config.HandlerOpts`, `Routes`, `RoutesFunc`, `Listener`, `Server.Mount`); Codegen: `service/service.go.tmpl` (user-owned `Register`), `mounts_services_gen.go.tmpl`, `routes_gen.go.tmpl`, `providers-graph.go.tmpl`, `cmd-svc-mount-gen.go.tmpl`, `cmd-tree-serve.go.tmpl`, `component_test_helpers.go.tmpl`, `codegen/templates.go` (mock) | The mount contract becomes per-`*connect.Server`: `mountkit.RegistrarV2`, `RegisterServiceV2`, `serverkit.RoutesV2` (4(c)). Generated mounts group services by interceptor chain, one Server per distinct chain on one mux. The user-owned `service.go` `Register` changes shape; this is the single largest hand-written migration item for a project (section 5) |
| 7 | Interceptors are per-Server, not per-handler: `connect.WithInterceptors(...)` on a handler becomes an argument of `connect.NewServer`; other handler options move to `connecthttp.Mount` | Pkg: `mountkit.WithHandlerOptions`, `serverkit.Config.HandlerOpts`, `Chain.HandlerOptions()` | Per-service interceptors force one Server per distinct chain (4(c)). `WithServerInterceptors` and `WithHTTPOptions` replace `WithHandlerOptions` in the V2 mount API |
| 8 | Client construction: `NewXClient(httpClient, baseURL, opts...)` becomes `NewXClient(*connect.Client)`, where the Client wraps `connecthttp.NewTransport(httpClient, baseURL, opts...)`; interceptors move to `connect.NewClient` | Pkg: `servicecatalog.NewClient`, `testkit.NewConnectClient`, `observe` client config (`Extras`, `ClientOptions`); Codegen: `component_test_helpers.go.tmpl`, `test/e2e/helpers_test.go.tmpl` | `servicecatalog.NewClientV2` takes `func(*connectv2.Client) T`, transport options and client interceptors. forge-built v2 clients default to `connecthttp.WithReadMaxBytes(0)` (4(e)). e2e helper template emits the v2 construction |
| 9 | Headers and trailers move to `*connect.CallInfo` (`RequestHeader()`, `ResponseHeader()`, `ResponseTrailer()`, `PeerAddr`); `CallInfoForHandlerContext` becomes `CallInfoForServerContext`; clients use `connect.NewClientContext`. v2 headers are `*connect.Header`, not `http.Header`. There is no exported `Peer` type; server-side HTTP info is `connecthttp.ServerInfoForContext` | Pkg: `observe` (request ID, trace response, edge context, auth heartbeat), `middleware` (rate limit and audit read the peer), `auth` (`AuthenticateHeaders` takes `http.Header`), `activity.Deps.ViaFrom`, `observe.TrustPredicate`/`TraceTrustFunc` (take `connect.Peer`) | V2 interceptors read `CallInfo`. Connect-free cores (`AuthenticateHeaders`) get a thin adapter from `*connect.Header`. The trust callbacks get connect-free V2 forms taking `(ctx, peerAddr string, header interface{ Get(string) string })` (final shape in the API reference) |
| 10 | `connect.NewError(code, error)` becomes `connect.NewError(code, msg string)` plus `.WithCause(err)`; `connect.Errorf` | Pkg: `svcerr` (`ToConnect`, `Code`, `New`), `authn` (`claims.go` returns a v1 `*connect.Error` as a plain `error`), `crud` (`errors.As(*connect.Error)` passthrough at `crud.go:335`), `testkit`, `tdd.AssertConnectError`, `querycapture`; linters `handler_connect_error`, `no_handler_error_mapping`, `lint_optional_deps_guard_helpers.go`; codemod `connecterrors` | v1 and v2 `*connect.Error` are distinct types and `Code` is a distinct type with the same numeric values. Every error-inspecting helper recognises both (`errors.As` against each, convert with `connectv2.Code(c)`). `svcerr.ToConnectV2`, `CodeV2`, `NewV2` are added. Linters and the codemod accept both import paths; the v1 `connect-errors` rewrite never runs on v2 code (its 2-argument form does not apply) |
| 11 | A plain (non-`*connect.Error`) error from a handler is not serialized: `CodeUnknown`, empty message (v1 sent `err.Error()`) | Pkg: `svcerr` interceptor; linter `unwrapped_domain_error` | Behind `svcerr` nothing changes, because v1 `svcerr.Interceptor` already turns an unrecognised plain error into `CodeInternal` + `svcerr.InternalMessage`; the v2 interceptor keeps that conversion (4(e)). The linter's message text is updated to say what v2 does |
| 12 | Error details: constructor moves to `connectproto.NewErrorDetail`; `AddDetail` becomes cloning `WithDetail`; `ErrorDetail.Value()` becomes `connectproto.UnmarshalErrorDetail` | Pkg: `svcerr` (reason/metadata details), `authn` | `svcerr.ToConnectV2` builds details with `connectv2proto`; on the wire the detail bytes are the same (wire-compat #12) |
| 13 | `NewWireError`/`IsWireError` become `NewError(...).WithRemote()` / `err.IsRemote()`. A remote error returned from a handler is scrubbed to `CodeInternal`, empty message, no details | Pkg: `svcerr` interceptor (any handler that calls another Connect service and returns the error) | The v2 `svcerr` interceptor re-creates a remote error locally with the same code, message and details, matching v1 passthrough (4(e)) |
| 14 | HTTP options move from `connect` to `connecthttp` (`WithReadMaxBytes`, `WithSendMaxBytes`, `WithCompressMinBytes`, `WithRequireConnectProtocolHeader`, `WithSendGzip`, `WithHTTPGet`, `WithHTTPGetMaxURLSize`, `WithProtoJSON`, `WithGRPC`, `WithGRPCWeb`, `WithSendCompression`); `ErrorWriter`, `NewErrorWriter`, `IsNotModifiedError` move too | Pkg: `serverkit` (`payloadCap`, `chain.go:330`), `testkit`, `observe/client.go` | Mechanical rename in the V2 builders. `payloadCap` keeps mapping to `connecthttp.WithReadMaxBytes` with 0 meaning unlimited |
| 15 | Default read limit 4 MiB on both server (`Mount`) and client (`NewTransport`); v1 was unbounded. Over the limit: `resource_exhausted` | Pkg: `serverkit` server path, every forge-built v2 client (`testkit`, `servicecatalog`, `observe` clients) | Pinned for parity (4(e)): `WithReadMaxBytes(0)` when the v1 builder would have been unbounded; forge-built v2 clients default to `WithReadMaxBytes(0)` |
| 16 | Options whose signature changed: `WithCodec` becomes `WithCodecs(...)` and **replaces** the default codecs; `WithCompression`/`WithAcceptCompression` become `WithCompressors(...)` and **replace** the defaults; `WithConditionalHandlerOptions` becomes `WithConditionalOptions(func(Spec) []Option)`; `NewNotModifiedError()` takes no header | Pkg: none uses them in forge builders today (`wire-compat.md` #5: no consumer uses `WithCodec`) | Not exposed by V2 builders in this release. Documented in the package doc: never call `WithCodecs`/`WithCompressors` without listing every default |
| 17 | `connect.Interceptor` (three methods) becomes `connect.ServerInterceptor` and `connect.ClientInterceptor` function types; `UnaryInterceptorFunc`, `WithInterceptors`, `HandlerOption`, `ClientOption` removed | Pkg: ~45 exported constructors in `observe`, `serverkit`, `svcerr`, `middleware`, `auth`, `authn`, `activity`, `audit`, `validate`, `testkit`; `serverkit.Authenticator` (embeds `connect.Interceptor`); `ChainDeps` fields; Codegen: `middleware.go.tmpl` (user-owned `NewAuthInterceptor`), `generator/contract/generator.go:1200-1208` (mock zero-values) | V2 constructors return `connectv2.ServerInterceptor` (or `ClientInterceptor` where only the client side exists, e.g. `ClientRequestIDInterceptor`). Func types are not embeddable or comparable, hence `AuthenticatorV2` and the slot design (4(d)). The contract mock generator adds the two func types to its zero-value table. A user-owned `NewAuthInterceptor` is hand-migrated |
| 18 | Streams: per-RPC named types (`<Svc><Method>ServerStream` / `ClientStream`) replace `*connect.BidiStream[I,O]` etc.; `Receive()` returns `(msg, err)` with `io.EOF`; client stream constructors return an error; `CloseRequest` becomes `CloseSend`, `CloseResponse` becomes `Close`; stream headers move to `CallInfo`; handler-side `Receive() bool`/`Msg()`/`Err()` loops change | Codegen: the four stream signatures in `handlers.go.tmpl`, `handlers_methods.go.tmpl`, `generator.go`, `scaffold/rpc.go`; Pkg: `serverkit/stream_drain.go`, any interceptor that wraps streams | Emitters write `{{pkg}}connect.{{Svc}}{{Method}}ServerStream` (the generator's naming). Interceptors wrap `ServerStream` (stream drain, idempotency capture). Existing user code is migrated by `connect-go-v2-migrate` |
| 19 | Helper packages returning v1 interceptor types need a sibling v2 helper by hand | Pkg (all of the above); consumers' own helpers | The "add `XV2` beside `X`" convention is exactly this guidance |
| 20 | `grpchealth`/`grpcreflect` register on the Server (`grpchealth.Register(server, checker)`, `grpcreflect.Register(server)`) | Pkg: `serverkit/health.go` (unexported handler over `grpc_health_v1` pb, mounted as a v1 handler) | The V2 path registers the same health rpc on a `*connect.Server` through the generated-handler contract. No behaviour change, same `/grpc.health.v1.Health/Check` procedure. `grpcreflect` is not used by forge today |
| 21 | vanguard v0.5 is v2-only: `vanguard.Mount(mux, server)` replaces `Transcoder`/`Service` | Pkg: `serverkit/rest.go` (bridge from v1 handlers via a v2 client/server) | A v1 project keeps the bridge. A v2 project mounts REST from its own Server directly and the bridge is skipped (follow-up inside this PR only if small; otherwise v2 projects keep using the bridge's output, which still works) |
| 22 | New: `connectinprocess` transport | Codegen: `component_test_helpers.go.tmpl`; Pkg: `testkit.NewConnectClient` | v2 `testkit` offers an in-process client constructor; HTTP-behaviour tests keep `httptest` |
| 23 | Unknown method on a known service: plain HTTP 404 becomes a protocol-correct `unimplemented` RPC error (non-RPC requests still 404) | None in forge code; consumers' e2e tests asserting 404 | Accepted wire delta (4(e)) |
| 24 | `validate` and `otelconnect` expose server and client forms and return errors from the constructors | Pkg: `validate`, `observe` (tracing, `TrustRemoteFrom`/`TrustRemoteBy`) | `observe.TracingInterceptorV2` and the trust options use `otelconnect.NewServerInterceptor`/`NewClientInterceptor` (v0.12 supports both majors) |
| 25 | `connect-es` and the web packages | `@reliantlabs/forge-web-runtime`, vite/next templates | **No change.** connect-es versions are independent of the connect-go major, and the protocol is unchanged. The Connect-protocol interceptor stack in the web runtime is untouched |

Items 6 and 7 together are the structural change. Items 5, 10 and 17 are where most `forge/pkg` code changes. Items 11, 13 and 15 are the three behaviours that can differ for a deployed client, and section 4(e) pins each.

## 4. Design

### (a) Connect major selection

A new small package `internal/connectmajor` answers one question per project: v1 or v2.

**Detection rule.** Parse the project root `go.mod` with `golang.org/x/mod/modfile` (forge already does this in several places; no shared helper exists today). The project is **v2 if and only if `connectrpc.com/connect/v2` is a direct require** (no `// indirect` comment). Otherwise it is v1. A direct require of both v1 and v2 is v2: a repository in the middle of the migration chose v2 by running `go get connectrpc.com/connect/v2` (step 1 of section 5).

Presence alone is not a signal. otelconnect v0.12 and vanguard v0.5 pull the other major in as an indirect dependency, so most v1 projects that pin a recent forge already have `connect/v2` in `go.sum`. Keying on the direct require keeps every existing project on v1 with no action.

If the root `go.mod` has no connect require at all (a project with only a `gen/` module), fall back to `gen/go.mod` with the same rule, then to v1. An optional `forge.yaml` override (`codegen.connect: v1|v2`) beats the detection. It exists for one case: a v1 project that directly requires `connect/v2` for an unrelated reason (for example to use `connectinprocess` in one test). The override lands only if that case turns up in dogfooding. `forge generate` prints the major it chose and why on every run, so a surprise is visible.

`connectmajor.Detect(dir)` returns a `Major` exposing `RuntimeImport()`, `PluginModule()` and `VersionModule()`. It is computed once per `forge generate` in `pipelineContext` (`internal/cli/generate_pipeline.go`) and passed to codegen (`ServiceTemplateData`, project and bootstrap template data, `routesView`) as a `ConnectV2 bool` plus the runtime import path; `generator.ProjectGenerator` gets it for birth files. Go emitters (`generator.go`, `scaffold/rpc.go`, `crud_gen.go`, `service_stub_gen.go`) branch on it.

**Plugin install.** `requiredProtoTools` in `internal/cli/tools.go` becomes a function of the major:

| Major | `Module` | `VersionModule` |
|---|---|---|
| v1 | `connectrpc.com/connect/cmd/protoc-gen-connect-go` | `connectrpc.com/connect` |
| v2 | `connectrpc.com/connect/v2/cmd/protoc-gen-connect-go` | `connectrpc.com/connect/v2` |

The binary name is `protoc-gen-connect-go` for both, which is why `buf.gen.yaml` needs no edit and why the cache must not key on the name. The installed-tool cache key (`generate_cache.go`, `generate_toolpins.go`) includes the module path and the resolved version, so a v1 binary can never satisfy a v2 lookup. The bootstrap script in `generator/dx_files.go` installs the module for the project's major (and stops using `@latest`: it resolves the go.mod version like `generate` does). The `tools.go.tmpl` blank import and the preflight's `goToolPackageHint` follow the major.

**One major per project.** Because the plugins emit the same package and file names, a project cannot mix generated majors. Mid-migration, hand-written code may import both runtimes (v2 allows it); the generated packages are whichever major `forge generate` last ran for. `forge generate` refuses nothing here; the build tells you if `service.go` and `_gen.go` disagree.

### (b) `forge/pkg` layout

**Same package, new symbols, `V2` suffix.** The v2 variant of exported symbol `X` is `XV2`:

```go
svcerr.Interceptor()   connect.Interceptor            // unchanged
svcerr.InterceptorV2() connectv2.ServerInterceptor    // new
observe.Chain(...)     []connect.Interceptor          // unchanged
observe.ChainV2(...)   []connectv2.ServerInterceptor  // new
serverkit.ChainDeps    / serverkit.ChainDepsV2
```

Inside `forge/pkg`, v2 imports use fixed aliases: `connectv2 "connectrpc.com/connect/v2"`, `"connectrpc.com/connect/v2/connecthttp"`, `connectv2proto "connectrpc.com/connect/v2/connectproto"`. v1 stays `"connectrpc.com/connect"`. `XV2` means the server side. A client-side variant is spelled `XClientV2` where the v1 type was used on both sides and the distinction matters.

**Connect-neutral symbols where v2 has no connect type.** v2 simple handlers are `func(ctx, *Req) (*Resp, error)`. For `crud.Handle*`, `tdd.Case` and `tdd.HandlerFunc` the v2 shape contains no connect type except `Code`, so a `V2` twin would be a duplicate of a neutral thing. In those cases the package gets one descriptively named connect-neutral core (provisionally `crud.CreateSimple` and its siblings; `tdd.SimpleCase`, `tdd.SimpleHandlerFunc`), and the v1 `Handle*` functions become thin wrappers over it (unwrap `req.Msg`, wrap `connect.NewResponse`). Sharing internals is allowed only if the v1 tests stay green unchanged. The package doc of each such package states the naming choice. Generic types cannot be overloaded, so a new name is needed for them whichever way.

**Why this layout.** It is the precedent otelconnect v0.12 set: `NewInterceptor` (v1) beside `NewServerInterceptor` (v2) in one package. It gives shared unexported internals for free (classification tables, header handling, the connect-free cores), no new import paths for consumers, predictable names for codegen and for the migration ("add `V2`"), and a mechanical rename (drop the suffix) when a future forge major retires v1. The cost is noisier godoc and a package that imports both majors; both are already in forge's go.mod.

Rejected alternatives:

- **Sibling subpackages (`pkg/svcerr/svcerrv2`, ...).** Clean godoc, but ~14 new packages in `project libraries`, shared internals would have to move to `internal/` cores, and generic types still need their own package-qualified copies. Rejected as more surface for no semantic gain. The one place to revisit is `tdd`, if the `Simple*` naming turns awkward in practice.
- **A parallel `pkg/v2/...` tree.** It reads like a forge major, which it is not (this is a minor release), and it either duplicates the connect-free API (two `middleware.GetUser`s, two claims contexts, so a v2 interceptor's claims are invisible to a v1 helper) or forces mixed imports. Rejected.

Dual-major error inspection is **not** a new API: `svcerr.Wrap`, `WrapOr`, `NotFound`, `Code`/`IsClassified`, `crud`'s passthrough, `authn`'s claims error and `tdd.AssertConnectError` simply recognise both majors' `*connect.Error` (conventions 5 above). Connect-free packages (`orm`, `config`, `testkit.Slow`, `middleware.GetUser`/`ClaimsFromContext`/`ContextWithClaims`/`ProcedureDeadlines`, `observe.NewHTTPClient`, `serverkit.Run`/`Worker`/`Probes`/`OpenDB`) are reused as-is. `querycapture` is internal-only and gets the same dual recognition.

### (c) Interceptors per `*connect.Server`

In v1 a handler carried its options. In v2 interceptors belong to the Server, and `connecthttp.Mount(mux, server, opts...)` registers one mux pattern per procedure plus a `/pkg.Svc/` subtree per service. Several Servers can therefore share a mux as long as their services are disjoint, which is the case: a service is mounted once.

The V2 mount API builds **one Server per distinct chain**:

- `mountkit.RegistrarV2` is `interface{ Register(*connectv2.Server) }`. The generated (or hand-written) service `Register` registers its handler on the Server it is given. HTTP and webhook registrars (`HTTPRegistrar`, `WebhookRegistrar`) are pure net/http and unchanged.
- `mountkit.RegisterServiceV2(mux, svc, ints []connectv2.ServerInterceptor, httpOpts ...connecthttp.Option)` builds a Server from the chain, registers the service, and mounts it. Options `WithServerInterceptors(...)` and `WithHTTPOptions(...connecthttp.Option)` replace `WithHandlerOptions`.
- `serverkit.Config` gains `ServerInterceptors []connectv2.ServerInterceptor` and `HTTPOptions []connecthttp.Option` beside `HandlerOpts`. `(*serverkit.Server).Mount(path, svc, ...)` serves both majors by type-asserting `RegistrarV2`. `RoutesV2` is `interface{ Register(mux, *connectv2.Server) []string }`, and a per-listener chain becomes a per-listener Server.
- The generated mounts (`mounts_services_gen`, `routes_gen`, `providers-graph`, `cmd-svc-mount-gen`) group services by their resolved chain (the same chain values the v1 code threads as `opts ...connect.HandlerOption`) and emit one `connect.NewServer(chain...)` per group, each mounted with `connecthttp.Mount(mux, server, httpOpts...)`.

Interceptor order is unchanged: in both majors the first interceptor is outermost (v2: `connect.go`, "interceptors fire in argument order. The first wraps the outermost call").

### (d) Interceptor identity: slots, not func comparison

v1 code compares interceptor interface values in two places: `svcerr.IsInterceptor`/`WithInterceptor` (is the svcerr interceptor already in this slice?) and the `serverkit.Omit` sentinel (a type implementing `Interceptor`, used to omit a slot). v2 interceptors are funcs and are not comparable.

The design is that identity lives in the chain builder, not in the interceptor value:

- `ChainDepsV2` keeps the named slots `ChainDeps` already has (`Outer`, `Trace`, `Deadline`, `Audit`, `RateLimit`, `Extras`, `Activity`, `StreamDrain`, `Validate`). Each slot is a small value `Slot{Interceptor connectv2.ServerInterceptor; Omit bool}` (or equivalent) rather than a bare interceptor, so "omit" is an explicit field and not a sentinel instance. `NoAudit` and the other `Omit` users map to the field.
- `ChainV2.ServerInterceptors()` places the `svcerr` interceptor itself, at a fixed position, so there is nothing to detect in a user-supplied slice. `svcerr.IsInterceptor` has no V2 twin; the check it served ("did the caller forget svcerr?") becomes a property of the builder. A bare `[]ServerInterceptor` built by hand carries no guarantee, which is why `ChainV2` is the supported way to build one.
- `AuthenticatorV2` (`ServerInterceptor() connectv2.ServerInterceptor; Audience(); Caller()`) replaces the embedded `connect.Interceptor`, which a func type cannot satisfy. `AuthForV2` and `AuthWithCallerV2` mirror `AuthFor`/`AuthWithCaller`.
- Tests assert chain composition through the builder's own output (a slot-ordered description of the chain), not through func identity.

### (e) Errors and wire parity

The wire protocol is unchanged (`wire-compat.md`: header names, content types, envelope flags, error JSON, gRPC trailers, details, codecs, compression, GET handling, default protocols). The V2 builders pin the three behaviours that default differently.

**`svcerr.InterceptorV2` semantics.** It runs in the **same position as v1: innermost**, directly around the handler. (This is deliberate: an outermost placement would hand logging, metrics and tracing interceptors an unconverted plain error, where in v1 they see the already-classified code. In v2 the scrub happens at the transport after the whole chain returns, so any position inside the chain produces the same wire error; innermost also preserves what the rest of the chain observes.) It does what v1 does, using one shared classification core:

1. A `*connect.Error` of either major that is local passes through, as a v2 error.
2. A svcerr-classified error is converted by `ToConnectV2` (code, user-safe message, reason header and metadata, details via `connectv2proto`).
3. A context error keeps its canceled/deadline-exceeded mapping.
4. Any other plain error becomes `CodeInternal` with `svcerr.InternalMessage` and the cause attached via `WithCause` (the cause stays visible to `errors.Is` server-side and is never sent). This is exactly what v1 `svcerr.Interceptor` does today, so a deployed client sees the same message in v1 and v2.
5. **Remote-error re-localisation.** A `*connect.Error` marked `IsRemote()` that a handler returns (it came from an upstream client call) would be scrubbed by v2 to `CodeInternal`, empty message, no details. v1 passed code, message and details through. The V2 interceptor re-creates it with `connectv2.NewError(code, message)`, copies every detail with `WithDetail`, and attaches the original with `WithCause`. There is no un-remote method, so re-creation is the only way. A v1 remote error (a v1 client in the same process) already passes through under rule 1.

So behind `svcerr` there is **no** wire delta for plain errors, and forge does not ship a separate "v1-compatible error interceptor" that leaks `err.Error()`: both consumers run `svcerr` in every production chain (`serverkit.NewChain`, `observe.Chain`), and v1-behind-svcerr already redacts. What needs checking in a consumer is any Server **not** built by `ChainV2`: test servers and `wirecheck`-style harnesses (section 6).

**Read limits.** v1 was unbounded by default; v2 defaults to 4 MiB on `Mount` and `NewTransport`, and exceeding it is `resource_exhausted`. forge already passes an explicit read limit on the server path (`payloadCap` in `serverkit/chain.go`); the V2 builders map it to `connecthttp.WithReadMaxBytes(n)`, with 0 and negative values meaning unlimited (`WithReadMaxBytes(0)`), exactly as v1 treats them. When nothing is set where v1 was unbounded, the V2 builder passes `WithReadMaxBytes(0)`. forge-built v2 **clients** (`testkit`, `servicecatalog.NewClientV2`, `observe` client config) default to `connecthttp.WithReadMaxBytes(0)` unless the caller sets one, because v1 clients were unbounded. The send limit is unbounded by default in both majors and is forwarded as today. `WithCodecs` and `WithCompressors` are not exposed.

**The one accepted delta.** A request for an unknown method on a known service returned a plain HTTP 404 under v1's per-handler mux; under `Mount` it gets a protocol-correct `unimplemented`. The case needs a client calling an rpc the server lacks (version skew). connect-es already maps a 404 to `unimplemented`, so web clients see the same thing. We accept it: matching v1 would need a custom mux wrapper. A consumer test asserting HTTP 404 for this case is updated.

### (f) Deprecation

In this release v1 `forge/pkg` symbols get a **prose** note only ("connect-go v2 projects use `XV2`"); there is no `Deprecated:` paragraph. A machine-readable marker makes `staticcheck` SA1019 fail in every consumer that lints non-test code, and a forge minor release must not turn a consumer's gate red on pin (the same reason forge's lint rules warn before they gate). The formal `Deprecated:` marker lands in a later release, after reliant and control-plane have migrated and no first-party v1 caller is left. Removal of v1 is a forge major and is the owner's decision.

### (g) What a v2 project gets from generation

- **`service.go`.** `func (s *Service) Register(server *connect.Server) { <pkg>connect.Register<Svc>Handler(server, s) }`. It is user-owned and written once, so it exists in this shape only for newly scaffolded services; migrating projects use the codemod in section 5. It satisfies `mountkit.RegistrarV2`.
- **Handlers.** Bare-message signatures for all four rpc kinds: `Get(ctx, *pb.Req) (*pb.Resp, error)`; server stream `(ctx, *pb.Req, <Svc><Method>ServerStream) error`; client stream `(ctx, <Svc><Method>ServerStream) (*pb.Resp, error)`; bidi `(ctx, <Svc><Method>ServerStream) error`. CRUD shims call the neutral `crud` cores and read request headers through `CallInfoForServerContext` where they need them.
- **Mounts.** `mounts_services_gen`, `routes_gen`, `providers-graph` and `cmd-svc-mount-gen` group services by chain and emit one Server per group (4(c)). `MountFunc` and `Routes` signatures take the V2 forms.
- **Middleware.** `middleware.go` scaffolds `NewAuthInterceptor` returning `(connect.ServerInterceptor, error)`, written in the function-type style.
- **Tests.** The scaffolded unit test uses `tdd` simple rows (`Req: &pb.X{}`, `Check func(*pb.Resp)`, `WantErr: connect.CodeInvalidArgument`); component tests build the service on a Server and call it through `connect.NewClient(connectinprocess.New(server))`; the e2e harness builds `connect.NewClient(connecthttp.NewTransport(httpClient, baseURL, connecthttp.WithReadMaxBytes(0)))`.
- **Dependencies.** `go.mod.tmpl` requires `connectrpc.com/connect/v2`, `otelconnect` v0.12+, `vanguard` v0.5+; `gen-go.mod.tmpl` and `tools.go.tmpl` follow.
- **Analyzers.** All accept both import paths (resolve the alias from the import spec, treat `connectrpc.com/connect/v2` as the runtime, recognise `Register<X>Handler`): `lint_optional_deps_guard_helpers`, `linter/contract/exclude_directive`, `contractcheck/outbound_io_no_rpc`, `doctor/payload`, `forgeconv/handler_connect_error` and `no_handler_error_mapping` (v2 `NewError` takes a string, so the argument-shape classifier is major-aware), `unwrapped_domain_error` (message updated), `codegen/crud_owner_source` and `unwired_stub_excise`. The `connect-errors` codemod does nothing on v2 code. Each fixture gets a v2 twin.
- **The forge PR's own proof.** Existing v1 e2e tests run unmodified as the control. A new `TestE2EConnectV2ScaffoldServesRPC` (modelled on `TestE2EScaffoldServerStartup` and `TestE2EPbThroughCrudPlusCustomRpc`) scaffolds `project new --connect v2` with an entity, a custom unary rpc and a server stream, generates, builds, boots the server and calls it with a v2 client and with a raw Connect-JSON request, and runs `forge lint`. Wire parity tests in `pkg/svcerr` serve the same error through a v1 and a v2 chain and compare status, headers and body bytes, including a remote error and a plain error. Each V2 symbol gets a test mirroring its v1 test, over `connecthttp` or `connectinprocess`.

### (h) New projects

In this release `forge project new` still scaffolds v1. A flag (provisionally `--connect v2`) scaffolds a v2 project: v2 requires in `go.mod`, v2 plugin in `tools.go`, v2 service templates. Flipping the default is a follow-up decided after dogfooding (section 7). Because detection reads `go.mod`, the flag only has to write the right requires; every later command follows.

## 5. Migrating a forge project from v1 to v2

The sequence follows the connect-go migration guide, with forge steps interleaved. Steps marked (M) are mechanical; (H) need a person or agent.

1. **Pin a forge release that includes this support.** (M)
2. **`go get connectrpc.com/connect/v2`** so it is a *direct* require. This is what flips `connectmajor` to v2. (M)
3. **`connect-go-v2-migrate -w`, first pass.** Rewrites `buf.gen.yaml` where it needs it (forge's local-plugin entry does not), drops `simple=true`, updates code that does not depend on generated types. The project does not compile until step 6. (M)
4. **`forge generate`.** Detects v2, installs the v2 plugin from its own module, and emits v2 `_gen` files (mounts, routes, bootstrap, component test helpers) and v2 generated Go. (M)
5. **forge's own shapes.** The migrate tool rewrites the inline `mux.Handle(New<X>Handler(...))` shape and warns on the rest. forge's scaffolded `service.go` `Register` assigns `path, h := ...` first, which the tool warns about and leaves, so forge ships a codemod (provisionally `forge fix connect-v2`) for the forge-owned shapes: `service.go` `Register` becomes `Register(server *connect.Server)`; the scaffolded tdd rows (`connect.NewRequest(&pb.X{})`, `Check func(*connect.Response[O])`) become simple rows. Whether this lands in the forge PR or in an immediate follow-up is an implementation-time call; the migration is unusable without it, so it must exist before the consumers start. (M)
6. **`connect-go-v2-migrate -w`, second pass.** Migrates handler signatures, client calls, `NewError` (to `NewError(code, err.Error()).WithCause(err)`), stream loops, header and trailer access, `validate`. (M)
7. **Switch hand-written uses of `forge/pkg` v1 symbols to their V2 forms.** `svcerr.Interceptor` to `InterceptorV2`, `observe.Chain` to `ChainV2`, `serverkit.ChainDeps` to `ChainDepsV2`, `servicecatalog.NewClient` to `NewClientV2`, `tdd.Case` to the simple forms, and so on. This is a rename by convention and can be scripted with `gofmt -r` or the codemod. (M for the rename; H where a callback signature changed: `authn.Policy.MapErrorV2`, `activity.Deps.ViaFromV2`, trust predicates.)
8. **Hand work the tool warns about.** Custom interceptors rewritten to `ServerInterceptor`/`ClientInterceptor`; `otelconnect` server vs client choice; `connect.WithCodec`/`WithCompression` (none today); the stream header cases that need two `CallInfo`s; `NewWireError`/`IsWireError`. (H)
9. **`go mod tidy`, build, `forge lint`, tests.** Fix remaining warnings. (M/H)

Services may be migrated one at a time inside a module (both runtimes can be imported), but the generated packages are one major, so in a forge project the unit of migration is the whole project.

## 6. Consumer migration plan: reliant and control-plane

### Scale

| | Non-gen files importing connect v1 | `connect.Request[` uses | Interceptor uses | Stream uses |
|---|---|---|---|---|
| reliant | ~818 | 1241 | ~68 | ~50 |
| control-plane | ~978 | 1701 | ~103 | ~3 |

The migrate tool does the bulk (handler and client signatures, `NewError`, most header access). The hand work is the interceptor uses (most are `connect.WithInterceptors(...)` at call sites, which go through `ChainV2`; a minority are custom interceptors), the 50 reliant streams, and review.

### Order

1. **Release forge with this support.** Pin it in reliant (bump per reliant `dev-docs/forge-upgrade.md`; record it in the upgrade log in the same PR). That bump alone changes nothing: both repos stay v1.
2. **reliant migrates.**
3. **control-plane pins forge and reliant in lockstep** (`task pin:forge REF=<forge sha> RELIANT=<reliant commit>`; `task pin:forge-coherence` fails skew), then migrates. control-plane `go.mod` pins `github.com/reliant-labs/reliant` and imports its packages, so the reliant pin that carries the migrated reliant is part of control-plane's bump. `reliant.v1` proxying in control-plane (`internal/proxy`, `llmproxy`, `workspaceproxy`) is a raw `httputil.ReverseProxy` and is not affected by the connect major. Only one control-plane file imports reliant's generated connect package (`e2e/user_stories/story_09_full_daemon_flow_test.go`); any other import of a reliant package whose exported API carries connect types has to move with the pin. Check this before the bump.

Both repos merge through their own queues; each phase below is a PR (or a small series) that a steward merges. Nothing here uses `merge:priority` or a release label by default.

### Phases per repo

1. **Tooling and plugin.** `go get connectrpc.com/connect/v2`; `forge generate` installs the v2 plugin; confirm `buf.gen.yaml` (reliant and control-plane use `local: protoc-gen-connect-go`) and the preflight pass. Update any hand-written scripts or CI steps that install the plugin.
2. **Generated code.** Regenerate; commit `gen/` as v2 in one PR. This breaks the build until phase 3, so phases 1 to 4 land as one PR, or as a branch merged together.
3. **Handlers and clients via the migrate tool.** Two passes plus forge's codemod (section 5). Review by package; handler logic is untouched by design.
4. **Mounts and service `Register`.** Generated mounts group by chain. reliant has several distinct chains (user, daemon, gateway; `internal/grpc/user_chain.go`, `daemon_listener.go`); each is one Server.
5. **Custom interceptors** (~68 and ~103 uses; the custom ones) rewritten by hand to `ServerInterceptor`/`ClientInterceptor`; forge-provided ones switch to their V2 forms.
6. **Streams** (reliant's 50): named stream types, `Receive()` loops, client stream constructors that now return an error, and stream-aware interceptors.
7. **Clients with `WithReadMaxBytes(0)`.** Every client transport (`connecthttp.NewTransport(hc, url, connecthttp.WithReadMaxBytes(0))`) unless a limit is chosen on purpose. This covers the daemon's gRPC clients (`cmd/reliant/cmd/daemon_instance.go:226`, `internal/toolexec/daemonruntime/runtime.go:637`, both `WithGRPC()`, which becomes `connecthttp.WithGRPC()`), the Go SDK, the CLI, and server-to-server clients.
8. **Tests.** `wirecheck`, handler unit tests (tdd simple rows), component tests (in-process transport), integration tests asserting 404 for unknown methods.

### Wire verification

Each repo has `internal/wirecheck` (`RoundTrip`: serve an error from a real Connect handler behind the svcerr interceptor and return what a client received). The plan extends it:

- **Byte-identical error bodies.** For representative rpcs (a classified svcerr error with reason and metadata; a plain error; a remote error forwarded from an upstream client call; a context cancel; a validation failure with details; a stream error), serve the same handler through the pre-migration v1 chain and the migrated v2 chain and compare status, headers and body bytes over Connect-JSON, Connect-proto and gRPC. These golden bytes are recorded on the last v1 commit, so a regression after migration is a diff.
- **Old clients against the new server.** An old daemon build (v1 client, `WithGRPC`), the Electron app and the web client (connect-es, JSON and one binary spec) run against a migrated server in a throwaway environment. Cover: a >4 MiB payload (attachment `bytes content`, `tools_daemon` `body`/`data`, workflow yaml, deploy `manifests_layer`) both directions, an error with a user-facing message, and a server-stream.
- **New clients against an old server.** A v2-built daemon against a v1 server, for the rollout window.

### Rollout guard

Both repos ship the **server first**. Old clients keep working because the wire is unchanged, so no client has to move in lockstep and no flag is needed. Clients (daemon, SDK, Electron) follow in ordinary releases. Rolling back is a normal deployment rollback: the wire is the same both ways. The wire verification above is the gate for the server release.

### Risks

- **Remote-error passthrough.** Any handler that calls another Connect service and returns its error depends on re-localisation in `svcerr.InterceptorV2` (4(e)). A Server that is not built through `ChainV2` loses it. Test: the remote-error case in wirecheck, and an audit of every `connect.NewServer`/`Mount` in each repo.
- **Servers outside the standard chain.** A hand-mounted Server without `svcerr` (test servers, small utility listeners) returns plain errors as an empty `CodeUnknown`. The audit finds them. reliant has ~82 handler `fmt.Errorf`/`errors.New` returns and control-plane ~20, some user-facing (for example `internal/handlers/connector`); they are safe behind `svcerr` only because of rule 4 in 4(e), which already redacts them in v1.
- **>4 MiB payloads.** Attachments, `tools_daemon` payloads, deploy manifests, storage browser and dashboard content, RUM data, long transcript lists. Every Server and every client transport must carry the explicit limit (4(e), phase 7). control-plane already sets a 4 MiB server limit explicitly; reliant sets unlimited. Missing one client is the likeliest regression.
- **SDK and public Go surface.** reliant treats the SDK and its Go API as public under its own semver (major 1, backwards compatible). If `sdk/go` or another published package exposes connect types or generated stubs, a v2 migration changes its API. Check which published surfaces expose connect types before phase 1; if there are any, keep the old API and add the new one beside it (the same convention forge uses), and let the owner decide the timing. This is the one item here that might need a decision beyond the plan.
- **vanguard REST.** reliant and control-plane that serve REST through vanguard move from the v1 bridge to the v2-native `vanguard.Mount`; verify the REST routes and their error mapping against the existing tests.
- **Stream interceptors.** Stream drain, idempotency capture (wraps `ServerStream.Send`) and logging status need stream-level tests, not only unary ones.
- **Reviewability.** ~1,800 changed files is a review problem, not a technical one. Phases are cut by package so reviewers can read mechanical diffs by sampling; the migrate tool's report and the golden wire bytes are the evidence.

### Rough sizing

These are estimates for a sequenced, agent-assisted migration with human review, not commitments: reliant about 1 to 2 weeks (streams, several chains, daemon clients, SDK check); control-plane about 1 week (fewer streams, similar volume, much of it mechanical). forge's own PR (packages, templates, plugin, analyzers, e2e) is larger than either and comes first. The calendar time is dominated by queued review and the wire verification, not by editing.

## 7. Open questions for the owner

1. **When do we flip the `project new` default to v2?** This release keeps v1 as the default and offers `--connect v2`. Options: flip after both consumers have been on v2 in production for a release cycle; flip in the next forge minor regardless; flip only in a forge major. Recommendation: after reliant and control-plane have been on v2 for one release cycle, in a minor, since it changes only new projects and detection protects existing ones.
2. **When does the formal `Deprecated:` marker land on v1 `forge/pkg` symbols?** It turns every v1 consumer's staticcheck red on pin. Options: the release after both consumers have migrated (recommended; no first-party caller remains); on a fixed date; with the default flip. Removal of v1 is a forge major regardless.
3. **Should forge ever adopt v2's plain-error redaction as a deliberate behaviour change?** Behind `svcerr` it is already the behaviour, so this only concerns code that returns plain errors outside a forge chain, where this design does nothing to preserve v1's leak of `err.Error()`. The question is whether a future forge major should make redaction a hard guarantee (for example refusing to mount a Server not built by `ChainV2`), or keep it advisory. Recommendation: keep it advisory in v1.x and make the standard chain mandatory in the next major.

## API reference (filled in from the implementation)

TODO: filled in from /tmp/connect-v2/api-v2.md when the implementation lands (final symbol names, signatures and package docs for every `XV2` and connect-neutral symbol; the `Simple*`/`Create*`-style names, `Register`/`RegistrarV2`, `Slot`, and the trust-callback shapes marked provisional above are confirmed here).
