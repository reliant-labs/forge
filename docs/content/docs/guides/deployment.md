---
title: "Deployment"
description: "Deploying Forge applications to production"
weight: 100
---

# Deployment

Guide to deploying Forge applications in various environments.

## Docker

### Build Image

```bash
task docker-build
```

### Run Container

```bash
docker run -p 8080:8080 \
  -e DATABASE_URL=postgres://... \
  reliantlabs/forge:latest
```

## Kubernetes

Forge includes Kubernetes manifests:

```bash
# Deploy to dev
kubectl apply -k deployments/k8s/overlays/dev/

# Deploy to staging
kubectl apply -k deployments/k8s/overlays/staging/

# Deploy to prod
kubectl apply -k deployments/k8s/overlays/prod/
```

### Service Manifest

```yaml
apiVersion: v1
kind: Service
metadata:
  name: myservice
spec:
  selector:
    app: myservice
  ports:
    - port: 8080
      targetPort: 8080
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: myservice
spec:
  replicas: 3
  selector:
    matchLabels:
      app: myservice
  template:
    metadata:
      labels:
        app: myservice
    spec:
      containers:
        - name: myservice
          image: reliantlabs/myapp:latest
          ports:
            - containerPort: 8080
          env:
            - name: DATABASE_URL
              valueFrom:
                secretKeyRef:
                  name: db-credentials
                  key: url
```

## Environment Configuration

### Development

```yaml
# config/dev.yaml
database:
  host: localhost
  port: 5432

log_level: debug
```

### Production

```yaml
# config/prod.yaml
database:
  host: prod-db.internal
  port: 5432
  pool_size: 25

log_level: info
```

## Health Checks

Services implement health endpoints:

```go
func (s *Server) Check(
    ctx context.Context,
    req *grpc_health_v1.HealthCheckRequest,
) (*grpc_health_v1.HealthCheckResponse, error) {
    // Check database
    if err := s.db.Ping(ctx); err != nil {
        return &grpc_health_v1.HealthCheckResponse{
            Status: grpc_health_v1.HealthCheckResponse_NOT_SERVING,
        }, nil
    }

    return &grpc_health_v1.HealthCheckResponse{
        Status: grpc_health_v1.HealthCheckResponse_SERVING,
    }, nil
}
```

## Monitoring

### Metrics

Prometheus metrics exported automatically:

```
service_requests_total{service="UserService",method="CreateUser",status="success"} 1234
service_request_duration_seconds{service="UserService",method="CreateUser"} 0.050
```

### Logging

Structured logging:

```go
log.Printf("[UserService.CreateUser] Processing request user_id=%s", userID)
```

## Graceful Shutdown

```go
func main() {
    // Start server
    server := grpc.NewServer()
    go server.Serve(lis)

    // Wait for interrupt
    quit := make(chan os.Signal, 1)
    signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
    <-quit

    // Graceful shutdown
    log.Println("Shutting down server...")
    server.GracefulStop()
}
```

## Best Practices

1. **Use environment configs** - Different settings per environment
2. **Health checks** - Implement for all services
3. **Graceful shutdown** - Handle signals properly
4. **Monitoring** - Export metrics
5. **Logging** - Use structured logging
6. **Secrets** - Use environment variables or secret managers
7. **Rolling updates** - Zero-downtime deployments

## See Also

- [Performance]({{< ref "performance" >}})
- [Best Practices]({{< ref "best-practices" >}})
