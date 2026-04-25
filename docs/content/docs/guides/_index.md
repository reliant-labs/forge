---
title: "Guides"
description: "Step-by-step guides for common Forge tasks"
weight: 30
icon: "menu_book"
---

# Guides

Step-by-step guides to help you build and deploy Forge applications.

## Getting Started

<div class="row row-cols-1 row-cols-md-2 g-4 mb-4">
<div class="col">

### [Creating Your First Service]({{< relref "creating-services" >}})
Learn how to define a service in proto, generate code, and implement business logic.

</div>
<div class="col">

### [Service Communication]({{< relref "service-communication" >}})
Understand how services communicate using Connect RPC and constructor injection.

</div>
</div>

## Advanced Topics

<div class="row row-cols-1 row-cols-md-2 g-4 mb-4">
<div class="col">

### [Creating Custom Middleware]({{< relref "creating-middleware" >}})
Build custom Connect RPC interceptors for authentication, logging, metrics, and more.

</div>
<div class="col">

### [Database Integration]({{< relref "database-integration" >}})
Migration-first database workflow, entity types, ORM functions, sqlc queries, and transactions.

</div>
<div class="col">

### [Testing Strategies]({{< relref "testing-strategies" >}})
Unit testing with the generated test harness, integration testing, and end-to-end testing.

</div>
<div class="col">

### [LLM Integration]({{< relref "llm-integration" >}})
Use the MCP server to enable safe AI-assisted development.

</div>
</div>

## Best Practices

<div class="row row-cols-1 row-cols-md-2 g-4 mb-4">
<div class="col">

### [Proto Design Patterns]({{< relref "proto-patterns" >}})
Best practices for designing proto APIs, versioning, and backward compatibility.

</div>
<div class="col">

### [Service Patterns]({{< relref "service-patterns" >}})
Common patterns for constructor injection, error handling, and transactions.

</div>
<div class="col">

### [Performance Optimization]({{< relref "performance" >}})
Tips and techniques for optimizing Forge applications.

</div>
<div class="col">

### [Production Deployment]({{< relref "deployment" >}})
Deploy Forge applications to production with Docker, Kubernetes, and more.

</div>
<div class="col">

### [KCL Deployment Guide]({{< relref "kcl" >}})
Kubernetes manifest generation with typed schemas, multi-environment configuration, and CLI overrides.

</div>
<div class="col">

### [CI/CD Pipelines]({{< relref "ci-cd" >}})
Generated GitHub Actions workflows, image promotion flow, and local development with k3d.

</div>
</div>

## Need Help?

- **[Troubleshooting]({{< relref "../troubleshooting" >}})** - Common issues and solutions
- **[FAQ]({{< relref "../faq" >}})** - Frequently asked questions
- **[Examples](https://github.com/reliant-labs/forge/tree/main/examples)** - Complete working examples

---

Can't find what you're looking for? [Open an issue](https://github.com/reliant-labs/forge/issues) or [start a discussion](https://github.com/reliant-labs/forge/discussions).