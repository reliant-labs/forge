// Scaffolded by forge. Yours to own — forge will not overwrite this file.
//
// The device bindings in src/lib/auth/native.ts: what the auth core actually
// hands the system browser.
import { beforeEach, describe, expect, it, jest } from "@jest/globals";

import { nativeBrowser } from "../src/lib/auth/native";

type OpenOptions = Record<string, unknown>;

// jest.mock is hoisted above the imports, so the factory runs before this
// const is initialised: it forwards to the mock lazily, at call time.
const mockOpenAuthSessionAsync = jest.fn(
  async (_url: string, _redirect: string, _options: OpenOptions) => ({ type: "dismiss" }),
);
jest.mock("expo-web-browser", () => ({
  openAuthSessionAsync: (url: string, redirect: string, options: OpenOptions) =>
    mockOpenAuthSessionAsync(url, redirect, options),
}));

function lastCall(): [string, string, OpenOptions] {
  const calls = mockOpenAuthSessionAsync.mock.calls;
  return calls[calls.length - 1] as [string, string, OpenOptions];
}

describe("nativeBrowser.openAuthSession", () => {
  beforeEach(() => {
    mockOpenAuthSessionAsync.mockClear();
  });

  it("passes the URL and redirect through and keeps the session ephemeral on iOS", async () => {
    await nativeBrowser.openAuthSession(
      "https://idp.example.com/authorize?x=1",
      "com.example.app:/oauthredirect",
      { preferEphemeralSession: true },
    );
    expect(mockOpenAuthSessionAsync).toHaveBeenCalledTimes(1);
    const [url, redirect, options] = lastCall();
    expect(url).toBe("https://idp.example.com/authorize?x=1");
    expect(redirect).toBe("com.example.app:/oauthredirect");
    expect(options.preferEphemeralSession).toBe(true);
  });

  // Android: in a separate task (expo-web-browser's default) an abandoned tab
  // outlives the sign-in and the next one resurfaces it, so the user finishes
  // a stale request and gets "authorization state mismatch" on every retry.
  it("opens the Custom Tab in the app's own task on Android", async () => {
    await nativeBrowser.openAuthSession(
      "https://idp.example.com/authorize",
      "com.example.app:/oauthredirect",
      { preferEphemeralSession: true },
    );
    expect(lastCall()[2].createTask).toBe(false);
  });
});
