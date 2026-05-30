/**
 * Shared formatting utilities for generated pages.
 * Used by list and detail page templates.
 */

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

export function isEnumLike(key: string, value: unknown): boolean {
  if (typeof value !== "string") return false;
  const enumKeys = ["status", "type", "kind", "role", "state", "category", "priority", "level"];
  return enumKeys.some((k) => key.toLowerCase().includes(k));
}

const badgeVariants = ["info", "success", "warning", "error", "neutral"] as const;

export function enumBadgeVariant(value: string): "info" | "success" | "warning" | "error" | "neutral" {
  const hash = [...value].reduce((a, c) => a + c.charCodeAt(0), 0) % badgeVariants.length;
  return badgeVariants[hash];
}
