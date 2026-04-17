# Forge Lint Command

## Overview

The `forge lint` command provides comprehensive linting capabilities for your Forge projects.

## Usage

```bash
forge lint [flags] [paths...]
```

## Flags

- `--contract` - Run contract interface enforcement linter
- `--fix` - Automatically fix issues where possible
- `-v, --verbose` - Verbose output

## Examples

### Standard Linting

Run all standard linters (golangci-lint + buf):

```bash
forge lint
```

### Contract Interface Enforcement

Check that exported methods on service types implement their contract interfaces:

```bash
forge lint --contract
```

Run on specific paths:

```bash
forge lint --contract ./services/...
forge lint --contract ./internal/myservice
```

### Auto-fix Issues

Automatically fix issues where possible:

```bash
forge lint --fix
```

## What Gets Checked

### Standard Lint (`forge lint`)

- **golangci-lint**: Runs all configured Go linters
  - errcheck
  - gosimple
  - govet
  - ineffassign
  - staticcheck
  - unused
  - gofmt
  - goimports
  - gocritic

- **buf lint**: Lints proto files for style and best practices

### Contract Interface Enforcement (`forge lint --contract`)

Ensures architectural consistency by enforcing that:
- ✅ Exported methods on service types MUST implement their contract interface (proto RPC or `contract.go`)
- ✅ Exported types, functions, constants, and variables are unrestricted
- ✅ Unexported methods are allowed
- ❌ Exported methods on service types that don't match the contract are not allowed

This ensures developers always work through the Forge framework and don't create workarounds.

## Integration with CI/CD

Add to your CI pipeline:

```yaml
# .github/workflows/lint.yml
jobs:
  lint:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v3
      - uses: actions/setup-go@v4
      - name: Run linters
        run: |
          make build
          ./dist/forge lint
          ./dist/forge lint --contract
```

## Exit Codes

- `0` - All checks passed
- `1` - Linting failed (violations found)
- `3` - Analysis found issues (used by go/analysis framework)

## Output Example

```bash
$ forge lint --contract ./services

🔍 Running contract interface enforcement linter...

./services/user/service.go:45:1: exported method userService.GetUserByEmail does not implement a contract interface; only contract methods should be exported on service types

❌ Contract interface violations found!

Exported methods on types implementing contract interfaces must be declared in the interface.
```

## See Also

- [golangci-lint configuration](./../.golangci.yml)
- [Contributing Guidelines](./CONTRIBUTING.md)