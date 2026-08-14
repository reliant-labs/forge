// Part of @reliantlabs/forge-web-runtime — the web twin of forge/pkg.
//
// The mock transport ENGINE. A generated frontend used to carry this entire
// pipeline as ~300 lines of Tier-1 scaffold; what varies per project is only
// the table of entities to dispatch on, so the engine lives here and the
// project keeps a declarative descriptor list.
//
// Dispatch order (unchanged from the scaffolded original):
//
//   1. A scenario selected via `?scenario=<name>` (or the auto-generated
//      `default` scenario) gets first crack at every unary RPC. Handlers
//      keyed by `${serviceTypeName}/${methodName}` return typed proto
//      messages and short-circuit the rest of the pipeline.
//   2. If the active scenario has `passthrough: true` AND a fallback
//      transport was supplied (hybrid mode — VITE_MOCK_API=hybrid /
//      NEXT_PUBLIC_MOCK_API=hybrid), unmatched RPCs are forwarded to the
//      real backend. This lets one scenario stub a single endpoint while
//      everything else exercises the live server.
//   3. Otherwise RPCs not overridden fall through to the deterministic
//      per-entity fixtures described by the entity descriptors.
//   4. Anything else rejects with Code.Unimplemented so callers see a
//      clear error instead of a silent empty response.
//
// Imported through the "@reliantlabs/forge-web-runtime/mock-transport" subpath,
// never the barrel: a production bundle must be able to shake the fixtures
// and this engine out entirely, which is why the generated `connect.ts`
// reaches its own shim through a dynamic `require()` gated on the mock-mode
// env var.
import { create, type DescMessage, type MessageInitShape } from "@bufbuild/protobuf";
import {
  Code,
  ConnectError,
  type Transport,
  type UnaryResponse,
} from "@connectrpc/connect";

/**
 * The shape the generated scenario files satisfy. Structural on purpose: the
 * project's own `Scenario` type (src/mocks/scenario-types.ts) carries a
 * richer, per-RPC-typed `handlers` map, and this is the subset the engine
 * dispatches through.
 */
export interface MockScenario {
  name: string;
  /** Per-RPC overrides keyed by `${serviceTypeName}/${methodName}`. */
  handlers: Record<string, ((req: never) => unknown) | undefined>;
  /** Forward unmatched RPCs to the real backend (hybrid mode only). */
  passthrough?: boolean;
  /** Non-RPC side effects, run once before the first RPC fires. */
  setup?: () => void;
}

/** The generated `src/mocks/scenarios` barrel, as the engine consumes it. */
export interface MockScenarioRegistry {
  byName(name: string): MockScenario | undefined;
  defaultScenario: MockScenario;
}

/** One CRUD arm of an entity's dispatch table. */
export interface MockListRpc {
  rpc: string;
  responseSchema: DescMessage;
  /**
   * The camelCase name of the list response's repeated field — the ACTUAL
   * proto field (e.g. `keys`), not the camelCased entity plural.
   */
  itemsField: string;
}

export interface MockGetRpc {
  rpc: string;
  responseSchema: DescMessage;
  /** The camelCase field wrapping the entity on the response message. */
  entityField: string;
}

export interface MockWriteRpc extends MockGetRpc {
  /**
   * The camelCase field wrapping the entity on the REQUEST message. AIP-134
   * requests nest the entity ({ item: {...}, updateMask }); the generated
   * forms submit it flattened. Both are accepted.
   */
  requestField: string;
}

export interface MockDeleteRpc {
  rpc: string;
}

/**
 * Everything the engine needs to serve one entity's CRUD quintet from
 * fixtures. Generated into `src/lib/mock-transport.ts` from the project's
 * proto descriptors.
 */
export interface MockEntityDescriptor {
  /** Fully-qualified proto service name, e.g. "demo.v1.ClinicService". */
  service: string;
  /** Lowercase entity name, used verbatim in the NotFound message. */
  label: string;
  /**
   * The camelCase PRIMARY-KEY field. The session store keys records by this
   * field; hardcoding "id" breaks every surrogate-PK entity (a
   * `usage_event_id` PK projects to a message with no `id` field).
   */
  pkField: string;
  /** Deterministic seed rows — the same values the dev database is seeded with. */
  fixtures: readonly unknown[];
  /** The entity message schema. Required by the create/update write paths. */
  entitySchema?: DescMessage;
  list?: MockListRpc;
  get?: MockGetRpc;
  create?: MockWriteRpc;
  update?: MockWriteRpc;
  delete?: MockDeleteRpc;
}

export interface CreateMockTransportOptions {
  /** The resolved scenario — see {@link resolveActiveScenario}. */
  scenario: MockScenario;
  /** Per-entity fixture dispatch. Omit for a scenario-only project. */
  entities?: readonly MockEntityDescriptor[];
  /**
   * Optional real Connect transport. When the active scenario declares
   * `passthrough: true`, any RPC not matched by a scenario handler is
   * forwarded here instead of falling through to the fixtures.
   */
  fallback?: Transport;
}

/**
 * Mutable session stores, keyed by descriptor identity.
 *
 * Create/Update/Delete round-trip within one browser session (a created row
 * appears in the next List; a deleted one disappears) and reset to the
 * deterministic fixtures on reload. The scaffolded original got that from
 * module-scope `const store = new Map(...)`; keying off the descriptor
 * object preserves it — a module-level descriptor array hands back the same
 * store on every call, so building the transport twice (hybrid mode, a test
 * swapping transports) does not silently fork the data.
 */
const stores = new WeakMap<MockEntityDescriptor, Map<string, unknown>>();

function storeFor(entity: MockEntityDescriptor): Map<string, unknown> {
  let store = stores.get(entity);
  if (!store) {
    store = new Map(
      entity.fixtures.map((row) => [
        String((row as Record<string, unknown>)[entity.pkField]),
        row,
      ]),
    );
    stores.set(entity, store);
  }
  return store;
}

/** Scenarios whose `setup()` has already run. */
const initialized = new WeakSet<MockScenario>();

/**
 * Resolve the scenario the app should run under, and run its `setup()` once.
 *
 * Call this at MODULE SCOPE (the generated `src/lib/mock-transport.ts` does).
 * Reading the URL once — rather than on every RPC — means navigating away
 * from `?scenario=` keeps the scenario active until a full reload, matching
 * the agent-driven flow where the URL is the single source of truth.
 *
 * `setup()` is for non-RPC state (localStorage flags, sessionStorage). It is
 * synchronous and makes no network calls.
 */
export function resolveActiveScenario(
  registry: MockScenarioRegistry,
): MockScenario {
  const requested =
    typeof globalThis !== "undefined" && globalThis.location
      ? new URLSearchParams(globalThis.location.search).get("scenario")
      : null;
  // A ternary, not `&&`: with `??` alone the empty-string falsy branch leaks
  // '' into the inferred union, and '' survives `??` (which only replaces
  // null/undefined), so downstream access on .setup / .handlers would fail
  // under strict tsc.
  const active =
    (requested ? registry.byName(requested) : undefined) ??
    registry.defaultScenario;

  if (!initialized.has(active)) {
    initialized.add(active);
    active.setup?.();
  }
  return active;
}

/**
 * Normalize whatever a streaming scenario handler returns into an
 * AsyncIterable<unknown>. Accepts arrays, iterables, async iterables, and
 * promises that resolve to any of the above. Single (non-iterable) values
 * are wrapped as a one-element stream — handy for handlers that conceptually
 * yield once.
 */
export async function* toAsyncIterable(
  value: unknown,
): AsyncIterable<unknown> {
  const resolved = await Promise.resolve(value);
  if (resolved == null) return;
  // AsyncIterable
  if (
    typeof (resolved as { [Symbol.asyncIterator]?: unknown })[
      Symbol.asyncIterator
    ] === "function"
  ) {
    for await (const m of resolved as AsyncIterable<unknown>) yield m;
    return;
  }
  // Iterable (including arrays)
  if (
    typeof (resolved as { [Symbol.iterator]?: unknown })[Symbol.iterator] ===
    "function"
  ) {
    for (const m of resolved as Iterable<unknown>) yield m;
    return;
  }
  // Single value
  yield resolved;
}

function makeUnaryResponse<T>(
  method: { name: string; parent: { typeName: string } },
  message: T,
): UnaryResponse<never, never> {
  return {
    service: method.parent as never,
    method: method as never,
    stream: false,
    header: new Headers(),
    message: message as never,
    trailer: new Headers(),
  };
}

/** A fixture-backed handler for one RPC key. */
type FixtureHandler = (input: unknown) => unknown;

/**
 * Compile the entity descriptors into a flat `${service}/${rpc}` → handler
 * map. Building it once per transport keeps per-RPC dispatch to a single
 * lookup, exactly like the generated `switch` it replaces.
 */
function buildFixtureTable(
  entities: readonly MockEntityDescriptor[],
): Map<string, FixtureHandler> {
  const table = new Map<string, FixtureHandler>();

  for (const entity of entities) {
    const pk = entity.pkField;
    const key = (rpc: string) => `${entity.service}/${rpc}`;

    if (entity.list) {
      const { rpc, responseSchema, itemsField } = entity.list;
      table.set(key(rpc), () =>
        create(responseSchema, {
          [itemsField]: Array.from(storeFor(entity).values()),
        } as MessageInitShape<DescMessage>),
      );
    }

    if (entity.get) {
      const { rpc, responseSchema, entityField } = entity.get;
      table.set(key(rpc), (input) => {
        // Read the key off the SAME field the store is keyed by, not a
        // hardcoded `id` — a surrogate-PK entity would otherwise look up
        // String(undefined) and miss every record.
        const req = input as Record<string, unknown> | undefined;
        const found = storeFor(entity).get(String(req?.[pk]));
        if (!found) {
          // A miss is NotFound — exactly what the real backend returns.
          // (Falling back to the first fixture made every detail page
          // "work" against the wrong record and hid bad-id bugs.)
          throw new ConnectError(
            `${entity.label} ${String(req?.[pk])} not found`,
            Code.NotFound,
          );
        }
        return create(responseSchema, {
          [entityField]: found,
        } as MessageInitShape<DescMessage>);
      });
    }

    if (entity.create && entity.entitySchema) {
      const { rpc, responseSchema, entityField, requestField } = entity.create;
      const entitySchema = entity.entitySchema;
      table.set(key(rpc), (input) => {
        // Create requests carry the entity fields flattened (that's how the
        // generated form submits); a nested { <entity>: {...} } payload is
        // accepted too.
        const req = (input ?? {}) as Record<string, unknown>;
        const init = {
          ...((req[requestField] ?? req) as Record<string, unknown>),
        };
        delete init.$typeName;
        // Mint the primary key on the PK field the store is keyed by — for a
        // surrogate-PK entity this is NOT `id`, and writing a generated `id`
        // would leave the store key undefined so the row never round-trips.
        const id =
          typeof init[pk] === "string" && init[pk]
            ? (init[pk] as string)
            : crypto.randomUUID();
        const created = create(entitySchema, {
          ...init,
          [pk]: id,
        } as MessageInitShape<DescMessage>);
        storeFor(entity).set(String(id), created);
        return create(responseSchema, {
          [entityField]: created,
        } as MessageInitShape<DescMessage>);
      });
    }

    if (entity.update && entity.entitySchema) {
      const { rpc, responseSchema, entityField, requestField } = entity.update;
      const entitySchema = entity.entitySchema;
      table.set(key(rpc), (input) => {
        const req = (input ?? {}) as Record<string, unknown>;
        // AIP-134 requests wrap the entity ({ <entity>: {...}, updateMask })
        // — the id lives inside the wrapper; flat requests carry it at the
        // top level.
        const patch = {
          ...((req[requestField] ?? req) as Record<string, unknown>),
        };
        delete patch.$typeName;
        const id = String(patch[pk] ?? req[pk] ?? "");
        const store = storeFor(entity);
        const existing = store.get(id);
        if (!existing) {
          throw new ConnectError(
            `${entity.label} ${id} not found`,
            Code.NotFound,
          );
        }
        const updated = create(entitySchema, {
          ...(existing as Record<string, unknown>),
          ...patch,
          [pk]: id,
        } as MessageInitShape<DescMessage>);
        store.set(id, updated);
        return create(responseSchema, {
          [entityField]: updated,
        } as MessageInitShape<DescMessage>);
      });
    }

    if (entity.delete) {
      table.set(key(entity.delete.rpc), (input) => {
        // Delete by the SAME field the store is keyed by.
        const req = input as Record<string, unknown> | undefined;
        storeFor(entity).delete(String(req?.[pk]));
        // Delete returns an empty response — the standard CRUD shape.
        return {};
      });
    }
  }

  return table;
}

/**
 * Create the mock transport.
 *
 * The generated `src/lib/mock-transport.ts` supplies the resolved scenario
 * and the project's entity descriptors; `connect.ts` supplies the optional
 * real transport when running in hybrid mode.
 */
export function createMockTransport(
  options: CreateMockTransportOptions,
): Transport {
  const { scenario, entities = [], fallback } = options;
  const passthrough = scenario.passthrough === true && fallback != null;
  const fixtures = buildFixtureTable(entities);

  // Bind the object literal to a Transport-typed variable rather than
  // casting at the return site. A trailing assertion cast does not propagate
  // Connect's Transport signature backwards into the literal's method
  // bodies, so under `strict` tsc every callback parameter on unary/stream
  // errors with TS7006 ("implicitly has an 'any' type").
  const transport: Transport = {
    // Connect v2's Transport.unary signature is
    //   (method, signal, timeoutMs, header, input, contextValues)
    // — method first, no separate service arg. `method.parent.typeName` is
    // the fully-qualified service name; that is what handler keys and
    // fixture keys are matched against.
    async unary(method, signal, timeoutMs, header, input, contextValues) {
      const key = `${method.parent.typeName}/${method.name}`;

      // 1) Scenario overlay.
      const handler = scenario.handlers[key];
      if (handler) {
        const result = await handler(input as never);
        return makeUnaryResponse(method, result);
      }

      // 2) Passthrough: route to the real backend instead of fixtures.
      //    Skips fixtures entirely — in hybrid mode the whole point is
      //    "use real data for anything not explicitly stubbed".
      if (passthrough) {
        return fallback!.unary(
          method,
          signal,
          timeoutMs,
          header,
          input,
          contextValues,
        );
      }

      // 3) Base fixture dispatch, backed by the mutable per-entity stores so
      //    writes round-trip within the session. Misses are REAL errors
      //    (Code.NotFound), never a silent wrong-record fallback.
      const fixture = fixtures.get(key);
      if (fixture) {
        return makeUnaryResponse(method, fixture(input));
      }

      // 4) Nothing matched. Surface a clear error instead of hanging.
      return Promise.reject(
        new ConnectError(
          `mock-transport: no scenario handler or entity fixture for ${key}`,
          Code.Unimplemented,
        ),
      );
    },

    // Streaming: a scenario handler can return an array, iterable, async
    // iterable, or a Promise resolving to any of those. The transport adapts
    // it into the AsyncIterable<Response> shape Connect expects. If no
    // handler matches, reject with Unimplemented — base-fixture streaming is
    // intentionally not served (there is no canonical "list of N messages"
    // shape for an arbitrary streaming RPC).
    //
    // No explicit return-type annotation: the outer `const transport:
    // Transport` binding lets tsc infer the per-callback signature from
    // Connect's generic Transport.stream<I, O>. Annotating this as
    // Promise<StreamResponse<never, never>> makes the passthrough branch
    // ill-typed (TS2322) because fallback.stream returns StreamResponse<I,O>.
    async stream(method, signal, timeoutMs, header, input, contextValues) {
      const key = `${method.parent.typeName}/${method.name}`;
      const handler = scenario.handlers[key];
      if (!handler) {
        // Passthrough mirrors the unary path: in hybrid mode, unmatched
        // streaming RPCs go to the real backend.
        if (passthrough) {
          return fallback!.stream(
            method,
            signal,
            timeoutMs,
            header,
            input,
            contextValues,
          );
        }
        return Promise.reject(
          new ConnectError(
            `mock-transport: no scenario handler for streaming RPC ${key}`,
            Code.Unimplemented,
          ),
        );
      }
      // Read the request stream eagerly into an array so the scenario handler
      // sees the full client-streaming payload. For server-stream and
      // unary-into-stream-out RPCs this is a single message.
      const requestMessages: unknown[] = [];
      try {
        for await (const m of input as AsyncIterable<unknown>) {
          requestMessages.push(m);
        }
      } catch {
        // Synthetic clients sometimes pass a single value rather than an
        // iterable; treat that as an empty request payload.
      }
      const handlerInput =
        requestMessages.length <= 1 ? requestMessages[0] : requestMessages;
      const result = handler(handlerInput as never);
      return {
        service: method.parent as never,
        method: method as never,
        stream: true,
        header: new Headers(),
        message: toAsyncIterable(result) as AsyncIterable<never>,
        trailer: new Headers(),
      };
    },
  };
  return transport;
}
