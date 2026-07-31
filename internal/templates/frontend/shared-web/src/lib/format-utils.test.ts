// Guards the badge-variant resolution in src/lib/format-utils.ts.
//
// The bug this pins down: an unregistered status used to fall through to a
// SUBSTRING "semantic guess", and "inactive" CONTAINS "active" — so a retired
// patient rendered success-green. The same inversion held for every common
// English negation: incomplete/complete, disabled/enabled, unhealthy/healthy,
// unverified/verified, disconnected/connected. A wrong colour is worse than no
// colour — grey reads "nobody registered this", green reads "this is fine" —
// so the guess is gone and an unknown status is neutral.
//
// Lives under the browser-shared root because it is a Vitest suite: Next.js
// and Vite SPA both run `vitest run`. React Native scaffolds the same
// format-utils.ts but runs jest-expo, which cannot resolve `vitest`.

import { describe, expect, it } from "vitest";

import { enumBadgeVariant, formatValue, registerStatusVariants } from "@/lib/format-utils";

// A protobuf-es v2 numeric enum, carrying both the forward and reverse entries.
enum PatientStatus {
  UNSPECIFIED = 0,
  ACTIVE = 1,
  INACTIVE = 2,
  SENT_TO_PHARMACY = 3,
}

describe("enumBadgeVariant", () => {
  // The measured defect, verbatim: a retired patient rendered as active.
  it("never renders a negated status as success", () => {
    for (const status of [
      "inactive",
      "incomplete",
      "disabled",
      "unhealthy",
      "unverified",
      "disconnected",
    ]) {
      expect(enumBadgeVariant(status)).not.toBe("success");
    }
  });

  it("still renders the affirmative form as success", () => {
    for (const status of [
      "active",
      "completed",
      "enabled",
      "healthy",
      "verified",
      "connected",
    ]) {
      expect(enumBadgeVariant(status)).toBe("success");
    }
  });

  // The inversion also reached the badge through the enum object, which is how
  // every generated page calls it.
  it("does not invert a negated member name resolved from an enum object", () => {
    expect(enumBadgeVariant(PatientStatus.ACTIVE, PatientStatus)).toBe("success");
    expect(enumBadgeVariant(PatientStatus.INACTIVE, PatientStatus)).not.toBe("success");
  });

  it("is neutral for an unset value", () => {
    expect(enumBadgeVariant(null)).toBe("neutral");
    expect(enumBadgeVariant(undefined)).toBe("neutral");
    expect(enumBadgeVariant("")).toBe("neutral");
  });

  // Neutral, not a guess: an unregistered domain status has no derivable
  // colour, and saying so is the honest answer.
  it("is neutral for an unregistered domain status", () => {
    expect(enumBadgeVariant("sent_to_pharmacy")).toBe("neutral");
    expect(enumBadgeVariant("payment_captured")).toBe("neutral");
  });

  // …and registerStatusVariants is where the answer gets DECLARED. Runs last:
  // registration mutates module state for the rest of the file.
  it("colours a domain status once the project registers it", () => {
    registerStatusVariants({
      sent_to_pharmacy: "info",
      capture_failed: "danger", // alias for "error"
      renewal_required: "warning",
    });

    expect(enumBadgeVariant("sent_to_pharmacy")).toBe("info");
    expect(enumBadgeVariant("SENT_TO_PHARMACY")).toBe("info");
    expect(enumBadgeVariant(PatientStatus.SENT_TO_PHARMACY, PatientStatus)).toBe("info");
    expect(enumBadgeVariant("capture_failed")).toBe("error");
    expect(enumBadgeVariant("renewal_required")).toBe("warning");
  });
});

// Guards the `bytes` column rendering in src/lib/format-utils.ts.
//
// The bug this pins down: a protobuf-es `bytes` field is a Uint8Array at
// runtime, and formatValue's final `String(value)` turned it into the
// comma-joined byte values — a 3-byte blob rendered as the literal text
// "1,2,3", and a 40 KB thumbnail rendered as a five-figure wall of digits
// that broke the table layout. `bytes` is the ONE proto scalar kind whose
// JS representation has no readable string form, so it is the one kind
// formatValue has to answer for explicitly.
describe("formatValue for a bytes column", () => {
  it("never renders the comma-joined byte values", () => {
    // The measured defect, verbatim.
    expect(formatValue(new Uint8Array([1, 2, 3]))).not.toBe("1,2,3");
    // …and no size string may contain the byte list for ANY blob: the
    // assertion is derived from the value under test, not from a
    // remembered example.
    for (const len of [1, 2, 3, 17, 999, 1000, 4096, 5_000_000]) {
      const blob = new Uint8Array(len).fill(7);
      expect(formatValue(blob)).not.toContain(Array.from(blob.slice(0, 3)).join(","));
    }
  });

  it("renders the blob's size", () => {
    expect(formatValue(new Uint8Array(0))).toBe("0 bytes");
    expect(formatValue(new Uint8Array(1))).toBe("1 byte");
    expect(formatValue(new Uint8Array([1, 2, 3]))).toBe("3 bytes");
    expect(formatValue(new Uint8Array(999))).toBe("999 bytes");
    expect(formatValue(new Uint8Array(1200))).toBe("1.2 KB");
    expect(formatValue(new Uint8Array(812_000))).toBe("812 KB");
    expect(formatValue(new Uint8Array(3_400_000))).toBe("3.4 MB");
  });

  // An unset column and a present-but-empty blob are different facts. Only
  // null/undefined are unset; a zero-length blob is a value.
  it("distinguishes an empty blob from an unset column", () => {
    expect(formatValue(new Uint8Array(0))).toBe("0 bytes");
    expect(formatValue(null)).toBe("—");
    expect(formatValue(undefined)).toBe("—");
  });

  // The size must MOVE with the blob — a formatter that returned a constant
  // (or read the wrong property) would satisfy "not 1,2,3" while telling the
  // reader nothing.
  it("reports a size that tracks the blob's length", () => {
    const seen = new Set<string>();
    for (const len of [0, 1, 500, 1500, 250_000, 7_000_000]) {
      seen.add(formatValue(new Uint8Array(len)));
    }
    expect(seen.size).toBe(6);
  });
});
