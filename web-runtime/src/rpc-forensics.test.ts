// Part of @reliantlabs/forge-web-runtime — the web twin of forge/pkg.
//
// The redacted request shape a failed RPC's `forge.forensics` event carries:
// deny by default, `debug_redact` honoured, bounded.
import {
  create,
  createFileRegistry,
  type DescMessage,
} from "@bufbuild/protobuf";
import { embedFileDesc, fileDesc } from "@bufbuild/protobuf/codegenv2";
import {
  FieldDescriptorProto_Label as Label,
  FieldDescriptorProto_Type as Type,
  file_google_protobuf_timestamp,
  FileDescriptorProtoSchema,
} from "@bufbuild/protobuf/wkt";
import { afterEach, describe, expect, it } from "vitest";

import {
  claimForensics,
  classifyRequest,
  isIdShaped,
  MAX_ARGS,
  MAX_SIGNATURES,
  MAX_VALUE_CHARS,
  resetForensics,
} from "./rpc-forensics.js";

const opt = Label.OPTIONAL;

/**
 * A request message with one field of every kind the rules distinguish,
 * loaded the way generated code loads it: protoc-gen-es embeds the file
 * descriptor (embedFileDesc) and the _pb.ts module boots it (fileDesc). That
 * is the path `debug_redact` has to survive.
 */
function forensicsTestRequest(): DescMessage {
  const proto = create(FileDescriptorProtoSchema, {
    name: "forensics_test.proto",
    package: "forensics.v1",
    syntax: "proto3",
    dependency: ["google/protobuf/timestamp.proto"],
    enumType: [
      {
        name: "State",
        value: [
          { name: "STATE_UNSPECIFIED", number: 0 },
          { name: "STATE_OPEN", number: 1 },
        ],
      },
    ],
    messageType: [
      {
        name: "Filter",
        field: [
          {
            name: "owner_id",
            number: 1,
            type: Type.STRING,
            label: opt,
            jsonName: "ownerId",
          },
          {
            name: "note",
            number: 2,
            type: Type.STRING,
            label: opt,
            jsonName: "note",
          },
          {
            name: "inner",
            number: 3,
            type: Type.MESSAGE,
            typeName: ".forensics.v1.Filter",
            label: opt,
            jsonName: "inner",
          },
        ],
      },
      {
        name: "ListThingsRequest",
        field: [
          {
            name: "chat_id",
            number: 1,
            type: Type.STRING,
            label: opt,
            jsonName: "chatId",
          },
          {
            name: "query",
            number: 2,
            type: Type.STRING,
            label: opt,
            jsonName: "query",
          },
          {
            name: "page_size",
            number: 3,
            type: Type.INT32,
            label: opt,
            jsonName: "pageSize",
          },
          {
            name: "include_archived",
            number: 4,
            type: Type.BOOL,
            label: opt,
            jsonName: "includeArchived",
          },
          {
            name: "state",
            number: 5,
            type: Type.ENUM,
            typeName: ".forensics.v1.State",
            label: opt,
            jsonName: "state",
          },
          {
            name: "tag_ids",
            number: 6,
            type: Type.STRING,
            label: Label.REPEATED,
            jsonName: "tagIds",
          },
          {
            name: "labels",
            number: 7,
            type: Type.MESSAGE,
            typeName: ".forensics.v1.ListThingsRequest.LabelsEntry",
            label: Label.REPEATED,
            jsonName: "labels",
          },
          {
            name: "filter",
            number: 8,
            type: Type.MESSAGE,
            typeName: ".forensics.v1.Filter",
            label: opt,
            jsonName: "filter",
          },
          {
            name: "password",
            number: 9,
            type: Type.STRING,
            label: opt,
            jsonName: "password",
            options: { debugRedact: true },
          },
          {
            name: "since",
            number: 10,
            type: Type.MESSAGE,
            typeName: ".google.protobuf.Timestamp",
            label: opt,
            jsonName: "since",
          },
          {
            name: "blob",
            number: 11,
            type: Type.BYTES,
            label: opt,
            jsonName: "blob",
          },
          {
            name: "big",
            number: 12,
            type: Type.INT64,
            label: opt,
            jsonName: "big",
          },
          {
            name: "ownerID",
            number: 13,
            type: Type.STRING,
            label: opt,
            jsonName: "ownerID",
          },
          {
            name: "secret_id",
            number: 14,
            type: Type.STRING,
            label: opt,
            jsonName: "secretId",
            options: { debugRedact: true },
          },
        ],
        nestedType: [
          {
            name: "LabelsEntry",
            options: { mapEntry: true },
            field: [
              {
                name: "key",
                number: 1,
                type: Type.STRING,
                label: opt,
                jsonName: "key",
              },
              {
                name: "value",
                number: 2,
                type: Type.STRING,
                label: opt,
                jsonName: "value",
              },
            ],
          },
        ],
      },
    ],
  });
  const file = fileDesc(embedFileDesc(proto).base64(), [
    file_google_protobuf_timestamp,
  ]);
  const desc = file.messages.find((m) => m.name === "ListThingsRequest");
  if (!desc) throw new Error("test message did not register");
  return desc;
}

/** A message of `n` int32 fields, f0..f<n-1>. */
function wideMessage(n: number): DescMessage {
  const registry = createFileRegistry(
    create(FileDescriptorProtoSchema, {
      name: "forensics_wide.proto",
      package: "forensics.v1",
      syntax: "proto3",
      messageType: [
        {
          name: "Wide",
          field: Array.from({ length: n }, (_, i) => ({
            name: `f${i}`,
            number: i + 1,
            type: Type.INT32,
            label: opt,
            jsonName: `f${i}`,
          })),
        },
      ],
    }),
    () => undefined,
  );
  const desc = registry.getMessage("forensics.v1.Wide");
  if (!desc) throw new Error("wide message did not register");
  return desc;
}

describe("classifyRequest", () => {
  const desc = forensicsTestRequest();

  it("records values only where they cannot carry what a person typed", () => {
    const msg = create(desc, {
      chatId: "chat_123",
      query: "my medical history",
      pageSize: 50,
      includeArchived: true,
      state: 1,
      tagIds: ["a", "b", "c"],
      labels: { team: "infra", env: "prod" },
      filter: { ownerId: "u_9", note: "call me at 555-0100" },
      password: "hunter2",
      since: { seconds: 1_700_000_000n, nanos: 0 },
      blob: new Uint8Array([1, 2, 3, 4]),
      big: 9_007_199_254_740_993n,
      ownerID: "u_1",
      secretId: "s_1",
    } as never);
    expect(classifyRequest(desc, msg)).toEqual({
      "forensics.arg.chat_id": "chat_123",
      "forensics.arg.query": "[len=18]",
      "forensics.arg.page_size": 50,
      "forensics.arg.include_archived": true,
      "forensics.arg.state": "STATE_OPEN",
      "forensics.arg.tag_ids": "[count=3]",
      "forensics.arg.labels": "[count=2]",
      "forensics.arg.filter.owner_id": "u_9",
      "forensics.arg.filter.note": "[len=19]",
      "forensics.arg.since": "2023-11-14T22:13:20.000Z",
      "forensics.arg.blob": "[len=4]",
      "forensics.arg.big": "9007199254740993",
      "forensics.arg.ownerID": "u_1",
    });
  });

  it("never records a debug_redact field, not even its presence", () => {
    const out = classifyRequest(
      desc,
      create(desc, { password: "hunter2", secretId: "s_1" } as never),
    );
    expect(out).toEqual({});
  });

  it("leaves unset fields out", () => {
    expect(
      classifyRequest(desc, create(desc, { pageSize: 10 } as never)),
    ).toEqual({
      "forensics.arg.page_size": 10,
    });
  });

  it("walks nested messages only so deep", () => {
    const msg = create(desc, {
      filter: { inner: { inner: { inner: { ownerId: "deep" } } } },
    } as never);
    expect(classifyRequest(desc, msg)).toEqual({
      "forensics.arg.filter.inner.inner.inner": "[message]",
    });
  });

  it("cuts long id values and says when a record is cut short", () => {
    const long = "x".repeat(MAX_VALUE_CHARS + 50);
    expect(
      classifyRequest(desc, create(desc, { chatId: long } as never))[
        "forensics.arg.chat_id"
      ],
    ).toHaveLength(MAX_VALUE_CHARS);

    const wide = wideMessage(MAX_ARGS + 8);
    const init: Record<string, number> = {};
    for (let i = 0; i < MAX_ARGS + 8; i++) init[`f${i}`] = i + 1;
    const out = classifyRequest(wide, create(wide, init as never));
    expect(
      Object.keys(out).filter((k) => k.startsWith("forensics.arg.")),
    ).toHaveLength(MAX_ARGS);
    expect(out["forensics.truncated"]).toBe(true);
  });

  it("never throws, whatever it is handed", () => {
    expect(classifyRequest(undefined, {})).toEqual({});
    expect(classifyRequest(desc, null)).toEqual({});
    expect(classifyRequest(desc, { $typeName: "nope", chatId: 5 })).toBeTypeOf(
      "object",
    );
  });
});

describe("isIdShaped", () => {
  it("knows an identifier by its name", () => {
    for (const name of [
      "id",
      "ids",
      "chat_id",
      "tag_ids",
      "chatId",
      "chatID",
      "tagIds",
      "tagIDs",
    ]) {
      expect(isIdShaped(name), name).toBe(true);
    }
    for (const name of [
      "identity",
      "idea",
      "video",
      "paid",
      "valid",
      "android",
    ]) {
      expect(isIdShaped(name), name).toBe(false);
    }
  });
});

describe("claimForensics", () => {
  afterEach(resetForensics);

  it("records a failure signature once per page session", () => {
    expect(claimForensics("/a.v1.S/M", "not_found")).toBe(true);
    expect(claimForensics("/a.v1.S/M", "not_found")).toBe(false);
    expect(claimForensics("/a.v1.S/M", "internal")).toBe(true);
    expect(claimForensics("/a.v1.S/N", "not_found")).toBe(true);
  });

  it("stops after MAX_SIGNATURES distinct signatures", () => {
    for (let i = 0; i < MAX_SIGNATURES; i++) {
      expect(claimForensics(`/a.v1.S/M${i}`, "internal")).toBe(true);
    }
    expect(claimForensics("/a.v1.S/Other", "internal")).toBe(false);
  });
});
