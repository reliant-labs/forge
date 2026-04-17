"""
Forge Base Configurations
Composable environment variable groups and common init container templates.
"""

import .schema

DB_ENV: [schema.EnvVar] = [
    schema.EnvVar {
        name = "DATABASE_URL"
        secret_ref = "db-credentials"
        secret_key = "database-url"
    }
    schema.EnvVar {
        name = "DB_HOST"
        secret_ref = "db-credentials"
        secret_key = "host"
    }
    schema.EnvVar {
        name = "DB_PORT"
        secret_ref = "db-credentials"
        secret_key = "port"
    }
    schema.EnvVar {
        name = "DB_NAME"
        secret_ref = "db-credentials"
        secret_key = "name"
    }
    schema.EnvVar {
        name = "DB_USER"
        secret_ref = "db-credentials"
        secret_key = "username"
    }
    schema.EnvVar {
        name = "DB_PASSWORD"
        secret_ref = "db-credentials"
        secret_key = "password"
    }
]

OTEL_ENV: [schema.EnvVar] = [
    schema.EnvVar {
        name = "OTEL_EXPORTER_OTLP_ENDPOINT"
        value = "http://otel-collector.observability:4317"
    }
    schema.EnvVar {
        name = "OTEL_TRACES_SAMPLER"
        value = "parentbased_traceidratio"
    }
    schema.EnvVar {
        name = "OTEL_TRACES_SAMPLER_ARG"
        value = "0.1"
    }
]

COMMON_ENV: [schema.EnvVar] = []

# kcl v0.11 rejects fully-qualified schema literals inside a lambda body
# (e.g. `schema.InitContainer { ... }`); the parser only accepts bare
# identifiers for schema instantiation there. Binding the schema to a local
# alias keeps us importing `schema` once at the module level while letting
# the lambda body use the unqualified form.
_InitContainer = schema.InitContainer

migration_init_container = lambda image: str, db_env: [schema.EnvVar] -> schema.InitContainer {
    _InitContainer {
        name = "run-migrations"
        image = image
        command = ["./migrate", "up"]
        env = db_env
    }
}