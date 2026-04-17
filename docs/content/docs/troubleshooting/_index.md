---
title: "Troubleshooting"
description: "Common issues and solutions"
weight: 90
---

# Troubleshooting

Common issues and their solutions.

## Build Issues

### Proto Generation Fails

**Problem**: `forge generate` fails with errors

**Solutions**:
```bash
# Ensure buf is installed
go install github.com/bufbuild/buf/cmd/buf@latest

# Check proto syntax
buf lint

# Clean and regenerate
rm -rf gen/
forge generate
```

### Compilation Errors After Proto Changes

**Problem**: Go code doesn't compile after proto changes

**Solutions**:
```bash
# Regenerate all code
forge generate

# Tidy modules
cd gen && go mod tidy && cd ..
go mod tidy
```

## Runtime Issues

### Service Not Starting

**Problem**: Service fails to start

**Solution**: Ensure the service is registered in `pkg/app/wire.go`. Running `forge generate` should update the wiring automatically. If a service was added manually, run:
```bash
forge generate
```

### Database Connection Fails

**Problem**: Cannot connect to database

**Solutions**:
```bash
# Check if infrastructure is running
docker compose -f deploy/docker-compose.yml ps

# Start infrastructure
docker compose -f deploy/docker-compose.yml up -d

# Verify connection string in config
echo $DATABASE_URL
```

### Port Already in Use

**Problem**: `listen tcp :8080: bind: address already in use`

**Solutions**:
```bash
# Find the process using the port
lsof -i :8080

# Kill the process
kill -9 <PID>

# Or use a different port in forge.yaml
```

## Code Generation Issues

### Generated Code Out of Date

**Problem**: CI fails with "generated code not committed"

**Solution**: Run `forge generate` locally and commit the changes:
```bash
forge generate
git add gen/ pkg/app/wire.go services/mocks/
git commit -m "regenerate code"
```

### Mock Generation Fails

**Problem**: Mocks don't match the contract interface

**Solution**: Ensure `contract.go` interfaces are up to date, then regenerate:
```bash
forge generate
```

## Deployment Issues

### k3d Cluster Not Found

**Problem**: `forge deploy dev` can't find k3d cluster

**Solution**: Let `forge deploy dev` create the cluster automatically, or create it manually:
```bash
k3d cluster create dev --registry-create dev-registry:0.0.0.0:5050
```

### KCL Manifest Errors

**Problem**: KCL fails with schema validation errors

**Solution**: Check your `deploy/kcl/*/main.k` files against `deploy/kcl/schema.k`. Common issues:
- Missing required fields in `Application` schema
- Type mismatches in environment variables
- Invalid resource quantities

## Common Error Table

| Error | Cause | Fix |
|-------|-------|-----|
| `buf: command not found` | buf not installed | `go install github.com/bufbuild/buf/cmd/buf@latest` |
| `air: command not found` | Air not installed | `go install github.com/air-verse/air@latest` |
| `generated code differs` | Forgot to run generate | `forge generate && git add -A` |
| `connect: connection refused` | Service not running | `forge run` |
| `no such file: wire.go` | Missing wiring code | `forge generate` |
| `contract violation` | Method not in contract | Add method to proto service or `contract.go` interface |

## Getting More Help

- Run any command with `--verbose` for detailed output
- Check the [CLI Reference]({{< relref "../reference/cli" >}}) for all available flags
- [Open an issue](https://github.com/reliant-labs/forge/issues) for bugs
