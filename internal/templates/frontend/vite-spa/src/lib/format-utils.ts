/**
 * Shared formatting utilities for generated pages.
 * Used by list, detail, and edit page templates, plus the error-toast
 * chokepoint in query-client.ts.
 */

import { ConnectError } from "@connectrpc/connect";

export function formatValue(value: unknown): string {
  if (value === null || value === undefined) return "—";
  if (typeof value === "boolean") return value ? "Yes" : "No";
  // Handle protobuf-es Timestamp objects (have seconds/nanos properties)
  if (typeof value === "object" && value !== null && "seconds" in value) {
    try {
      const ts = value as { seconds: bigint; nanos?: number };
      return new Date(Number(ts.seconds) * 1000).toLocaleDateString(undefined, {
        year: "numeric",
        month: "short",
        day: "numeric",
        hour: "2-digit",
        minute: "2-digit",
      });
    } catch {
      /* fall through */
    }
  }
  const s = String(value);
  if (/^\d{4}-\d{2}-\d{2}T/.test(s)) {
    try {
      return new Date(s).toLocaleDateString(undefined, {
        year: "numeric",
        month: "short",
        day: "numeric",
        hour: "2-digit",
        minute: "2-digit",
      });
    } catch {
      return s;
    }
  }
  return s;
}

/**
 * userMessage — turn an RPC/runtime error into copy fit for end users.
 *
 * ConnectError.message prefixes the gRPC code ("[not_found] no such task");
 * rawMessage is the server's human-readable text without that framing.
 * Use this everywhere an error is shown (banners, toasts) instead of
 * `err.message`.
 */
export function userMessage(err: unknown): string {
  if (err instanceof ConnectError) {
    return err.rawMessage || "Something went wrong. Please try again.";
  }
  if (err instanceof Error) {
    return err.message || "Something went wrong. Please try again.";
  }
  return String(err);
}

/**
 * toDatetimeLocal — convert a proto Timestamp / ISO string / Date into the
 * `YYYY-MM-DDTHH:mm` shape an <input type="datetime-local"> expects.
 * Returns "" for unset values so controlled inputs stay controlled.
 */
export function toDatetimeLocal(value: unknown): string {
  let d: Date | null = null;
  if (value instanceof Date) {
    d = value;
  } else if (typeof value === "object" && value !== null && "seconds" in value) {
    const ts = value as { seconds: bigint };
    d = new Date(Number(ts.seconds) * 1000);
  } else if (typeof value === "string" && value !== "") {
    const parsed = new Date(value);
    if (!Number.isNaN(parsed.getTime())) d = parsed;
  }
  if (!d) return "";
  const pad = (n: number) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

export function isEnumLike(key: string, value: unknown): boolean {
  if (typeof value !== "string") return false;
  const enumKeys = ["status", "type", "kind", "role", "state", "category", "priority", "level"];
  return enumKeys.some((k) => key.toLowerCase().includes(k));
}

export type BadgeVariant = "info" | "success" | "warning" | "error" | "neutral";

/**
 * statusVariants — explicit status-word → badge-variant map. Extend it with
 * your domain's vocabulary; anything unknown renders neutral. (This replaced
 * a hash-of-charcodes scheme that assigned semantic colors at random —
 * "failed" could render green and "active" red, differently per value.)
 */
const statusVariants: Record<string, BadgeVariant> = {
  active: "success",
  approved: "success",
  completed: "success",
  connected: "success",
  done: "success",
  enabled: "success",
  healthy: "success",
  online: "success",
  paid: "success",
  ready: "success",
  succeeded: "success",
  verified: "success",

  draft: "info",
  in_progress: "info",
  new: "info",
  open: "info",
  running: "info",
  scheduled: "info",
  trial: "info",

  degraded: "warning",
  expiring: "warning",
  paused: "warning",
  pending: "warning",
  retrying: "warning",
  suspended: "warning",
  warning: "warning",

  blocked: "error",
  canceled: "error",
  cancelled: "error",
  declined: "error",
  deleted: "error",
  disabled: "error",
  error: "error",
  expired: "error",
  failed: "error",
  offline: "error",
  rejected: "error",
  unhealthy: "error",
};

export function enumBadgeVariant(value: string): BadgeVariant {
  return statusVariants[value.trim().toLowerCase()] ?? "neutral";
}
