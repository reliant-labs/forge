# KCL Deployments

Forge uses [KCL](https://kcl-lang.io/) (Kusion Configuration Language) instead of Helm or Kustomize for Kubernetes manifest generation. KCL is a typed configuration language with schema validation, which means misconfigured deployments are caught at `kcl run` time rather than at `kubectl apply` time. This guide covers the generated KCL structure, the Application schema, multi-environment configuration, and how to customize deployments.

## Why KCL Instead of Helm or Kustomize

Helm templates are untyped Go text templates operating on YAML -- you can produce invalid Kubernetes manifests without any warning until apply time. Kustomize patches are fragile and hard to reason about for complex configurations. KCL addresses both problems:

- **Typed schemas** -- The `Application` schema defines exactly what fields are valid, what types they accept, and what constraints they must satisfy. A missing required field or an invalid replica count is a compile error, not a runtime surprise.
- **Validation rules** -- `check` blocks enforce invariants like "replicas must be positive" and "PDB must have either min_available or max_unavailable". These run during `kcl run` and produce clear error messages.
- **Composability** -- Base configurations (common env vars, init containers, shared resources) are KCL values you import and compose with `+`, not YAML anchors or Kustomize patches.
- **No templating language** -- KCL is a real programming language with imports, lambda functions, and type checking. You define configurations, not templates that produce configurations.

## Generated KCL Structure

When you create a project, Forge generates this structure under `deploy/kcl/`:

```
deploy/kcl/
├── schema.k          # Type definitions (Application, Environment, Resources, etc.)
├── render.k          # Private lambdas that produce K8s manifests from schemas
├── base.k            # Shared configurations (common env vars, DB env, init containers)
├── dev/main.k        # Development environment
├── staging/main.k    # Staging environment
└── prod/main.k       # Production environment
```

Each environment's `main.k` is the entry point. When you run `forge deploy dev`, it executes:

```bash
kcl run deploy/kcl/dev/main.k -D image_tag=<tag> -D namespace=<ns>
```

The output is standard Kubernetes YAML that gets piped to `kubectl apply --server-side`.

## The Application Schema

The `schema.k` file defines the types used across all environments. The central type is `Application`:

```python
schema Application:
    """Complete application deployment configuration."""
    name: str
    image: str
    replicas: int = 1
    ports: [ServicePort]
    resources: Resources
    env_vars: [EnvVar] = []
    health_check?: HealthCheck
    security_context: SecurityContext = SecurityContext {}
    ingress?: IngressConfig
    autoscaling?: AutoScaling
    pdb?: PodDisruptionBudget
    init_containers: [InitContainer] = []
    command?: [str]
    args?: [str]

    check:
        replicas > 0, "replicas must be positive"
```

Each `Application` renders into a Deployment, Service, and optionally an Ingress, HPA, and PDB. The render engine (`render.k`) contains the lambdas that transform these typed configurations into Kubernetes manifest YAML.

### Supporting Schemas

```python
schema Resources:
    cpu_request: str        # e.g., "100m"
    memory_request: str     # e.g., "128Mi"
    cpu_limit: str          # e.g., "500m"
    memory_limit: str       # e.g., "256Mi"

schema ServicePort:
    name: str
    port: int               # Service port
    target_port: int        # Container port
    protocol: str = "TCP"

schema HealthCheck:
    http_path: str          # e.g., "/healthz"
    port: int
    initial_delay_seconds: int = 5
    period_seconds: int = 10
    failure_threshold: int = 3

schema EnvVar:
    name: str
    value?: str             # Plain value
    secret_ref?: str        # Secret reference as "secret-name/key"

    check:
        (value is not None) or (secret_ref is not None), "EnvVar must have either value or secret_ref"

schema AutoScaling:
    min_replicas: int
    max_replicas: int
    cpu_target: int = 80
    memory_target?: int

    check:
        min_replicas > 0, "min_replicas must be positive"
        max_replicas >= min_replicas, "max_replicas must be >= min_replicas"

schema SecurityContext:
    non_root: bool = True
    read_only_rootfs: bool = True
    drop_all_caps: bool = True
    seccomp_runtime_default: bool = True
```

## Multi-Environment Configuration

Each environment defines an `Environment` that groups applications with environment-specific settings:

```python
schema Environment:
    name: str
    namespace: str
    image_registry: str
    image_tag: str
    applications: {str:Application}
    network_policies: bool = False
    pod_security_labels: bool = True
```

### Development Environment

The generated `dev/main.k` looks like this:

```python
import deploy.kcl.schema
import deploy.kcl.base
import deploy.kcl.render

env = schema.Environment {
    name = "dev"
    namespace = "myproject-dev"
    image_registry = "localhost:5000"
    image_tag = "latest"
    network_policies = False
    pod_security_labels = True
    applications = {
        "myproject" = schema.Application {
            name = "myproject"
            image = "myproject"
            replicas = 1
            ports = [
                schema.ServicePort {
                    name = "http"
                    port = 80
                    target_port = 8080
                }
            ]
            resources = schema.Resources {
                cpu_request = "100m"
                memory_request = "128Mi"
                cpu_limit = "500m"
                memory_limit = "256Mi"
            }
            env_vars = base.COMMON_ENV + [
                schema.EnvVar {
                    name = "LOG_LEVEL"
                    value = "debug"
                }
            ]
            health_check = schema.HealthCheck {
                http_path = "/healthz"
                port = 8080
            }
        }
    }
}

manifests = render.render_environment(env)
```

### Staging and Production

Staging and production environments follow the same pattern but with different resource allocations, replica counts, and additional features like autoscaling and network policies. You customize these files directly.

A production configuration might look like:

```python
env = schema.Environment {
    name = "prod"
    namespace = "myproject-prod"
    image_registry = "ghcr.io/example"
    image_tag = "v1.0.0"
    network_policies = True
    applications = {
        "api" = schema.Application {
            name = "api"
            image = "api"
            replicas = 3
            ports = [
                schema.ServicePort { name = "http", port = 80, target_port = 8080 }
            ]
            resources = schema.Resources {
                cpu_request = "250m"
                memory_request = "256Mi"
                cpu_limit = "1000m"
                memory_limit = "512Mi"
            }
            env_vars = base.COMMON_ENV + base.DB_ENV + [
                schema.EnvVar { name = "LOG_LEVEL", value = "warn" }
            ]
            health_check = schema.HealthCheck { http_path = "/healthz", port = 8080 }
            autoscaling = schema.AutoScaling {
                min_replicas = 3
                max_replicas = 10
                cpu_target = 70
            }
            pdb = schema.PodDisruptionBudget {
                min_available = 2
            }
            ingress = schema.IngressConfig {
                host = "api.example.com"
                tls = True
                paths = [
                    schema.IngressPath {
                        path = "/"
                        service_name = "api"
                        service_port = 80
                    }
                ]
            }
        }
    }
}
```

## Composing with Base Configurations

The `base.k` file provides reusable configuration blocks:

```python
# Common environment variables shared across all services
COMMON_ENV: [schema.EnvVar] = [
    schema.EnvVar { name = "LOG_LEVEL", value = "info" }
    schema.EnvVar { name = "SERVICE_NAME", value = "" }
]

# Database connection variables (all from secrets)
DB_ENV: [schema.EnvVar] = [
    schema.EnvVar { name = "DATABASE_URL", secret_ref = "db-credentials/database-url" }
    schema.EnvVar { name = "DB_HOST", secret_ref = "db-credentials/host" }
    schema.EnvVar { name = "DB_PORT", secret_ref = "db-credentials/port" }
    schema.EnvVar { name = "DB_NAME", secret_ref = "db-credentials/name" }
    schema.EnvVar { name = "DB_USER", secret_ref = "db-credentials/username" }
    schema.EnvVar { name = "DB_PASSWORD", secret_ref = "db-credentials/password" }
]

# Redis connection
REDIS_ENV: [schema.EnvVar] = [
    schema.EnvVar { name = "REDIS_URL", secret_ref = "redis-credentials/url" }
]

# Init container for database migrations
migration_init_container = lambda image: str, db_env: [schema.EnvVar] -> schema.InitContainer {
    schema.InitContainer {
        name = "run-migrations"
        image = image
        command = ["./migrate", "up"]
        env = db_env
    }
}
```

Compose these in your environment configs:

```python
env_vars = base.COMMON_ENV + base.DB_ENV + base.REDIS_ENV + [
    schema.EnvVar { name = "CUSTOM_VAR", value = "custom-value" }
]
```

## Adding Services to KCL

When you add a new service with `forge add service`, the KCL files are not automatically updated -- you add the service to each environment's `main.k` manually. This is intentional: each environment may need different configuration for the same service.

Add a new service to `dev/main.k`:

```python
applications = {
    "api" = schema.Application { ... }   # existing
    "users" = schema.Application {
        name = "users"
        image = "users"
        replicas = 1
        ports = [
            schema.ServicePort { name = "grpc", port = 50051, target_port = 50051 }
        ]
        resources = schema.Resources {
            cpu_request = "100m"
            memory_request = "128Mi"
            cpu_limit = "500m"
            memory_limit = "256Mi"
        }
        env_vars = base.COMMON_ENV + base.DB_ENV
        health_check = schema.HealthCheck { http_path = "/healthz", port = 50051 }
    }
}
```

## CLI Overrides

The `forge deploy` command passes `-D` flags to `kcl run`, letting you override values at deploy time:

```bash
# Override namespace
forge deploy dev --namespace custom-ns

# Override image tag
forge deploy staging --image-tag sha-abc1234
```

These translate to:

```bash
kcl run deploy/kcl/staging/main.k -D image_tag=sha-abc1234 -D namespace=custom-ns
```

You can reference these dynamic values in your `main.k` with the `option()` function if you need custom dynamic fields beyond what `forge deploy` passes.

## What Gets Generated

The render engine (`render.k`) produces these Kubernetes resources from each `Application`:

| Resource | Condition |
|----------|-----------|
| Deployment | Always |
| Service | Always |
| Ingress | When `ingress` is set |
| HorizontalPodAutoscaler | When `autoscaling` is set |
| PodDisruptionBudget | When `pdb` is set |
| NetworkPolicy (default-deny + allow-dns + allow-within-namespace) | When `network_policies` is `True` on the Environment |
| Namespace (with pod security labels) | When `pod_security_labels` is `True` |

All generated resources include standard labels (`app.kubernetes.io/name`, `app.kubernetes.io/managed-by: forge`) and follow Kubernetes security best practices: non-root containers, read-only root filesystem, dropped capabilities, and RuntimeDefault seccomp profiles.

## Previewing Manifests

Use `--dry-run` to see what would be applied without making changes:

```bash
forge deploy prod --dry-run --image-tag v1.0.0
```

This prints the full YAML output so you can review before applying.

## Related Guides

- [Getting Started](getting-started.md) -- Creating a project and deploying for the first time
- [CI/CD Pipelines](cicd.md) -- How the deploy workflow uses KCL in GitHub Actions
- [CLI Reference](cli-reference.md) -- Full `forge deploy` flag documentation
