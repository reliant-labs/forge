// Part of @reliantlabs/forge-web-runtime — the web twin of forge/pkg.
//
// The redacted shape of a failed RPC's request: what `forge.forensics` carries
// on the browser's RPC span (rpc-span.ts), so "why did this call fail for that
// user" has the arguments' shape beside the error, and never their content.
//
// ── Deny by default ──────────────────────────────────────────────────────
//
// A field is recorded only in a form that cannot carry what a person typed.
// The rules are forge's server-side forensics rules (docs/proposals/
// runtime-context.md, B.3), applied to the request message by reflection:
//
//   number, bool                      the value (a 64-bit integer beyond 2^53
//                                     as its decimal string)
//   enum                              the value's name, else its number
//   string named like an id           the value, cut to MAX_VALUE_CHARS
//     (`id`, `ids`, `*_id`, `*_ids`, `*Id`, `*ID`, `*Ids`, `*IDs`)
//   any other string                  "[len=N]": its length only
//   bytes                             "[len=N]"
//   repeated, map                     "[count=N]": its length only
//   google.protobuf.Timestamp         the ISO-8601 instant
//   google.protobuf.Duration          seconds, as a number
//   other message                     its set fields, recursively, down to
//                                     MAX_DEPTH levels ("[message]" below that)
//   field option `debug_redact`       never: the field is left out entirely
//
// Unset fields are left out, so a record names what the caller sent. Keys are
// `forensics.arg.<proto field name>`, nested fields joined with `.`
// (`forensics.arg.filter.owner_id`). At most MAX_ARGS fields are recorded;
// a record cut short says so with `forensics.truncated = true`.
//
// The `debug_redact` option survives into generated code: protoc-gen-es
// embeds each file's descriptor with its field options (only json names and
// source info are stripped), so `field.proto.options.debugRedact` is the
// option as the .proto declared it.
//
// DOM-free: the interceptors entry point (React Native) reaches it.
import {
  ScalarType,
  type DescField,
  type DescMessage,
} from "@bufbuild/protobuf";
import {
  reflect,
  type ReflectList,
  type ReflectMap,
  type ReflectMessage,
} from "@bufbuild/protobuf/reflect";

/** Attribute values a span event accepts. */
export type ForensicsValue = string | number | boolean;

/** How many levels of nested messages are walked below the request. */
export const MAX_DEPTH = 3;
/** The most fields one record carries. */
export const MAX_ARGS = 32;
/** The longest id value recorded; longer ones are cut. */
export const MAX_VALUE_CHARS = 128;

/** The event name forge's forensics records share, browser and server. */
export const FORENSICS_EVENT = "forge.forensics";
/** The attribute prefix of a recorded argument. */
export const FORENSICS_ARG_PREFIX = "forensics.arg.";

// `id`, `ids`, `owner_id`, `owner_ids` (proto's snake_case), and `ownerId`,
// `ownerID`, `ownerIds`, `ownerIDs` for a .proto that spells fields in camel.
const SNAKE_ID_RE = /(^|_)ids?$/i;
const CAMEL_ID_RE = /[a-z0-9](Id|ID|Ids|IDs)$/;

/** Whether a field's name says it holds an identifier. */
export function isIdShaped(name: string): boolean {
  return SNAKE_ID_RE.test(name) || CAMEL_ID_RE.test(name);
}

/**
 * classifyRequest returns the forensics attributes of a request message under
 * the rules in the header, keyed `forensics.arg.<path>`. Never throws: a
 * message it cannot walk yields what was classified before the failure.
 */
export function classifyRequest(
  desc: DescMessage | undefined,
  message: unknown,
): Record<string, ForensicsValue> {
  const out: Record<string, ForensicsValue> = {};
  if (!desc || typeof message !== "object" || message === null) {
    return out;
  }
  const state = { count: 0, truncated: false };
  try {
    walk(reflect(desc, message as never, false), "", 0, out, state);
  } catch {
    // Forensics must never break the call it describes.
  }
  if (state.truncated) {
    out["forensics.truncated"] = true;
  }
  return out;
}

interface WalkState {
  count: number;
  truncated: boolean;
}

function walk(
  msg: ReflectMessage,
  prefix: string,
  depth: number,
  out: Record<string, ForensicsValue>,
  state: WalkState,
): void {
  for (const field of msg.sortedFields) {
    if (state.truncated) {
      return;
    }
    if (field.proto.options?.debugRedact === true || !msg.isSet(field)) {
      continue;
    }
    const path = prefix + field.name;
    if (field.fieldKind === "message") {
      const value = msg.get(field) as ReflectMessage;
      const special = wellKnown(value);
      if (special !== undefined) {
        put(out, state, path, special);
      } else if (depth >= MAX_DEPTH) {
        put(out, state, path, "[message]");
      } else {
        walk(value, `${path}.`, depth + 1, out, state);
      }
      continue;
    }
    if (field.fieldKind === "list") {
      put(out, state, path, `[count=${(msg.get(field) as ReflectList).size}]`);
      continue;
    }
    if (field.fieldKind === "map") {
      put(out, state, path, `[count=${(msg.get(field) as ReflectMap).size}]`);
      continue;
    }
    const value = classifyLeaf(field, msg.get(field));
    if (value !== undefined) {
      put(out, state, path, value);
    }
  }
}

function put(
  out: Record<string, ForensicsValue>,
  state: WalkState,
  path: string,
  value: ForensicsValue,
): void {
  if (state.count >= MAX_ARGS) {
    state.truncated = true;
    return;
  }
  out[FORENSICS_ARG_PREFIX + path] = value;
  state.count++;
}

/** A scalar or enum field's recorded value, or undefined to leave it out. */
function classifyLeaf(
  field: DescField,
  value: unknown,
): ForensicsValue | undefined {
  if (field.fieldKind === "enum") {
    const n = Number(value);
    return field.enum.value[n]?.name ?? n;
  }
  if (field.fieldKind !== "scalar") {
    return undefined;
  }
  switch (field.scalar) {
    case ScalarType.STRING: {
      const s = String(value);
      return isIdShaped(field.name)
        ? s.slice(0, MAX_VALUE_CHARS)
        : `[len=${s.length}]`;
    }
    case ScalarType.BYTES:
      return `[len=${(value as Uint8Array).length}]`;
    case ScalarType.BOOL:
      return Boolean(value);
    default:
      return number(value);
  }
}

function number(value: unknown): ForensicsValue {
  if (typeof value === "bigint") {
    const n = Number(value);
    return Number.isSafeInteger(n) ? n : value.toString();
  }
  if (typeof value === "string") {
    // A 64-bit field generated with jstype = JS_STRING.
    return value.slice(0, MAX_VALUE_CHARS);
  }
  const n = Number(value);
  return Number.isFinite(n) ? n : String(n);
}

/** Timestamp and Duration as one readable value; undefined for any other. */
function wellKnown(msg: ReflectMessage): ForensicsValue | undefined {
  const name = msg.desc.typeName;
  if (
    name !== "google.protobuf.Timestamp" &&
    name !== "google.protobuf.Duration"
  ) {
    return undefined;
  }
  const m = msg.message as unknown as {
    seconds?: bigint | number;
    nanos?: number;
  };
  const seconds = Number(m.seconds ?? 0) + (m.nanos ?? 0) / 1e9;
  if (name === "google.protobuf.Duration") {
    return seconds;
  }
  const at = new Date(seconds * 1000);
  return Number.isNaN(at.getTime()) ? "[invalid timestamp]" : at.toISOString();
}

// ── Once per failure signature per page session ─────────────────────────

/** The most distinct failure signatures one page session records. */
export const MAX_SIGNATURES = 256;

const seen = new Set<string>();

/**
 * claimForensics reports whether the failure `procedure` + `code` should get
 * a forensics record now: true the first time this page session sees that
 * pair, false after, and false for every pair once MAX_SIGNATURES have been
 * recorded, so a page that fails in a loop costs a bounded amount.
 */
export function claimForensics(procedure: string, code: string): boolean {
  const key = `${procedure}\u0000${code}`;
  if (seen.has(key) || seen.size >= MAX_SIGNATURES) {
    return false;
  }
  seen.add(key);
  return true;
}

/** Test seam. */
export function resetForensics(): void {
  seen.clear();
}
