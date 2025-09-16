---
title: "Forge Documentation"
description: "Proto-first application scaffolding framework for building production-ready Go services and Next.js frontends with Connect RPC, explicit wiring, and contract enforcement."
weight: 1
icon: "home"
---

# Welcome to Forge

**Forge** is a proto-first application scaffolding framework for building production-ready Go services and Next.js frontends. It generates complete projects with Connect RPC, explicit dependency wiring, contract enforcement, and CI/CD pipelines.

## Core Philosophy

<div class="row row-cols-1 row-cols-md-3 g-4 mb-4">
<div class="col">

### Two Contract Systems
Proto contracts for external boundaries (APIs, config). Go interface contracts for internal boundaries (packages). Each boundary type uses the right tool for the job.

</div>
<div class="col">

### Single Binary Design
One Go binary per project with a Cobra CLI. All services share one process, one HTTP mux, one middleware stack. Dependencies wired explicitly in generated code.

</div>
<div class="col">

### Contract Enforcement
The `forge lint --contract` linter ensures all exported methods match their contract — whether proto RPCs or Go interfaces. No accidental API leaks.

</div>
</div>

## Key Features

- **Explicit Wiring**: All dependencies constructed in generated `pkg/app/wire.go` — no `init()`, no registries
- **Constructor Injection**: `Deps` structs with typed fields for every service and internal package
- **Connect RPC**: HTTP/1.1 + HTTP/2 + gRPC compatibility without proxies
- **Code Generation**: Automatic generation from proto files and Go interfaces for stubs, mocks, middleware, and wiring
- **Contract Enforcement**: Linter ensures all exports respect proto or interface contracts
- **Test Harness**: Generated `NewTestXxx` helpers for testing with mock dependencies
- **Internal Packages**: `contract.go` with Go interfaces, generated mocks and middleware wrappers

## Quick Start

```bash
# Install Forge CLI
go install github.com/reliant-labs/forge/cmd/forge@latest

# Create a new project
forge new myapp --mod github.com/company/myapp
cd myapp

# Generate code from proto files
forge generate

# Run your application
forge run
```

## Documentation Sections

<div class="row row-cols-1 row-cols-md-2 g-4 mb-4">
<div class="col">

### [Getting Started]({{< relref "getting-started" >}})
Learn how to install Forge, create your first project, and understand the basic workflow.

</div>
<div class="col">

### [Architecture]({{< relref "architecture" >}})
Deep dive into single binary design, two contract systems, explicit wiring, and constructor injection.

</div>
<div class="col">

### [Core Concepts]({{< relref "core-concepts" >}})
Understand proto-first development, Go interface contracts, service patterns, and architectural principles.

</div>
<div class="col">

### [Guides]({{< relref "guides" >}})
Step-by-step guides for common tasks like creating services, internal packages, database integration, and testing.

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

Traditional scaffolding tools generate boilerplate without enforcing architecture. Forge replaces this with:

- **Contract enforcement** instead of ad-hoc exports
- **Explicit wiring** instead of hidden `init()` registration
- **Constructor injection** instead of global state
- **Generated test harness** instead of manual mock setup

This results in more maintainable, consistent, and AI-friendly codebases.

---

**Ready to get started?** Head to [Getting Started]({{< relref "getting-started" >}}) to install Forge and create your first project.
