// The two auth screens, driven through the DOM.
//
// These render the SAME components both frontend kinds route to, with the
// router replaced by a recording `navigate` — which is exactly why the
// screens take navigation as a prop. No Next.js router and no TanStack router
// is mounted here, so one suite covers both scaffolds.
//
// The assertions derive from what the screens DID: which path they navigated
// to, how many times the exchange ran, what the user was shown.

import { cleanup, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import {
  AuthCallbackScreen,
  resetRedemptionsForTests,
  SignInScreen,
} from "./auth-screens";
import { OAuthError } from "./oidc";

// The screens call handleAuthCallback(); the flow it drives is covered by
// oidc.test.ts, so here it is a spy that records how it was called.
const handleAuthCallback = vi.hoisted(() => vi.fn());
vi.mock("./oidc-provider", () => ({
  handleAuthCallback,
  isOidcConfigured: () => true,
  CALLBACK_PATH: "/auth/callback",
  SIGN_IN_PATH: "/auth/sign-in",
}));

beforeEach(() => {
  handleAuthCallback.mockReset();
  // The screen caches redemptions per callback URL to survive StrictMode's
  // double-invoke (see auth-screens.tsx). These cases reuse the same fixture
  // query string, so without this reset a later case would reuse an earlier
  // one's cached promise and assert against an exchange that never ran.
  resetRedemptionsForTests();
});

afterEach(cleanup);

describe("SignInScreen", () => {
  it("redirects to the provider immediately, with nothing to click", async () => {
    // THE WHOLE POINT OF THIS SCREEN. A signed-out visitor is sent to the IdP
    // on mount — no interstitial button, because pressing it is the only
    // thing it could ever do. Hitting a gated URL signed out should land on
    // the provider's login form, the way every production app behaves.
    const login = vi.fn(async () => undefined);
    render(
      <SignInScreen login={login} isAuthenticated={false} navigate={vi.fn()} />,
    );

    await waitFor(() => expect(login).toHaveBeenCalledTimes(1));

    // No credential form either: forge's backend never sees a password, so a
    // field here could only POST somewhere that does not exist.
    expect(document.querySelector('input[type="password"]')).toBeNull();
    expect(document.querySelector("input")).toBeNull();
    // And no button competing with the redirect that is already in flight.
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("fires the redirect once, not twice under StrictMode double-mount", async () => {
    // A second login() would overwrite the first one's PKCE verifier
    // mid-flight, and the callback would then fail to redeem its own code.
    const login = vi.fn(async () => undefined);
    const { rerender } = render(
      <SignInScreen login={login} isAuthenticated={false} navigate={vi.fn()} />,
    );
    rerender(
      <SignInScreen login={login} isAuthenticated={false} navigate={vi.fn()} />,
    );

    await waitFor(() => expect(login).toHaveBeenCalledTimes(1));
  });

  it("sends an already-signed-in visitor onward instead of asking again", async () => {
    const navigate = vi.fn();
    render(
      <SignInScreen
        login={vi.fn()}
        isAuthenticated
        navigate={navigate}
        returnTo="/items"
      />,
    );
    await waitFor(() => expect(navigate).toHaveBeenCalledWith("/items"));
  });

  it("shows a pre-redirect failure instead of swallowing it", async () => {
    // No IdP configured, no WebCrypto, or storage blocked — all three are
    // actionable, and all three surface here rather than as a dead button.
    const login = vi.fn(async () => {
      throw new Error("crypto.subtle is unavailable");
    });
    render(
      <SignInScreen login={login} isAuthenticated={false} navigate={vi.fn()} />,
    );

    // The redirect fires on its own; when it throws, the screen stops being
    // a spinner and says what went wrong. A silent failure here would be a
    // permanently blank page.
    await waitFor(() => {
      expect(screen.getByRole("alert").textContent).toContain(
        "crypto.subtle is unavailable",
      );
    });
    // And offers a way to retry, since the cause may be transient.
    expect(screen.getByRole("button", { name: /try again/i })).toBeTruthy();
  });
});

describe("AuthCallbackScreen", () => {
  it("completes the exchange and navigates to the recorded destination", async () => {
    handleAuthCallback.mockResolvedValue("/items/42");
    const navigate = vi.fn();

    render(
      <AuthCallbackScreen search="?code=abc&state=xyz" navigate={navigate} />,
    );

    await waitFor(() => expect(navigate).toHaveBeenCalledWith("/items/42"));
    expect(handleAuthCallback).toHaveBeenCalledWith("?code=abc&state=xyz");
  });

  it("redeems the single-use code EXACTLY once under StrictMode", async () => {
    // React 18 StrictMode double-invokes effects in development. An
    // authorization code is single-use, so a second run would redeem a spent
    // code and show `invalid_grant` on a login that actually succeeded.
    handleAuthCallback.mockResolvedValue("/");
    const navigate = vi.fn();

    const { StrictMode } = await import("react");
    render(
      <StrictMode>
        <AuthCallbackScreen search="?code=abc&state=xyz" navigate={navigate} />
      </StrictMode>,
    );

    await waitFor(() => expect(navigate).toHaveBeenCalled());
    expect(handleAuthCallback).toHaveBeenCalledTimes(1);
  });

  it("reports an IdP rejection with the provider's own description", async () => {
    handleAuthCallback.mockRejectedValue(
      new OAuthError({
        code: "invalid_scope",
        description: "scope offline_access is not enabled",
      }),
    );
    render(
      <AuthCallbackScreen search="?error=invalid_scope" navigate={vi.fn()} />,
    );

    await waitFor(() => {
      expect(screen.getByRole("alert").textContent).toContain(
        "scope offline_access is not enabled",
      );
    });
  });

  it("words a user-cancelled sign-in as a cancellation, not a failure", async () => {
    // access_denied is the user saying no. Showing them a red error for a
    // choice they made is how a scaffold teaches people to ignore errors.
    handleAuthCallback.mockRejectedValue(
      new OAuthError({ code: "access_denied" }),
    );
    render(
      <AuthCallbackScreen search="?error=access_denied" navigate={vi.fn()} />,
    );

    await waitFor(() => {
      expect(screen.getByText(/cancelled/i)).toBeTruthy();
    });
  });

  it("reports a local failure (CSRF, no pending login) as an error", async () => {
    handleAuthCallback.mockRejectedValue(
      new Error(
        "auth: the state returned by the identity provider does not match",
      ),
    );
    const navigate = vi.fn();
    render(
      <AuthCallbackScreen search="?code=abc&state=wrong" navigate={navigate} />,
    );

    await waitFor(() => {
      expect(screen.getByRole("alert").textContent).toContain("does not match");
    });
    // And it does NOT navigate anywhere: a failed exchange must not look like
    // a successful sign-in.
    expect(navigate).not.toHaveBeenCalled();
  });

  it("shows a live region while the exchange is in flight", async () => {
    handleAuthCallback.mockReturnValue(new Promise(() => undefined));
    render(<AuthCallbackScreen search="?code=abc" navigate={vi.fn()} />);
    expect(screen.getByRole("status")).toBeTruthy();
  });
});
