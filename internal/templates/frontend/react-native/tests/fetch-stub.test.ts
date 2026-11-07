// Scaffolded by forge. Yours to own — forge will not overwrite this file.
//
// The pattern for app tests that stub the network. src/lib/connect.ts uses the
// imported `expo/fetch`; tests/setup/expo-fetch.ts maps that onto
// globalThis.fetch, so replacing globalThis.fetch (or running MSW, which does
// the same) intercepts the app's real exported `transport`.
import { afterEach, describe, expect, it, jest } from "@jest/globals";
import { createFileRegistry, fromJson, type DescService } from "@bufbuild/protobuf";
import { FileDescriptorProtoSchema } from "@bufbuild/protobuf/wkt";
import { createClient } from "@connectrpc/connect";
import { transport } from "../src/lib/connect";

const file = fromJson(FileDescriptorProtoSchema, {
  name: "ping.proto",
  package: "test.v1",
  syntax: "proto3",
  messageType: ["PingRequest", "PingReply"].map((name) => ({
    name,
    field: [
      { name: "text", number: 1, type: "TYPE_STRING", label: "LABEL_OPTIONAL", jsonName: "text" },
    ],
  })),
  service: [
    {
      name: "PingService",
      method: [
        { name: "Say", inputType: ".test.v1.PingRequest", outputType: ".test.v1.PingReply" },
      ],
    },
  ],
});
const service: DescService = createFileRegistry(file, () => undefined).getService(
  "test.v1.PingService",
)!;

const realFetch = globalThis.fetch;

afterEach(() => {
  globalThis.fetch = realFetch;
});

describe("stubbing globalThis.fetch reaches the app transport", () => {
  it("serves a unary call from the stub, installed after the transport was created", async () => {
    const stub = jest.fn(async (input: unknown, init?: RequestInit) => {
      void input;
      void init;
      return new Response(JSON.stringify({ text: "from the stub" }), {
        status: 200,
        headers: { "content-type": "application/json" },
      });
    });
    globalThis.fetch = stub as unknown as typeof globalThis.fetch;

    const ping = createClient(service, transport) as unknown as {
      say(req: { text: string }): Promise<{ text: string }>;
    };
    await expect(ping.say({ text: "hi" })).resolves.toMatchObject({ text: "from the stub" });

    expect(stub).toHaveBeenCalledTimes(1);
    const first = stub.mock.calls[0] as unknown as [{ url?: string } | string];
    const url = typeof first[0] === "string" ? first[0] : String(first[0]?.url ?? first[0]);
    expect(new URL(url).pathname).toBe("/test.v1.PingService/Say");
  });
});
