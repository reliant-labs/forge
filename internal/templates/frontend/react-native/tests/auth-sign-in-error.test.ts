// Scaffolded by forge. Yours to own — forge will not overwrite this file.
import { describe, expect, it } from "@jest/globals";

import { OidcConfigError } from "../src/lib/auth/config";
import {
  AuthorizationError,
  OfflineError,
  ProtocolError,
  StateMismatchError,
} from "../src/lib/auth/oidc";
import { BrowserBusyError } from "../src/lib/auth/oidc-provider";
import { describeSignInError } from "../src/lib/auth/sign-in-error";
import { StorageError, StorageUnavailableError } from "../src/lib/auth/token-store";

describe("describeSignInError", () => {
  it.each([
    [new OidcConfigError("EXPO_PUBLIC_FORGE_OIDC_ISSUER", "bad"), "config", false],
    [new OfflineError("down"), "offline", true],
    [new StorageUnavailableError(), "storage-unavailable", false],
    [new StorageError("write", new Error("locked")), "storage", true],
    [new BrowserBusyError("busy"), "busy", true],
    [new AuthorizationError("access_denied", null), "denied", true],
    [new StateMismatchError("state"), "failed", true],
    [new Error("boom"), "failed", true],
  ])("%s → %s (retryable %s)", (error, kind, retryable) => {
    const info = describeSignInError(error);
    expect(info.kind).toBe(kind);
    expect(info.retryable).toBe(retryable);
    expect(info.message.length).toBeGreaterThan(10);
  });

  it("does not echo IdP-controlled protocol text to the screen", () => {
    const info = describeSignInError(new ProtocolError("secret-ish detail from the IdP"));
    expect(info.message).not.toContain("secret-ish");
  });
});
