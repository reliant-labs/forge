import fs from "node:fs";
import { join, resolve } from "node:path";

import react from "@vitejs/plugin-react";
import { defineConfig } from "vitest/config";

// Every package @reliantlabs/forge-web-runtime declares as a peer must resolve to
// ONE copy — this app's.
//
// peerDependencies states the contract ("the consuming app supplies this
// copy"), and npm honours it for a registry install by hoisting a single copy
// to the app root. It CANNOT honour it for the `file:` specifier a dev build
// of forge writes to bridge a local checkout: that is materialised as a
// symlink, the checkout behind it carries its own node_modules, and a bundler
// resolves a module's bare imports by walking up from that module's own
// location. So the runtime binds React and React Query out of the forge
// checkout while this app binds its own — two copies, each with its own
// module-scope state and its own React context.
//
// That breaks precisely the hooks: every generated `use<Rpc>` runs inside the
// runtime's service-hooks factory, so it would read a React Query context the
// test's own <QueryClientProvider> never wrote to —
// "TypeError: Cannot read properties of null (reading 'useContext')".
//
// Reading the list from the package rather than hard-coding it means a peer
// added to the runtime later is deduped here without anyone remembering to
// update this file. Same mechanism the vite-spa scaffold uses.
const webRuntimePeers: string[] = Object.keys(
  JSON.parse(
    fs.readFileSync(
      join(__dirname, "node_modules", "@reliantlabs", "forge-web-runtime", "package.json"),
      "utf8",
    ),
  ).peerDependencies ?? {},
);

export default defineConfig({
  plugins: [react()],
  resolve: {
    alias: {
      "@": resolve(__dirname, "src"),
    },
    // Resolve every runtime peer from this project root, whatever the
    // importer. A registry install already resolves that way, so this is a
    // no-op there and a fix only where the dev bridge is live. react-dom is
    // added explicitly: it is not a declared peer (the runtime never imports
    // it) but it must not be split from React.
    dedupe: [...webRuntimePeers, "react-dom"],
  },
  test: {
    environment: "jsdom",
    include: ["src/**/*.{test,spec}.{ts,tsx}"],
  },
});
