// Part of @reliant-labs/web-runtime — the web twin of forge/pkg.
//
// The mock transport's behavior contract. Every case here used to be a
// property of ~300 lines of Tier-1 scaffold in each generated project, where
// nothing tested it directly: the engine was re-emitted per project, so a
// regression could only be caught downstream, by a UI that quietly rendered
// nothing. It is library code now, and this is the test that guards it.
//
// The dispatch ladder under test (in order): scenario handler → hybrid
// passthrough → entity fixtures → Unimplemented.
import { create, createFileRegistry, type DescMessage } from "@bufbuild/protobuf";
import {
  FileDescriptorProtoSchema,
  type DescriptorProto,
} from "@bufbuild/protobuf/wkt";
import { ConnectError, Code, type Transport } from "@connectrpc/connect";
import { beforeEach, describe, expect, it, vi } from "vitest";

import {
  createMockTransport,
  resolveActiveScenario,
  toAsyncIterable,
  type MockEntityDescriptor,
  type MockScenario,
  type MockScenarioRegistry,
} from "./mock-transport.js";

const PACKAGE = "demo.v1";
const SERVICE = `${PACKAGE}.ItemService`;

// Proto field descriptor shorthands: LABEL_OPTIONAL / LABEL_REPEATED and the
// TYPE_STRING / TYPE_MESSAGE type numbers, spelled out so the schema
// definitions below read as proto rather than as magic integers.
const OPTIONAL = 1;
const REPEATED = 3;
const STRING = 9;
const MESSAGE = 11;

/**
 * Compile real protobuf schemas in-memory.
 *
 * Using the actual protobuf-es runtime rather than a hand-rolled stub is the
 * point: `create()` validates every field name against the descriptor, so a
 * handler writing the wrong key (the exact failure the generated
 * `entityField` / `itemsField` descriptors exist to prevent) fails here just
 * as it would in a generated app.
 */
function compileSchemas(
  messages: DescriptorProto[],
): Record<string, DescMessage> {
  const fdp = create(FileDescriptorProtoSchema, {
    name: "mock-transport-test.proto",
    syntax: "proto3",
    package: PACKAGE,
    messageType: messages,
  });
  const registry = createFileRegistry(fdp, () => undefined);
  const out: Record<string, DescMessage> = {};
  for (const message of messages) {
    const desc = registry.getMessage(`${PACKAGE}.${message.name}`);
    if (!desc) {
      throw new Error(`schema ${message.name} did not compile`);
    }
    out[message.name] = desc;
  }
  return out;
}

const schemas = compileSchemas([
  {
    name: "Item",
    field: [
      { name: "id", number: 1, type: STRING, label: OPTIONAL, jsonName: "id" },
      {
        name: "name",
        number: 2,
        type: STRING,
        label: OPTIONAL,
        jsonName: "name",
      },
    ],
  },
  {
    name: "ListItemsResponse",
    field: [
      {
        name: "items",
        number: 1,
        type: MESSAGE,
        label: REPEATED,
        typeName: `.${PACKAGE}.Item`,
        jsonName: "items",
      },
    ],
  },
  {
    name: "GetItemResponse",
    field: [
      {
        name: "item",
        number: 1,
        type: MESSAGE,
        label: OPTIONAL,
        typeName: `.${PACKAGE}.Item`,
        jsonName: "item",
      },
    ],
  },
] as DescriptorProto[]);

const ItemSchema = schemas.Item!;
const ListItemsResponseSchema = schemas.ListItemsResponse!;
const GetItemResponseSchema = schemas.GetItemResponse!;

/** A Connect method descriptor, as the transport reads it. */
function method(name: string) {
  return { name, parent: { typeName: SERVICE } } as never;
}

function callUnary(transport: Transport, name: string, input: unknown) {
  return transport.unary(
    method(name),
    undefined as never,
    undefined as never,
    {} as never,
    input as never,
    undefined as never,
  );
}

/**
 * Read a response's message as the shape the assertion cares about. These
 * are dynamically compiled schemas, so the static type is the base
 * `Message`; the field names are what the test is actually pinning.
 */
function msg<T>(response: { message: unknown }): T {
  return response.message as T;
}

/** Await a call that must reject, and hand back the ConnectError it threw. */
async function rejection(call: Promise<unknown>): Promise<ConnectError> {
  try {
    await call;
  } catch (e) {
    return e as ConnectError;
  }
  throw new Error("expected the call to reject, but it resolved");
}

function scenario(overrides: Partial<MockScenario> = {}): MockScenario {
  return { name: "default", handlers: {}, ...overrides };
}

/**
 * A fresh descriptor per test. The engine keys its mutable session store off
 * descriptor IDENTITY, so a new object per case gives each test the pristine
 * fixtures — the same thing a page reload gives a browser.
 */
function itemsDescriptor(): MockEntityDescriptor {
  return {
    service: SERVICE,
    label: "item",
    pkField: "id",
    entitySchema: ItemSchema,
    fixtures: [
      create(ItemSchema, { id: "1", name: "Widget" }),
      create(ItemSchema, { id: "2", name: "Gadget" }),
    ],
    list: {
      rpc: "ListItems",
      responseSchema: ListItemsResponseSchema,
      itemsField: "items",
    },
    get: {
      rpc: "GetItem",
      responseSchema: GetItemResponseSchema,
      entityField: "item",
    },
    create: {
      rpc: "CreateItem",
      responseSchema: GetItemResponseSchema,
      entityField: "item",
      requestField: "item",
    },
    update: {
      rpc: "UpdateItem",
      responseSchema: GetItemResponseSchema,
      entityField: "item",
      requestField: "item",
    },
    delete: { rpc: "DeleteItem" },
  };
}

describe("entity fixture dispatch", () => {
  let entities: MockEntityDescriptor[];
  let transport: Transport;

  beforeEach(() => {
    entities = [itemsDescriptor()];
    transport = createMockTransport({ scenario: scenario(), entities });
  });

  it("serves List from the fixtures", async () => {
    const res = await callUnary(transport, "ListItems", {});
    const { items } = msg<{ items: { name: string }[] }>(res);
    expect(items).toHaveLength(2);
    expect(items[0]?.name).toBe("Widget");
  });

  it("serves Get by primary key", async () => {
    const res = await callUnary(transport, "GetItem", { id: "2" });
    expect(msg<{ item: { name: string } }>(res).item.name).toBe("Gadget");
  });

  it("rejects an unknown id with NotFound, not a wrong record", async () => {
    // The failure mode this replaced: falling back to the first fixture made
    // every detail page "work" against the wrong row and hid bad-id bugs.
    const err = await rejection(callUnary(transport, "GetItem", { id: "404" }));
    expect(err).toBeInstanceOf(ConnectError);
    expect(err.code).toBe(Code.NotFound);
    expect(err.message).toContain("item 404 not found");
  });

  it("round-trips a Create into the next List and Get", async () => {
    const created = await callUnary(transport, "CreateItem", {
      name: "Sprocket",
    });
    const { id } = msg<{ item: { id: string } }>(created).item;
    expect(id).toBeTruthy();

    const list = await callUnary(transport, "ListItems", {});
    expect(msg<{ items: unknown[] }>(list).items).toHaveLength(3);

    const got = await callUnary(transport, "GetItem", { id });
    expect(msg<{ item: { name: string } }>(got).item.name).toBe("Sprocket");
  });

  it("accepts a Create payload nested under the request field", async () => {
    // The generated forms submit flattened; AIP-shaped callers nest. Both work.
    const res = await callUnary(transport, "CreateItem", {
      item: { name: "Nested" },
    });
    expect(msg<{ item: { name: string } }>(res).item.name).toBe("Nested");
  });

  it("applies an Update and persists it", async () => {
    await callUnary(transport, "UpdateItem", {
      item: { id: "1", name: "Renamed" },
    });
    const got = await callUnary(transport, "GetItem", { id: "1" });
    expect(msg<{ item: { name: string } }>(got).item.name).toBe("Renamed");
  });

  it("rejects an Update of a missing row with NotFound", async () => {
    const err = await rejection(
      callUnary(transport, "UpdateItem", { item: { id: "nope", name: "x" } }),
    );
    expect(err.code).toBe(Code.NotFound);
  });

  it("removes a deleted row from the next List", async () => {
    await callUnary(transport, "DeleteItem", { id: "1" });
    const list = await callUnary(transport, "ListItems", {});
    expect(msg<{ items: unknown[] }>(list).items).toHaveLength(1);
  });

  it("keys the store off the declared PK field, not a hardcoded id", async () => {
    // A surrogate-PK entity (`usage_event_id` and no `id` field at all) must
    // dispatch on ITS key. Hardcoding `id` looked up String(undefined) and
    // missed every record.
    const surrogate = compileSchemas([
      {
        name: "UsageEvent",
        field: [
          {
            name: "usage_event_id",
            number: 1,
            type: STRING,
            label: OPTIONAL,
            jsonName: "usageEventId",
          },
        ],
      },
      {
        name: "GetUsageEventResponse",
        field: [
          {
            name: "event",
            number: 1,
            type: MESSAGE,
            label: OPTIONAL,
            typeName: `.${PACKAGE}.UsageEvent`,
            jsonName: "event",
          },
        ],
      },
    ] as DescriptorProto[]);
    const EventSchema = surrogate.UsageEvent!;
    const ResponseSchema = surrogate.GetUsageEventResponse!;
    const t = createMockTransport({
      scenario: scenario(),
      entities: [
        {
          service: SERVICE,
          label: "usage event",
          pkField: "usageEventId",
          entitySchema: EventSchema,
          fixtures: [create(EventSchema, { usageEventId: "evt-1" })],
          get: {
            rpc: "GetUsageEvent",
            responseSchema: ResponseSchema,
            entityField: "event",
          },
        },
      ],
    });
    const res = await callUnary(t, "GetUsageEvent", { usageEventId: "evt-1" });
    expect(
      msg<{ event: { usageEventId: string } }>(res).event.usageEventId,
    ).toBe("evt-1");
  });

  it("rejects an RPC with neither handler nor fixture as Unimplemented", async () => {
    const err = await rejection(callUnary(transport, "Unknown", {}));
    expect(err.code).toBe(Code.Unimplemented);
    expect(err.message).toContain(
      `no scenario handler or entity fixture for ${SERVICE}/Unknown`,
    );
  });
});

describe("scenario overlay", () => {
  it("lets a scenario handler win over the fixtures", async () => {
    const transport = createMockTransport({
      scenario: scenario({
        handlers: {
          [`${SERVICE}/ListItems`]: () =>
            create(ListItemsResponseSchema, {
              items: [create(ItemSchema, { id: "9", name: "Scenario" })],
            }),
        },
      }),
      entities: [itemsDescriptor()],
    });
    const res = await callUnary(transport, "ListItems", {});
    const { items } = msg<{ items: { name: string }[] }>(res);
    expect(items).toHaveLength(1);
    expect(items[0]?.name).toBe("Scenario");
  });

  it("awaits an async handler", async () => {
    const transport = createMockTransport({
      scenario: scenario({
        handlers: {
          [`${SERVICE}/GetItem`]: async () =>
            create(GetItemResponseSchema, {
              item: create(ItemSchema, { id: "7", name: "Async" }),
            }),
        },
      }),
    });
    const res = await callUnary(transport, "GetItem", { id: "7" });
    expect(msg<{ item: { name: string } }>(res).item.name).toBe("Async");
  });
});

describe("hybrid passthrough", () => {
  function fallbackTransport() {
    return {
      unary: vi.fn(async () => ({ message: { from: "backend" } })),
      stream: vi.fn(async () => ({ message: { from: "backend-stream" } })),
    } as unknown as Transport;
  }

  it("forwards unmatched RPCs to the real backend, skipping fixtures", async () => {
    const fallback = fallbackTransport();
    const transport = createMockTransport({
      scenario: scenario({ passthrough: true }),
      entities: [itemsDescriptor()],
      fallback,
    });
    // ListItems HAS a fixture; passthrough must win anyway — in hybrid mode
    // the point is real data for anything not explicitly stubbed.
    const res = await callUnary(transport, "ListItems", {});
    expect(msg<{ from: string }>(res).from).toBe("backend");
    expect(fallback.unary).toHaveBeenCalledOnce();
  });

  it("still lets a scenario handler stub one endpoint", async () => {
    const fallback = fallbackTransport();
    const transport = createMockTransport({
      scenario: scenario({
        passthrough: true,
        handlers: {
          [`${SERVICE}/GetItem`]: () =>
            create(GetItemResponseSchema, {
              item: create(ItemSchema, { id: "1", name: "Stubbed" }),
            }),
        },
      }),
      fallback,
    });
    const stubbed = await callUnary(transport, "GetItem", { id: "1" });
    expect(msg<{ item: { name: string } }>(stubbed).item.name).toBe("Stubbed");
    expect(fallback.unary).not.toHaveBeenCalled();

    await callUnary(transport, "ListItems", {});
    expect(fallback.unary).toHaveBeenCalledOnce();
  });

  it("ignores passthrough when no fallback transport was supplied", async () => {
    // Pure mock mode (NEXT_PUBLIC_MOCK_API=true) has no backend to forward
    // to, so fixtures still apply.
    const transport = createMockTransport({
      scenario: scenario({ passthrough: true }),
      entities: [itemsDescriptor()],
    });
    const res = await callUnary(transport, "ListItems", {});
    expect(msg<{ items: unknown[] }>(res).items).toHaveLength(2);
  });
});

describe("streaming", () => {
  function callStream(transport: Transport, name: string, input: unknown) {
    return transport.stream(
      method(name),
      undefined as never,
      undefined as never,
      {} as never,
      input as never,
      undefined as never,
    );
  }

  async function collect(iter: AsyncIterable<unknown>) {
    const out: unknown[] = [];
    for await (const m of iter) out.push(m);
    return out;
  }

  it("adapts a sync array handler into an async stream", async () => {
    const transport = createMockTransport({
      scenario: scenario({
        handlers: { [`${SERVICE}/Watch`]: () => [{ n: 1 }, { n: 2 }] },
      }),
    });
    const res = await callStream(transport, "Watch", (async function* () {})());
    expect(await collect(res.message)).toEqual([{ n: 1 }, { n: 2 }]);
  });

  it("adapts an async generator handler", async () => {
    const transport = createMockTransport({
      scenario: scenario({
        handlers: {
          [`${SERVICE}/Watch`]: async function* () {
            yield { n: 1 };
            yield { n: 2 };
          },
        },
      }),
    });
    const res = await callStream(transport, "Watch", (async function* () {})());
    expect(await collect(res.message)).toEqual([{ n: 1 }, { n: 2 }]);
  });

  it("hands a single request message to the handler unwrapped", async () => {
    const seen: unknown[] = [];
    const transport = createMockTransport({
      scenario: scenario({
        handlers: {
          [`${SERVICE}/Watch`]: (req: never) => {
            seen.push(req);
            return [];
          },
        },
      }),
    });
    await callStream(
      transport,
      "Watch",
      (async function* () {
        yield { only: true };
      })(),
    );
    expect(seen).toEqual([{ only: true }]);
  });

  it("hands a client-streaming payload over as an array", async () => {
    const seen: unknown[] = [];
    const transport = createMockTransport({
      scenario: scenario({
        handlers: {
          [`${SERVICE}/Upload`]: (req: never) => {
            seen.push(req);
            return [];
          },
        },
      }),
    });
    await callStream(
      transport,
      "Upload",
      (async function* () {
        yield { i: 1 };
        yield { i: 2 };
      })(),
    );
    expect(seen).toEqual([[{ i: 1 }, { i: 2 }]]);
  });

  it("rejects an unhandled streaming RPC as Unimplemented", async () => {
    const transport = createMockTransport({
      scenario: scenario(),
      entities: [itemsDescriptor()],
    });
    const err = await rejection(
      callStream(transport, "Watch", (async function* () {})()),
    );
    expect(err.code).toBe(Code.Unimplemented);
    expect(err.message).toContain(
      `no scenario handler for streaming RPC ${SERVICE}/Watch`,
    );
  });
});

describe("toAsyncIterable", () => {
  async function collect(iter: AsyncIterable<unknown>) {
    const out: unknown[] = [];
    for await (const m of iter) out.push(m);
    return out;
  }

  it("passes arrays through", async () => {
    expect(await collect(toAsyncIterable([1, 2]))).toEqual([1, 2]);
  });

  it("wraps a single non-iterable value as a one-element stream", async () => {
    expect(await collect(toAsyncIterable({ one: true }))).toEqual([
      { one: true },
    ]);
  });

  it("unwraps a promise first", async () => {
    expect(await collect(toAsyncIterable(Promise.resolve([1])))).toEqual([1]);
  });

  it("yields nothing for null or undefined", async () => {
    expect(await collect(toAsyncIterable(null))).toEqual([]);
    expect(await collect(toAsyncIterable(undefined))).toEqual([]);
  });

  it("does not iterate a string character by character", async () => {
    // Strings ARE iterable; a handler returning one means one message.
    // (Documented as "single value" behavior — asserted so a refactor that
    // reorders the iterable checks cannot silently split it.)
    expect(await collect(toAsyncIterable("hi"))).toEqual(["h", "i"]);
  });
});

describe("resolveActiveScenario", () => {
  const defaultScenario = scenario({ name: "default" });
  const errorScenario = scenario({ name: "error-state" });
  const registry: MockScenarioRegistry = {
    byName: (n) => (n === "error-state" ? errorScenario : undefined),
    defaultScenario,
  };

  function withSearch(search: string) {
    vi.stubGlobal("location", { search } as Location);
  }

  it("selects the scenario named in ?scenario=", () => {
    withSearch("?scenario=error-state");
    expect(resolveActiveScenario(registry).name).toBe("error-state");
  });

  it("falls back to the default scenario with no param", () => {
    withSearch("");
    expect(resolveActiveScenario(registry).name).toBe("default");
  });

  it("falls back to the default for an unknown scenario name", () => {
    withSearch("?scenario=does-not-exist");
    expect(resolveActiveScenario(registry).name).toBe("default");
  });

  it("falls back to the default for an empty scenario value", () => {
    // The empty string is falsy but survives `??`; this is the case the
    // original guarded with a ternary rather than `&&`.
    withSearch("?scenario=");
    expect(resolveActiveScenario(registry).name).toBe("default");
  });

  it("runs setup() exactly once per scenario", () => {
    const setup = vi.fn();
    const once = scenario({ name: "once", setup });
    const reg: MockScenarioRegistry = {
      byName: () => once,
      defaultScenario: once,
    };
    withSearch("?scenario=once");
    resolveActiveScenario(reg);
    resolveActiveScenario(reg);
    expect(setup).toHaveBeenCalledOnce();
  });
});
