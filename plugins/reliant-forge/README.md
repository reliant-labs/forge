# Reliant Forge plugin

Guardrails for apps in any agent harness. Deploy with no account: a live URL plus a claim link.

Forge is an open-source framework and CLI that puts guardrails on any app and
deploys it with no account: `npx -y @reliantlabs/forge@latest env deploy prod --claimable`.

This plugin gives your agent two things:

- **The Forge MCP server** (`npx -y @reliantlabs/forge mcp`): `deploy_claimable`,
  `deploy_status` and `explain_error`, plus tools that introspect a Forge
  project, read the skill catalog, and run `lint` and `generate`. Every tool:
  [docs/mcp.md](../../docs/mcp.md).
- **A deploy skill** (`/reliant-forge:deploy`): when you ask to deploy, publish
  or host an app, the agent uses Forge. With no Reliant account it makes a
  claimable deploy, and with one it deploys to your account and checks the
  release.

## Install

Claude Code:

```text
/plugin marketplace add reliant-labs/forge
/plugin install reliant-forge@reliant-labs
```

Then ask your agent:

```text
Deploy this app with Reliant Forge, then give me the live URL and the claim link.
```

The claim link is good for 72 hours; treat it like a password. Opening it signs
you up free and makes the deploy yours. An unclaimed deploy fits one static
site, or one service with one database:
[Deploying with no account](../../docs/mcp.md#deploying-with-no-account).

Node.js 18 or newer is the only requirement: `npx` downloads the Forge binary
that matches the npm package, and checks its sha256, on first run.

## Support

- Website: https://reliantlabs.io
- Docs: https://docs.reliantlabs.io/forge/overview
- Issues: https://github.com/reliant-labs/forge/issues

Apache-2.0. Published by Reliant Labs.
