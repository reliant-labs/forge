---
title: "Forge Documentation"
description: "Production infrastructure generator for Go services and Next.js frontends — middleware, observability, deployment, and architectural guardrails from day one."
weight: 1
icon: "home"
---

# Welcome to Forge

**Forge** generates production-grade infrastructure for Go services and Next.js frontends. You define API contracts in proto and internal boundaries with Go interfaces — Forge produces the middleware stack, dependency wiring, test harness, CI/CD pipelines, Docker images, and Kubernetes manifests so you can focus on business logic.

## Core Philosophy

<div class="row row-cols-1 row-cols-md-3 g-4 mb-4">
<div class="col">

### Production Infrastructure
Middleware, observability, deployment pipelines, and container images from day one. Not just stubs — real infrastructure that works in production.

</div>
<div class="col">

### Two Contract Systems
Proto contracts for external APIs and config. Go interface contracts for internal package boundaries. Each boundary type uses the right tool for the job.

</div>
<div class="col">

### Architectural Guardrails
Explicit wiring in generated code, constructor injection for all dependencies, and contract enforcement that prevents accidental API surface leaks.

</div>
</div>

## Key Features

- **Middleware Stack**: Auth, logging, tracing, and recovery middleware wired automatically
- **Explicit Wiring**: All dependencies constructed in generated `pkg/app/wire.go` — no `init()`, no registries
- **Constructor Injection**: `Deps` structs with typed fields for every service and internal package
- **Connect RPC**: HTTP/1.1 + HTTP/2 + gRPC compatibility without proxies
- **Generated Test Harness**: `NewTestXxx` helpers with mock dependencies — ready for unit and integration tests
- **Contract Enforcement**: Linter ensures all exports respect proto or interface contracts
- **CI/CD Pipelines**: GitHub Actions workflows for test, build, and deploy
- **Kubernetes Deployment**: KCL manifests, Dockerfiles, and k3d local clusters out of the box

## Quick Start

```bash
# Install Forge CLI
go install github.com/reliant-labs/forge/cmd/forge@latest

# Create a new project
forge new myapp --mod github.com/company/myapp
cd myapp

# Generate infrastructure from your contracts
forge generate

# Run your application
forge run
```

## Documentation Sections

<div class="row row-cols-1 row-cols-md-2 g-4 mb-4">
<div class="col">

### [Getting Started]({{< relref "getting-started" >}})
Install Forge, create your first project, and understand the scaffold-then-own lifecycle.

</div>
<div class="col">

### [Architecture]({{< relref "architecture" >}})
Deep dive into infrastructure generation, dual contracts, explicit wiring, and schema evolution.

</div>
<div class="col">

### [Core Concepts]({{< relref "core-concepts" >}})
Understand Forge's layered architecture — API, infrastructure, data, and contract layers.

</div>
<div class="col">

### [Guides]({{< relref "guides" >}})
Step-by-step guides for creating services, internal packages, database integration, and testing.

</div>
<div class="col">

### [CLI Reference]({{< relref "reference/cli" >}})
Complete reference for all Forge CLI commands, flags, and examples.

</div>
<div class="col">

### [API Reference]({{< relref "api-reference" >}})
Detailed API documentation for Forge packages and interfaces.

</div>
</div>

## Why Forge?

Traditional scaffolding tools generate boilerplate and leave you to build everything else. Forge generates the infrastructure that makes production systems reliable:

- **Production middleware** instead of DIY plumbing
- **Explicit wiring** instead of hidden `init()` registration
- **Constructor injection** instead of global state
- **Generated test harness** instead of manual mock setup
- **CI/CD and deployment** instead of writing pipelines from scratch
- **Contract enforcement** instead of ad-hoc exports

The result: you write business logic and evolve your database schema. Forge handles everything around it.

---

**Ready to get started?** Head to [Getting Started]({{< relref "getting-started" >}}) to install Forge and create your first project.
