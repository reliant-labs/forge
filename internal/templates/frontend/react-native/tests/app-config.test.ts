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
    expect(app.expo.scheme).toMatch(/^[a-z][a-z0-9+.-]*$/i);
    expect(app.expo.ios.bundleIdentifier).toMatch(/^[a-z0-9-]+(\.[a-z0-9-]+)+$/i);
    expect(app.expo.android.package).toMatch(/^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)+$/i);
  });
});
