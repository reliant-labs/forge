// Scaffolded by forge. Yours to own — forge will not overwrite this file.
//
// Drives the real <AuthContextProvider> (src/lib/auth/context) over the real
// OIDC provider, token store and identity-epoch session, with the device
// bindings (native.ts, the transport's token setters) replaced by fakes. It
// proves the wiring: restore on mount, sign-in, logout clearing the React
// Query cache and aborting what is in flight, switching accounts, and a dead
// refresh token signing the app out.
import { afterEach, beforeEach, describe, expect, it, jest } from "@jest/globals";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, useEffect, type ReactNode } from "react";
import { createRoot } from "test-renderer";

import { OidcConfigError } from "../src/lib/auth/config";
import { createOidcAuthProvider, type OidcProviderDeps } from "../src/lib/auth/oidc-provider";
import { currentSignal, session } from "../src/lib/auth/session";
import {
  StorageUnavailableError,
  createTokenStore,
  storageNamespace,
} from "../src/lib/auth/token-store";
import { AuthContextProvider } from "../src/lib/auth/context";
import { useAuth, type AuthContextValue } from "../src/lib/auth/use-auth";
import { EventBusProvider } from "../src/lib/event-context";
import { getEventBus } from "../src/lib/events";
import {
  CLIENT_ID,
  FakeBrowser,
  FakeIdp,
  FakeMarker,
  FakeSecureStore,
  ISSUER,
  POST_LOGOUT,
  REDIRECT,
  config,
  nodeCrypto,
  type BrowserScript,
} from "./auth-fakes";

const mockMake = jest.fn();
const mockSetTokenGetter = jest.fn();
const mockSetRejectedHandler = jest.fn();

jest.mock("../src/lib/auth/native", () => ({
  createNativeAuthProvider: (...args: unknown[]) => mockMake(...args),
}));
jest.mock("../src/lib/connect", () => ({
  setAuthTokenGetter: (...args: unknown[]) => mockSetTokenGetter(...args),
  setTokenRejectedHandler: (...args: unknown[]) => mockSetRejectedHandler(...args),
}));

(globalThis as { IS_REACT_ACT_ENVIRONMENT?: boolean }).IS_REACT_ACT_ENVIRONMENT = true;

const ENV = {
  EXPO_PUBLIC_FORGE_OIDC_ISSUER: ISSUER,
  EXPO_PUBLIC_FORGE_OIDC_CLIENT_ID: CLIENT_ID,
  EXPO_PUBLIC_FORGE_OIDC_REDIRECT_URI: REDIRECT,
  EXPO_PUBLIC_FORGE_OIDC_POST_LOGOUT_REDIRECT_URI: POST_LOGOUT,
};
const ENV_KEYS = Object.keys(ENV);

interface World {
  idp: FakeIdp;
  secureStore: FakeSecureStore;
  browser: FakeBrowser;
  store: ReturnType<typeof createTokenStore>;
  clock: { t: number };
  queryClient: QueryClient;
}

let world: World;
let auth: { current: AuthContextValue };
let committed: AuthContextValue[];
let unmount: (() => void) | null;

function makeWorld(): World {
  const idp = new FakeIdp();
  const secureStore = new FakeSecureStore();
  const store = createTokenStore({
    namespace: storageNamespace(config.issuer, config.clientId),
    secureStore,
    launchMarker: new FakeMarker(true),
  });
  return {
    idp,
    secureStore,
    store,
    browser: new FakeBrowser(idp),
    clock: { t: Date.now() },
    queryClient: new QueryClient(),
  };
}

// Records every value React COMMITS (an effect runs after commit, never for a
// discarded render), and the latest one for the assertions.
function Probe() {
  const value = useAuth();
  useEffect(() => {
    auth.current = value;
    committed.push(value);
  });
  return null;
}

function Tree({ children }: { children?: ReactNode }) {
  return (
    <QueryClientProvider client={world.queryClient}>
      <EventBusProvider>
        <AuthContextProvider>
          <Probe />
          {children}
        </AuthContextProvider>
      </EventBusProvider>
    </QueryClientProvider>
  );
}

async function mount() {
  const root = createRoot();
  unmount = () => root.unmount();
  await act(async () => {
    root.render(<Tree />);
  });
}

// A user who signed in on an earlier launch: tokens are in the keystore.
async function seedSavedSession(sub = "user-1") {
  world.idp.sub = sub;
  const provider = createOidcAuthProvider(config, providerDeps());
  await provider.signIn();
}

function providerDeps(overrides: Partial<OidcProviderDeps> = {}): OidcProviderDeps {
  return {
    fetch: world.idp.fetch,
    crypto: nodeCrypto,
    browser: world.browser,
    store: world.store,
    session,
    now: () => world.clock.t,
    discoveryCache: new Map(),
    ...overrides,
  };
}

const cached = () => world.queryClient.getQueryCache().getAll().length;
const seedCache = () => world.queryClient.setQueryData(["widgets", "list"], { items: [1, 2, 3] });

async function signInNow() {
  let user: Awaited<ReturnType<AuthContextValue["signIn"]>> = null;
  await act(async () => {
    user = await auth.current.signIn();
  });
  return user;
}

beforeEach(() => {
  world = makeWorld();
  auth = { current: undefined as unknown as AuthContextValue };
  committed = [];
  unmount = null;
  Object.assign(process.env, ENV);
  mockMake.mockReset();
  mockSetTokenGetter.mockReset();
  mockSetRejectedHandler.mockReset();
  mockMake.mockImplementation((cfg: unknown, overrides: unknown) =>
    createOidcAuthProvider(
      cfg as typeof config,
      providerDeps(overrides as Partial<OidcProviderDeps>),
    ),
  );
});

afterEach(() => {
  act(() => unmount?.());
  // A query left in the cache holds a gcTime timer that keeps Jest from exiting.
  world.queryClient.clear();
  for (const k of ENV_KEYS) delete process.env[k];
  delete process.env.EXPO_PUBLIC_MOCK_API;
  jest.restoreAllMocks();
});

describe("which provider the context chooses", () => {
  it("without EXPO_PUBLIC_FORGE_OIDC_* keeps the mock: no sign-in required, no token wiring", async () => {
    for (const k of ENV_KEYS) delete process.env[k];
    process.env.EXPO_PUBLIC_MOCK_API = "true";
    await mount();
    expect(auth.current.requiresSignIn).toBe(false);
    expect(auth.current.configError).toBeNull();
    expect(auth.current.isLoading).toBe(false);
    expect(auth.current.isAuthenticated).toBe(true); // the fixture user, exactly as before
    expect(auth.current.identity.email).toBe("dev@localhost");
    expect(mockMake).not.toHaveBeenCalled();
    expect(mockSetTokenGetter).not.toHaveBeenCalled();
  });

  it("with an INVALID configuration surfaces the error and does not fall back to the mock", async () => {
    process.env.EXPO_PUBLIC_FORGE_OIDC_ISSUER = "http://idp.example.com"; // plain http, not loopback
    const error = jest.spyOn(console, "error").mockImplementation(() => {});
    await mount();
    expect(auth.current.requiresSignIn).toBe(true);
    expect(auth.current.configError).toBeInstanceOf(OidcConfigError);
    expect(auth.current.configError?.message).toContain("EXPO_PUBLIC_FORGE_OIDC_ISSUER");
    expect(auth.current.isAuthenticated).toBe(false);
    expect(auth.current.isLoading).toBe(false);
    expect(error).toHaveBeenCalled();
    await expect(auth.current.signIn()).rejects.toBeInstanceOf(OidcConfigError);
    expect(mockMake).not.toHaveBeenCalled();
  });

  it("with a valid configuration builds the OIDC provider and wires the transport to it", async () => {
    await mount();
    expect(auth.current.requiresSignIn).toBe(true);
    expect(mockMake).toHaveBeenCalledTimes(1);
    expect(mockMake.mock.calls[0][0]).toMatchObject({ issuer: ISSUER, clientId: CLIENT_ID });
    expect(mockSetTokenGetter).toHaveBeenCalledTimes(1);
    expect(mockSetRejectedHandler).toHaveBeenCalledWith(expect.any(Function));
  });

  it("unwires the transport when it unmounts", async () => {
    await mount();
    mockSetTokenGetter.mockClear();
    mockSetRejectedHandler.mockClear();
    act(() => unmount?.());
    unmount = null;
    expect(mockSetRejectedHandler).toHaveBeenLastCalledWith(null);
    const getter = mockSetTokenGetter.mock.calls.at(-1)?.[0] as () => Promise<string | null>;
    await expect(getter()).resolves.toBeNull();
  });
});

describe("restore on mount", () => {
  it("is loading while the saved session is read, then signed in as the saved user", async () => {
    await seedSavedSession("user-1");
    await mount();
    // The first committed value is the loading state: a signed-in user must
    // never be shown the sign-in screen while the keystore is being read.
    expect(committed[0].isLoading).toBe(true);
    expect(committed[0].isAuthenticated).toBe(false);
    expect(committed.filter((v) => !v.isLoading && !v.isAuthenticated)).toHaveLength(0);
    expect(auth.current.isLoading).toBe(false);
    expect(auth.current.isAuthenticated).toBe(true);
    expect(auth.current.identity).toMatchObject({ subject: "user-1", email: "user-1@example.com" });
  });

  it("signs the user in with a token the transport can read", async () => {
    await seedSavedSession("user-1");
    await mount();
    const getter = mockSetTokenGetter.mock.calls.at(-1)?.[0] as () => Promise<string | null>;
    await expect(getter()).resolves.toBe("access-1");
  });

  it("with nothing saved is signed out and finished loading", async () => {
    await mount();
    expect(auth.current.isLoading).toBe(false);
    expect(auth.current.isAuthenticated).toBe(false);
  });

  it("with a lapsed access token renews it before reporting signed in", async () => {
    await seedSavedSession("user-1");
    world.clock.t += 3600 * 1000;
    await mount();
    expect(auth.current.isAuthenticated).toBe(true);
    const getter = mockSetTokenGetter.mock.calls.at(-1)?.[0] as () => Promise<string | null>;
    await expect(getter()).resolves.toBe("access-2");
  });

  it("offline at launch stays signed in with the saved session", async () => {
    await seedSavedSession("user-1");
    world.clock.t += 3600 * 1000;
    world.idp.offline = true;
    await mount();
    expect(auth.current.isLoading).toBe(false);
    expect(auth.current.isAuthenticated).toBe(true);
  });

  it("with unusable secure storage is signed out, not stuck loading, and signIn names the problem", async () => {
    world.secureStore.available = false;
    const warn = jest.spyOn(console, "warn").mockImplementation(() => {});
    await mount();
    expect(auth.current.isLoading).toBe(false);
    expect(auth.current.isAuthenticated).toBe(false);
    expect(warn).toHaveBeenCalled();
    let error: unknown;
    await act(async () => {
      error = await auth.current.signIn().catch((e: unknown) => e);
    });
    expect(error).toBeInstanceOf(StorageUnavailableError);
  });

  it("with a dead saved grant signs out and clears the cache", async () => {
    await seedSavedSession("user-1");
    world.idp.validRefresh.clear();
    world.clock.t += 3600 * 1000;
    seedCache();
    await mount();
    expect(auth.current.isAuthenticated).toBe(false);
    expect(auth.current.isLoading).toBe(false);
    expect(cached()).toBe(0);
  });
});

describe("signIn", () => {
  it("signs in through the system browser and exposes the user", async () => {
    await mount();
    const user = await signInNow();
    expect(user).toMatchObject({ id: "user-1" });
    expect(auth.current.isAuthenticated).toBe(true);
    expect(auth.current.identity.subject).toBe("user-1");
    expect(world.browser.opened).toHaveLength(1);
    const getter = mockSetTokenGetter.mock.calls.at(-1)?.[0] as () => Promise<string | null>;
    await expect(getter()).resolves.toBe("access-1");
  });

  it("resolves null and stays signed out when the user cancels", async () => {
    const cancel: BrowserScript = () => ({ type: "cancel" });
    world.browser.script = cancel;
    await mount();
    expect(await signInNow()).toBeNull();
    expect(auth.current.isAuthenticated).toBe(false);
  });

  it("rejects with a typed error when the device is offline", async () => {
    await mount();
    world.idp.offline = true;
    let error: unknown;
    await act(async () => {
      error = await auth.current.signIn().catch((e: unknown) => e);
    });
    expect(error).toMatchObject({ name: "OfflineError" });
    expect(auth.current.isAuthenticated).toBe(false);
  });
});

describe("logout", () => {
  it("clears the React Query cache, aborts in-flight work, revokes and wipes the session", async () => {
    await mount();
    await signInNow();
    seedCache();
    expect(cached()).toBe(1);
    const inFlight = currentSignal();
    expect(inFlight.aborted).toBe(false);

    await act(async () => {
      await auth.current.logout();
    });

    expect(auth.current.isAuthenticated).toBe(false);
    expect(inFlight.aborted).toBe(true);
    expect(currentSignal().aborted).toBe(false); // a fresh epoch for whoever signs in next
    expect(cached()).toBe(0);
    expect(await world.store.load()).toBeNull();
    expect(world.idp.revoked).toEqual(["refresh-1"]);
    const getter = mockSetTokenGetter.mock.calls.at(-1)?.[0] as () => Promise<string | null>;
    await expect(getter()).resolves.toBeNull();
  });

  it("makes the next sign-in ask for credentials again (prompt=login)", async () => {
    await mount();
    await signInNow();
    await act(async () => {
      await auth.current.logout();
    });
    await signInNow();
    expect(world.browser.opened[1].url.searchParams.get("prompt")).toBe("login");
  });

  it("still signs out locally when the IdP is unreachable", async () => {
    await mount();
    await signInNow();
    world.idp.offline = true;
    await act(async () => {
      await auth.current.logout();
    });
    expect(auth.current.isAuthenticated).toBe(false);
    expect(await world.store.load()).toBeNull();
  });
});

describe("identity change", () => {
  it("a different account signing in ends the previous session: cache cleared, old requests aborted", async () => {
    await mount();
    await signInNow();
    expect(auth.current.identity.subject).toBe("user-1");
    seedCache();
    const oldSignal = currentSignal();

    world.idp.sub = "user-2";
    await signInNow();

    expect(auth.current.identity.subject).toBe("user-2");
    expect(auth.current.isAuthenticated).toBe(true);
    expect(oldSignal.aborted).toBe(true);
    expect(cached()).toBe(0);
  });

  it("the same account signing in again keeps the cache", async () => {
    await mount();
    await signInNow();
    seedCache();
    await signInNow();
    expect(auth.current.identity.subject).toBe("user-1");
    expect(cached()).toBe(1);
  });
});

describe("a session the IdP no longer honours (invalid_grant)", () => {
  async function expireGrant() {
    world.idp.validRefresh.clear();
    world.clock.t += 3600 * 1000;
  }

  it("signs out and clears the cache when a request needs a token", async () => {
    await mount();
    await signInNow();
    seedCache();
    const oldSignal = currentSignal();
    await expireGrant();

    const getter = mockSetTokenGetter.mock.calls.at(-1)?.[0] as () => Promise<string | null>;
    await act(async () => {
      await expect(getter()).resolves.toBeNull();
    });

    expect(auth.current.isAuthenticated).toBe(false);
    expect(oldSignal.aborted).toBe(true);
    expect(cached()).toBe(0);
    expect(await world.store.load()).toBeNull();
  });

  it("signs out when the transport reports auth:expired and the renewal is refused", async () => {
    await mount();
    await signInNow();
    seedCache();
    await expireGrant();

    await act(async () => {
      getEventBus().emit("auth:expired");
    });

    expect(auth.current.isAuthenticated).toBe(false);
    expect(cached()).toBe(0);
  });

  it("keeps the session on auth:expired when the renewal works", async () => {
    await mount();
    await signInNow();
    world.clock.t += 3600 * 1000;

    await act(async () => {
      getEventBus().emit("auth:expired");
    });

    expect(auth.current.isAuthenticated).toBe(true);
  });

  it("a 401 handler renews once, then the next sign-in works after the grant died", async () => {
    await mount();
    await signInNow();
    const onRejected = mockSetRejectedHandler.mock.calls.at(-1)?.[0] as (
      token: string | null,
    ) => Promise<boolean>;
    await act(async () => {
      await expect(onRejected("access-1")).resolves.toBe(true);
    });
    await expect(
      (mockSetTokenGetter.mock.calls.at(-1)?.[0] as () => Promise<string | null>)(),
    ).resolves.toBe("access-2");
  });
});
