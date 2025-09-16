---
title: "KCL Deployment Guide"
description: "Kubernetes manifest generation with KCL schemas and multi-environment configuration"
weight: 50
---

# KCL Deployment Guide

Forge uses [KCL](https://kcl-lang.io) instead of Helm or Kustomize for Kubernetes manifest generation. KCL is a configuration language with a type system — it catches errors at render time rather than at `kubectl apply` time, and the schema definitions serve as documentation for every configurable field.

When you run `forge deploy <env>`, KCL renders your application definitions into Kubernetes YAML, which is then piped to `kubectl apply --server-side`. The entire deploy pipeline is: **KCL render → kubectl apply → wait for rollout**.

## File Layout

The KCL files live under `deploy/kcl/` with this structure:

```
deploy/kcl/
├── schema.k              # Type definitions (Application, Environment, etc.)
├── render.k              # Private lambdas that produce K8s manifests
├── base.k                # Reusable env var groups and init containers
├── dev/
│   └── main.k            # Dev environment definition
├── staging/
│   └── main.k            # Staging environment definition
└── prod/
    └── main.k            # Production environment definition
```

`schema.k`, `render.k`, and `base.k` are shared across all environments. Each environment has its own `main.k` that composes applications with environment-specific settings.

## The Application Schema

The `Application` schema in `schema.k` defines everything about a deployed service. Here is the complete reference with default values:

```python
schema Application:
    name: str                                          # Required: service name
    image: str                                         # Required: Docker image name
    replicas: int = 1                                  # Pod replica count
    ports: [ServicePort] = [ServicePort {}]            # Port mappings (default: 80 → 8080)
    resources: Resources = Resources {}                # CPU/memory requests and limits
    env_vars: [EnvVar] = []                            # Environment variables
    health_check: HealthCheck = HealthCheck {}          # Liveness/readiness probes
    security_context: SecurityContext = SecurityContext {} # Pod security settings
    ingress: IngressConfig = IngressConfig {}           # Ingress with TLS
    autoscaling: AutoScaling = AutoScaling {}           # HPA configuration
    pdb: PodDisruptionBudget = PodDisruptionBudget {}  # Disruption budget
    init_containers: [InitContainer] = []              # Init containers
    labels: {str: str} = {}                            # Additional labels
    annotations: {str: str} = {}                       # Annotations
    service_account_name: str = ""                     # Custom service account
    command: [str] = []                                # Override container command
    args: [str] = []                                   # Override container args
```

### Supporting Schemas

**Resources** — CPU and memory requests/limits:

```python
schema Resources:
    cpu_request: str = "100m"
    cpu_limit: str = "500m"
    memory_request: str = "128Mi"
    memory_limit: str = "512Mi"
```

**ServicePort** — port mapping from Service to container:

```python
schema ServicePort:
    name: str = "http"
    port: int = 80              # Service port
    target_port: int = 8080     # Container port
    protocol: str = "TCP"       # TCP, UDP, or SCTP
```

**HealthCheck** — HTTP health probe configuration:

```python
schema HealthCheck:
    http_path: str = "/healthz"
    http_port: int = 8080
    initial_delay: int = 5      # Seconds before first probe
    period: int = 10            # Seconds between probes
    timeout: int = 3
    failure_threshold: int = 3
```

**SecurityContext** — pod and container security settings. The defaults follow the Kubernetes restricted pod security standard:

```python
schema SecurityContext:
    run_as_non_root: bool = True
    run_as_user: int = 65532
    run_as_group: int = 65532
    read_only_root_filesystem: bool = True
    allow_privilege_escalation: bool = False
    drop_capabilities: [str] = ["ALL"]
    seccomp_profile: str = "RuntimeDefault"
```

**EnvVar** — environment variable with optional secret reference:

```python
schema EnvVar:
    name: str
    value: str = ""             # Plain-text value
    secret_ref: str = ""        # Kubernetes Secret name
    secret_key: str = ""        # Key within the Secret
```

An `EnvVar` must have either `value` or `secret_ref` set. When `secret_ref` is set and `secret_key` is empty, the secret key defaults to the env var `name`.

**AutoScaling** — Horizontal Pod Autoscaler:

```python
schema AutoScaling:
    enabled: bool = False
    min_replicas: int = 1
    max_replicas: int = 10
    cpu_target: int = 80        # Target CPU utilization percentage
    memory_target: int = 80
```

**PodDisruptionBudget**:

```python
schema PodDisruptionBudget:
    enabled: bool = False
    min_available: int | None = 1
    max_unavailable: int | None = None
```

**IngressConfig** — with TLS and cert-manager support:

```python
schema IngressConfig:
    enabled: bool = False
    host: str = ""                                      # Required when enabled
    tls: bool = True
    cert_manager_issuer: str = "letsencrypt-prod"
    paths: [{str: str}] = [{"path": "/", "path_type": "Prefix"}]
```

**InitContainer**:

```python
schema InitContainer:
    name: str
    image: str
    command: [str] = []
    env: [EnvVar] = []
```

## The Environment Schema

The `Environment` schema groups multiple applications under an environment namespace:

```python
schema Environment:
    name: str                          # Environment name (dev, staging, prod)
    namespace: str                     # Kubernetes namespace
    image_registry: str = ""           # Registry prefix for images
    image_tag: str = "latest"          # Default image tag
    applications: {str: Application}   # Map of app name → Application
    network_policies: bool = True      # Generate default NetworkPolicies
    resource_quotas: bool = False      # Generate ResourceQuotas
```

## Writing Environment Configs

Each environment's `main.k` imports the schemas, defines applications, and calls the render function. Here's a typical dev environment:

```python
# deploy/kcl/dev/main.k
import ..schema
import ..render
import ..base

# CLI overrides (passed via -D flags)
image_tag: str = option("image_tag", default="latest")
namespace: str = option("namespace", default="myproject-dev")

env = schema.Environment {
    name = "dev"
    namespace = namespace
    image_registry = "localhost:5050"
    image_tag = image_tag
    applications = {
        "api" = schema.Application {
            name = "api"
            image = "api"
            replicas = 1
            ports = [schema.ServicePort {port = 80, target_port = 8080}]
            env_vars = base.COMMON_ENV + base.DB_ENV + [
                schema.EnvVar {name = "LOG_LEVEL", value = "debug"}
            ]
            resources = schema.Resources {
                cpu_request = "50m"
                memory_request = "64Mi"
                cpu_limit = "200m"
                memory_limit = "256Mi"
            }
        }
        "users" = schema.Application {
            name = "users"
            image = "users"
            replicas = 1
            ports = [schema.ServicePort {port = 80, target_port = 8081}]
            env_vars = base.COMMON_ENV + base.DB_ENV
        }
    }
    network_policies = False  # Disable for local development
}

# Render all Kubernetes manifests
items = render.render_environment(env)
```

A production environment adds autoscaling, PDBs, ingress, and init containers for migrations:

```python
# deploy/kcl/prod/main.k
import ..schema
import ..render
import ..base

image_tag: str = option("image_tag", default="latest")
namespace: str = option("namespace", default="myproject-prod")

env = schema.Environment {
    name = "prod"
    namespace = namespace
    image_registry = "ghcr.io/myorg"
    image_tag = image_tag
    applications = {
        "api" = schema.Application {
            name = "api"
            image = "api"
            replicas = 3
            ports = [schema.ServicePort {port = 80, target_port = 8080}]
            env_vars = base.COMMON_ENV + base.DB_ENV + base.OTEL_ENV + [
                schema.EnvVar {name = "LOG_LEVEL", value = "info"}
            ]
            resources = schema.Resources {
                cpu_request = "250m"
                memory_request = "256Mi"
                cpu_limit = "1000m"
                memory_limit = "1Gi"
            }
            autoscaling = schema.AutoScaling {
                enabled = True
                min_replicas = 3
                max_replicas = 10
                cpu_target = 70
            }
            pdb = schema.PodDisruptionBudget {
                enabled = True
                min_available = 2
            }
            ingress = schema.IngressConfig {
                enabled = True
                host = "api.myproject.com"
                tls = True
            }
            init_containers = [
                base.migration_init_container("ghcr.io/myorg/api-migrate:${image_tag}", base.DB_ENV)
            ]
        }
    }
    network_policies = True
}

items = render.render_environment(env)
```

## What Gets Rendered

The `render.render_environment()` function produces the following Kubernetes resources for each application:

| Resource | Always | Conditional |
|----------|--------|-------------|
| Namespace | Yes | — |
| Deployment | Yes | — |
| Service | Yes | — |
| ServiceAccount | Yes | — |
| Role + RoleBinding | Yes | — |
| Ingress | — | `ingress.enabled = True` |
| HorizontalPodAutoscaler | — | `autoscaling.enabled = True` |
| PodDisruptionBudget | — | `pdb.enabled = True` |
| NetworkPolicies (default-deny, allow-dns, allow-internal, allow-ingress) | — | `environment.network_policies = True` |

Every resource gets standard labels (`app.kubernetes.io/name`, `app.kubernetes.io/managed-by: forge`) and the `restricted` pod security standard is enforced at the namespace level.

## Base Configurations

The `base.k` file provides reusable environment variable groups that you compose into applications:

```python
base.DB_ENV        # DATABASE_URL, DB_HOST, DB_PORT, DB_NAME, DB_USER, DB_PASSWORD (all from secrets)
base.REDIS_ENV     # REDIS_URL (from secret)
base.OTEL_ENV      # OTEL_EXPORTER_OTLP_ENDPOINT, OTEL_SERVICE_NAME, OTEL_TRACES_SAMPLER, etc.
base.COMMON_ENV    # LOG_LEVEL, SERVICE_NAME
```

Compose them with `+`:

```python
env_vars = base.COMMON_ENV + base.DB_ENV + base.REDIS_ENV + [
    schema.EnvVar {name = "CUSTOM_VAR", value = "custom-value"}
]
```

The `migration_init_container` lambda creates an init container that runs database migrations before the main container starts:

```python
init_containers = [
    base.migration_init_container("myregistry/migrate:v1", base.DB_ENV)
]
```

## CLI Overrides with -D Flags

KCL's `option()` function reads values from the CLI. When `forge deploy` runs KCL, it passes `-D image_tag=<tag>` and `-D namespace=<ns>`. You can add your own:

```python
# In main.k
replicas: int = int(option("replicas", default="3"))
log_level: str = option("log_level", default="info")
```

Override from the command line:

```bash
kcl run deploy/kcl/staging/main.k \
  -D image_tag=sha-abc1234 \
  -D namespace=myproject-staging \
  -D replicas=5 \
  -D log_level=debug
```

Or through `forge deploy` which passes `image_tag` and `namespace` automatically:

```bash
forge deploy staging --image-tag sha-abc1234 --namespace custom-ns
```

## Adding a New Application

When you add a service with `forge add service`, you also need to add it to each environment's `main.k`. The pattern is:

1. Add the application to the `applications` dict in each `main.k` file
2. Configure environment-specific resources, replicas, and env vars
3. Run `forge deploy dev --dry-run` to verify the rendered manifests

## Customizing Deployments

To customize beyond what the schema provides, you have two options.

**Extend the schema.** Add fields to `schema.k` and handle them in the render lambdas in `render.k`. For example, to add a `topologySpreadConstraints` field:

```python
# In schema.k
schema Application:
    # ... existing fields ...
    topology_zone_spread: bool = False
```

```python
# In render.k, inside _render_deployment
if app.topology_zone_spread:
    topologySpreadConstraints = [
        {
            maxSkew = 1
            topologyKey = "topology.kubernetes.io/zone"
            whenUnsatisfiable = "DoNotSchedule"
            labelSelector = {matchLabels = {"app.kubernetes.io/name" = app.name}}
        }
    ]
```

**Post-process with KCL.** After calling `render.render_environment()`, you can modify or append resources:

```python
items = render.render_environment(env)

# Add a custom ConfigMap
items += [{
    apiVersion = "v1"
    kind = "ConfigMap"
    metadata = {
        name = "app-config"
        namespace = env.namespace
    }
    data = {
        "config.yaml" = "key: value"
    }
}]
```

## Previewing Manifests

To see what KCL renders without applying:

```bash
forge deploy prod --dry-run --image-tag v1.0.0
```

Or run KCL directly:

```bash
kcl run deploy/kcl/prod/main.k -D image_tag=v1.0.0
```

This prints the full YAML to stdout, which you can pipe to `kubectl diff` to see what would change:

```bash
kcl run deploy/kcl/prod/main.k -D image_tag=v1.0.0 | kubectl diff -f -
```

## Related Topics

- **[Architecture]({{< relref "../architecture" >}})** — how KCL fits into the overall deployment model
- **[CI/CD Guide]({{< relref "ci-cd" >}})** — how CI/CD workflows use KCL for automated deploys
- **[Getting Started]({{< relref "../getting-started" >}})** — the `forge deploy` command in context
