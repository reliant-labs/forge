---
title: "CLI Reference"
description: "Complete reference for all Forge CLI commands"
weight: 50
icon: "terminal"
---

# CLI Reference

See the [full CLI Reference]({{< relref "../reference/cli" >}}) for complete documentation of all Forge commands, flags, and examples.

## Quick Reference

| Command | Description |
|---------|-------------|
| `forge new` | Create a new project |
| `forge add service` | Add a Go service |
| `forge add frontend` | Add a Next.js frontend |
| `forge package new` | Create an internal package with `contract.go` |
| `forge generate` | Generate code from protos and contracts |
| `forge build` | Build binaries and Docker images |
| `forge run` | Start the dev environment |
| `forge deploy` | Deploy to Kubernetes |
| `forge test` | Run tests |
| `forge lint` | Run linters (use `--contract` for contract enforcement) |
| `forge db` | Database migration management |

## Generated Binary Commands

| Command | Description |
|---------|-------------|
| `<binary> server [services...]` | Start the HTTP server |
| `<binary> version` | Print build info |
