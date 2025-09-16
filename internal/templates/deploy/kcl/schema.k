"""
Forge Application Schemas
Defines the configuration types for Kubernetes application deployments.
"""

schema Resources:
    """CPU and memory resource requests and limits."""
    cpu_request: str = "100m"
    cpu_limit: str = "500m"
    memory_request: str = "128Mi"
    memory_limit: str = "512Mi"

schema SecurityContext:
    """Pod and container security settings."""
    run_as_non_root: bool = True
    run_as_user: int = 65532
    run_as_group: int = 65532
    read_only_root_filesystem: bool = True
    allow_privilege_escalation: bool = False
    drop_capabilities: [str] = ["ALL"]
    seccomp_profile: str = "RuntimeDefault"

schema HealthCheck:
    """HTTP health check probe configuration."""
    http_path: str = "/healthz"
    http_port: int = 8080
    initial_delay: int = 5
    period: int = 10
    timeout: int = 3
    failure_threshold: int = 3

    check:
        initial_delay >= 0, "initial_delay must be non-negative"
        period > 0, "period must be positive"
        timeout > 0, "timeout must be positive"
        failure_threshold > 0, "failure_threshold must be positive"

schema EnvVar:
    """Environment variable with optional secret reference."""
    name: str
    value: str = ""
    secret_ref: str = ""
    secret_key: str = ""

    check:
        value != None or secret_ref, "EnvVar must have either a value or a secret_ref"

schema ServicePort:
    """Service port mapping."""
    name: str = "http"
    port: int = 80
    target_port: int = 8080
    protocol: str = "TCP"

    check:
        port > 0, "port must be positive"
        target_port > 0, "target_port must be positive"
        protocol in ["TCP", "UDP", "SCTP"], "protocol must be TCP, UDP, or SCTP"

schema AutoScaling:
    """Horizontal Pod Autoscaler configuration."""
    enabled: bool = False
    min_replicas: int = 1
    max_replicas: int = 10
    cpu_target: int = 80
    memory_target: int = 80

    check:
        min_replicas > 0, "min_replicas must be positive"
        max_replicas >= min_replicas, "max_replicas must be >= min_replicas"
        cpu_target > 0 and cpu_target <= 100, "cpu_target must be between 1 and 100"
        memory_target > 0 and memory_target <= 100, "memory_target must be between 1 and 100"

schema PodDisruptionBudget:
    """Pod disruption budget configuration."""
    enabled: bool = False
    min_available: int | None = 1
    max_unavailable: int | None = None

    check:
        not enabled or (min_available is not None or max_unavailable is not None), \
            "PDB must have either min_available or max_unavailable when enabled"

schema InitContainer:
    """Init container specification."""
    name: str
    image: str
    command: [str] = []
    env: [EnvVar] = []

schema IngressConfig:
    """Ingress configuration with TLS and cert-manager support."""
    enabled: bool = False
    host: str = ""
    tls: bool = True
    cert_manager_issuer: str = "letsencrypt-prod"
    paths: [{str: str}] = [{"path": "/", "path_type": "Prefix"}]

    check:
        not enabled or host, "host is required when ingress is enabled"

schema Application:
    """Complete application deployment configuration."""
    name: str
    image: str
    replicas: int = 1
    ports: [ServicePort] = [ServicePort {}]
    resources: Resources = Resources {}
    env_vars: [EnvVar] = []
    health_check: HealthCheck = HealthCheck {}
    security_context: SecurityContext = SecurityContext {}
    ingress: IngressConfig = IngressConfig {}
    autoscaling: AutoScaling = AutoScaling {}
    pdb: PodDisruptionBudget = PodDisruptionBudget {}
    init_containers: [InitContainer] = []
    labels: {str: str} = {}
    annotations: {str: str} = {}
    service_account_name: str = ""
    command: [str] = []
    args: [str] = []

    check:
        replicas > 0, "replicas must be positive"

schema Environment:
    """Environment-level configuration grouping multiple applications."""
    name: str
    namespace: str
    image_registry: str = ""
    image_tag: str = "latest"
    applications: {str: Application}
    network_policies: bool = True
    resource_quotas: bool = False