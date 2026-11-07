// Scaffolded by forge. Yours to own — forge will not overwrite this file.
import { describe, expect, it } from "@jest/globals";
import app from "../app.json";
import manifest from "../package.json";

// This is a configuration smoke test, not proof of device/auth compatibility.
// Keep these assertions aligned when changing the app identity or router.
describe("Expo app configuration", () => {
  it("registers the Router entry and plugin together", () => {
    expect(manifest.main).toBe("expo-router/entry");
    expect(app.expo.plugins).toContain("expo-router");
  });

  it("declares native identifiers and a valid deep-link scheme", () => {
    const schemes = [app.expo.scheme].flat();
    // ONE scheme: a second entry makes expo-linking warn on every launch, and
    // prebuild already registers the iOS bundle id as a URL scheme. The
    // reverse-DNS id (not the bare app name) also cannot collide with another
    // app's deep links.
    expect(typeof app.expo.scheme).toBe("string");
    expect(schemes).toHaveLength(1);
    // RFC 3986 schemes are lowercase-by-convention, start with a letter, and have no `_`.
    for (const scheme of schemes) expect(scheme).toMatch(/^[a-z][a-z0-9+.-]*$/);
    // The native sign-in redirect is `<app id>:/oauthredirect`; the OS only
    // routes it back to the app when the app id is a registered scheme.
    expect(schemes).toContain(app.expo.ios.bundleIdentifier);
    expect(schemes).toContain(app.expo.android.package);
    // One id serves as URI scheme, iOS bundle id and Android package, so it is
    // the intersection of the three grammars (lowercase, letter-led segments, no `-`/`_`).
    expect(app.expo.ios.bundleIdentifier).toBe(app.expo.android.package);
    expect(app.expo.ios.bundleIdentifier).toMatch(/^[a-z][a-z0-9]*(\.[a-z][a-z0-9]*)+$/);
    expect(app.expo.android.package).toMatch(/^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$/);
  });
});
