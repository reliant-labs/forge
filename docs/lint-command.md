# Forge Lint Command

## Overview

The `forge lint` command provides comprehensive linting capabilities for your Forge projects.

## Usage

```bash
forge lint [flags] [paths...]
```

## Flags

- `--proto` - Run proto method enforcement linter (checks that exported methods only implement proto services)
- `--fix` - Automatically fix issues where possible
- `-v, --verbose` - Verbose output

## Examples

### Standard Linting

Run all standard linters (golangci-lint + buf):

```bash
forge lint
```

### Proto Method Enforcement

Check that all exported methods on receivers implement proto service interfaces:

```bash
forge lint --proto
```

Run on specific paths:

```bash
forge lint --proto ./services/...
forge lint --proto ./internal/myservice
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

### Proto Method Enforcement (`forge lint --proto`)

Ensures architectural consistency by enforcing that:
- ✅ Exported methods on receivers MUST implement proto service interfaces
- ✅ Exported functions (not on receivers) are allowed
- ✅ Unexported methods are allowed
- ❌ Exported methods that don't implement proto services are not allowed

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
          ./dist/forge lint --proto
```

Or use the Makefile:

```bash
make lint                # Standard linting
make lint-protomethod    # Proto method linting only
```

## Exit Codes

- `0` - All checks passed
- `1` - Linting failed (violations found)
- `3` - Analysis found issues (used by go/analysis framework)

## Output Example

```bash
$ forge lint --proto ./services

🔍 Running proto method enforcement linter...

Running: go run ./cmd/protomethod ./services

./services/user/service.go:45:1: exported method userService.GetUserByEmail does not implement a proto service interface; only proto service methods should be exported on receivers
./services/order/service.go:23:1: exported method orderService.CalculateTotal does not implement a proto service interface; only proto service methods should be exported on receivers

❌ Proto method violations found!

Exported methods on receivers must implement proto service interfaces.
See docs/linter-protomethod.md for more information.
```

## See Also

- [Proto Method Linter Documentation](./linter-protomethod.md)
- [golangci-lint configuration](./../.golangci.yml)
- [Contributing Guidelines](./CONTRIBUTING.md)
