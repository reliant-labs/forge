// Scaffolded by forge. Yours to own — forge will not overwrite this file.
//
// Jest setup: route `expo/fetch` through the global fetch.
//
// src/lib/connect.ts hands connect-web the imported `expo/fetch` on purpose
// (streaming needs it; see that file). Under jest-expo that import resolves to
// a native-module stub whose requests never reach the network, so a test that
// stubs `globalThis.fetch` (or runs MSW) would be silently bypassed and the app
// would talk to nothing. This mapping makes the app's real transport observable
// to those stubs: the call is resolved against `globalThis.fetch` at CALL time,
// so a stub installed after import still wins.
//
// It is a test-environment mapping only; production code is unchanged. To prove
// the production transport really uses expo/fetch, opt out in that one file —
// see tests/connect-transport.test.ts (`jest.unmock` + its own mock).
import { jest } from "@jest/globals";

jest.mock("expo/fetch", () => ({
  fetch: (...args: Parameters<typeof globalThis.fetch>) => globalThis.fetch(...args),
}));
