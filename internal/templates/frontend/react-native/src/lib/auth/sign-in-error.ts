// Scaffolded by forge. Yours to own — forge will not overwrite this file.
//
// src/lib/auth/sign-in-error.ts — what to tell the person when signIn() fails.
//
// Pure: no React, no native modules. The sign-in screen (app/sign-in.tsx)
// renders `message`; `retryable` decides whether to offer "Try again".
// Cancelling the browser is NOT an error — signIn() resolves null for it —
// so it has no entry here.

import { OidcConfigError } from "./config";
import { AuthorizationError, OfflineError, OidcError } from "./oidc";
import { BrowserBusyError } from "./oidc-provider";
import { StorageError, StorageUnavailableError } from "./token-store";

export type SignInFailure =
  | "config"
  | "offline"
  | "storage-unavailable"
  | "storage"
  | "busy"
  | "denied"
  | "failed";

export interface SignInErrorInfo {
  kind: SignInFailure;
  message: string;
  retryable: boolean;
}

export function describeSignInError(e: unknown): SignInErrorInfo {
  if (e instanceof OidcConfigError) {
    return {
      kind: "config",
      message: `Sign-in is misconfigured: ${e.message}`,
      retryable: false,
    };
  }
  if (e instanceof OfflineError) {
    return {
      kind: "offline",
      message: "You appear to be offline. Check your connection and try again.",
      retryable: true,
    };
  }
  if (e instanceof StorageUnavailableError) {
    return {
      kind: "storage-unavailable",
      message:
        "This device cannot store your sign-in securely, so you cannot sign in here. " +
        "Use a development build or a physical device (Expo Go and the web target have no secure storage).",
      retryable: false,
    };
  }
  if (e instanceof StorageError) {
    return {
      kind: "storage",
      message: "Your device's secure storage refused the request. Unlock the device and try again.",
      retryable: true,
    };
  }
  if (e instanceof BrowserBusyError) {
    return {
      kind: "busy",
      message: "Another sign-in window is already open. Finish or close it, then try again.",
      retryable: true,
    };
  }
  if (e instanceof AuthorizationError) {
    return {
      kind: "denied",
      message: "Sign-in was not completed. Please try again.",
      retryable: true,
    };
  }
  // Never echo `e.message` of a protocol failure to the screen: it can carry
  // IdP text. The console has the detail.
  if (e instanceof OidcError) {
    return { kind: "failed", message: "Sign-in failed. Please try again.", retryable: true };
  }
  return {
    kind: "failed",
    message: "Something went wrong signing in. Please try again.",
    retryable: true,
  };
}
