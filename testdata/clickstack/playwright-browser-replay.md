# Browser replay contract fixture

The pinned HyperDX `2.19.0` stack does not publish a stable browser/replay ingest contract suitable for a direct test. This harness therefore records `browser_replay_requires_pinned_sdk_playwright_runner` rather than guessing a protocol.

To enable the contract, add a Playwright fixture using the pinned `@hyperdx/browser` version from the exact HyperDX release. It must emit an exception and a session, then assert the version-pinned ClickHouse table and session/trace correlation. No proprietary source maps or application source are used.
