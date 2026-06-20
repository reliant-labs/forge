// Package appkit is the runtime engine behind the generated
// pkg/app/bootstrap.go file in forge projects.
//
// # Pattern
//
// NOTE (FORGE_SHAPE_REDESIGN §2): appkit is the LEGACY DI engine. The
// live composition path is the generated cmd/server.go over the
// internal/app layer (OpenInfra → Build → PostBuild → Inventory). appkit
// still owns the worker/operator supervised-component contracts
// (WorkerInstance / ContextWorkerInstance / WrapWorker) that the new
// internal/app/lifecycle_gen.go re-uses; the string-keyed selection
// (Options.Only / LazyConstruct) has been retired.
//
// Forge's bootstrap generator used to emit a ~650-line program: package
// construction, per-service wiring, Connect/HTTP mounting, REST
// transcoding, and diagnostics boot were all open-coded in the generated
// file. Anyone who needed custom
// worker construction or an extra HTTP mount had to fork bootstrap.go —
// and a forked generated file never regenerates again.
//
// appkit inverts that: the generated bootstrap.go is now a TABLE — one
// dumb row per service, worker, operator, and internal package — and
// this package owns all the behavior (ordering, filtering, hooks, REST,
// diagnostics). Adding a row is `forge generate`'s job; changing
// behavior is the job of [Hooks], populated from the user-owned
// pkg/app/setup.go. There is nothing left in the generated file worth
// forking.
//
// A generated row carries the only things forge can know that this
// library cannot — concrete types and wiring call sites — as closures,
// which keeps the table compile-time type-safe with zero reflection:
//
//	Services: []appkit.ServiceDef{
//	    {Name: "api", ConnectName: apiv1connect.APIServiceName,
//	        Construct: func() (appkit.Mounter, error) {
//	            deps := wireAPIDeps(app, cfg, logger, devMode)
//	            svc, err := api.New(deps)
//	            if err != nil {
//	                return nil, fmt.Errorf("initializing api service: %w", err)
//	            }
//	            app.Services.API = svc
//	            return func(mux *http.ServeMux) {
//	                svc.Register(mux, slices.Concat(opts, []connect.HandlerOption{connect.WithInterceptors(
//	                    middleware.AuthzInterceptor(deps.Authorizer),
//	                )})...)
//	                svc.RegisterHTTP(mux, middleware.HTTPStack(logger))
//	            }, nil
//	        }},
//	}
//
// # Orchestration order
//
// [Run] executes the table in a fixed order that mirrors the historical
// generated bootstrap exactly:
//
//  1. def.Setup — the user-owned Setup(app, cfg) that builds
//     infrastructure (DB pool, NATS, audit sink) and assigns it onto
//     *App fields read back by the wireXxxDeps functions.
//  2. Diagnostics boot ([DiagnosticsLog] / [DiagnosticsStrict]) — emits
//     unwired-scaffold warnings recorded by the codegen pipeline.
//  3. Internal packages, in table order (services may depend on them).
//  4. Service construction, in table order. Each Construct returns the
//     [Mounter] for that service so construction and mounting stay
//     separable. Every registered row is constructed and mounted —
//     string-keyed selection is retired.
//  5. Hooks.BeforeMount.
//  6. Service mounts, in table order.
//  7. Hooks.ExtraMounts — plain pattern/handler pairs for hand-rolled
//     HTTP endpoints (LLM proxies, registry adapters, debug routes)
//     that previously forced a bootstrap fork.
//  8. Hooks.AfterMount.
//  9. Workers, in table order — each construction passes through
//     Hooks.ConstructWorker when set, so a project can substitute its
//     own constructor for any worker without forking the table.
//  10. Operators, in table order.
//  11. REST transcoding (when [Def].REST is non-nil): a vanguard
//     transcoder is built over the mux from every service row's
//     ConnectName and handed to REST.Assign, which the generated table
//     points at app.RESTHandler.
//
// # Hooks
//
// [Hooks] exists for the two documented reasons projects forked the old
// generated bootstrap:
//
//   - Custom worker construction: set Hooks.ConstructWorker in
//     setup.go. It receives the worker's name and the table's default
//     constructor; call the default for workers you don't care about,
//     do your own construction (and your own assignment onto
//     app.Workers.<X>) for the ones you do.
//   - Custom HTTP mounts: append to Hooks.ExtraMounts, or use
//     BeforeMount/AfterMount when ordering relative to the generated
//     service mounts matters.
//
// The generated App struct carries a `Hooks appkit.Hooks` field;
// because hooks are read AFTER Setup returns, assigning them in
// setup.go is always observed:
//
//	func Setup(app *App, cfg *config.Config) error {
//	    app.Hooks.ExtraMounts = append(app.Hooks.ExtraMounts,
//	        appkit.MountDef{Pattern: "/llm/", Handler: newLLMProxy(cfg)})
//	    app.Hooks.ConstructWorker = func(name string, construct func() error) error {
//	        if name == "trader" {
//	            app.Workers.Trader = trader.NewWithBook(app.Book)
//	            return nil
//	        }
//	        return construct()
//	    }
//	    return nil
//	}
//
// # Behavioural fingerprint
//
// The pre-table generated bootstrap wrote these observable strings,
// all preserved verbatim (either here or in the generated row
// closures, which own component-specific error wrapping):
//
//   - "setup: <wrapped error>" when Setup fails.
//   - `unknown service/worker/operator name, ignoring` warn log with
//     "name" and "known" attributes.
//   - "init vanguard REST transcoder: <wrapped error>".
//   - "initializing <pkg> service|worker|operator: <wrapped error>"
//     (emitted by the generated Construct closures).
package appkit
