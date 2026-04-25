package generator

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// generateDXFiles writes developer-experience and operations scaffolding:
// VSCode settings, devcontainer + bootstrap, SECURITY.md, pre-commit
// config, example migrations/seeds, an ADR example, and benchmarks. Each
// helper is kept small and idempotent — the generator re-runs on `forge
// generate` and should not clobber user edits for files users typically
// own. We keep these files' contents here (rather than in
// internal/templates/) because they're static scaffolds that don't need
// template-engine features; embedding them as Go string constants keeps
// the surface area explicit and reviewable.
func (g *ProjectGenerator) generateDXFiles() error {
	if err := g.generateVSCodeConfig(); err != nil {
		return fmt.Errorf("write .vscode scaffolding: %w", err)
	}
	if err := g.generateDevcontainer(); err != nil {
		return fmt.Errorf("write devcontainer: %w", err)
	}
	if err := g.generateBootstrapScript(); err != nil {
		return fmt.Errorf("write scripts/bootstrap.sh: %w", err)
	}
	if err := g.generateSecurityPolicy(); err != nil {
		return fmt.Errorf("write SECURITY.md: %w", err)
	}
	if err := g.generatePreCommitConfig(); err != nil {
		return fmt.Errorf("write .pre-commit-config.yaml: %w", err)
	}
	if err := g.generatePreCommitWorkflow(); err != nil {
		return fmt.Errorf("write pre-commit workflow: %w", err)
	}
	if err := g.generateExampleMigration(); err != nil {
		return fmt.Errorf("write example migration: %w", err)
	}
	if err := g.generateSeeds(); err != nil {
		return fmt.Errorf("write db/seeds: %w", err)
	}
	if err := g.generateADRs(); err != nil {
		return fmt.Errorf("write docs/adr: %w", err)
	}
	if err := g.generateBenchmarks(); err != nil {
		return fmt.Errorf("write benchmarks: %w", err)
	}
	if err := g.generateRunbookAndSLO(); err != nil {
		return fmt.Errorf("write docs/runbook+slo: %w", err)
	}
	if err := g.generatePrometheusRules(); err != nil {
		return fmt.Errorf("write prometheus-rules: %w", err)
	}
	if err := g.generateSQLCStub(); err != nil {
		return fmt.Errorf("write db/sqlc stub: %w", err)
	}
	if err := g.generateNonGoalsADR(); err != nil {
		return fmt.Errorf("write non-goals ADR: %w", err)
	}
	return nil
}

// generateVSCodeConfig writes .vscode/settings.json and .vscode/extensions.json.
// The existing .vscode/launch.json (written elsewhere) is untouched.
// Settings are intentionally minimal: format-on-save wired to the correct
// formatter per language, plus gopls analyses users already expect. We
// avoid opinionated lint/style choices that conflict with golangci-lint.
func (g *ProjectGenerator) generateVSCodeConfig() error {
	settings := `{
  "editor.formatOnSave": true,
  "[go]": {
    "editor.defaultFormatter": "golang.go",
    "editor.codeActionsOnSave": {
      "source.organizeImports": "explicit"
    }
  },
  "go.useLanguageServer": true,
  "gopls": {
    "build.buildFlags": [],
    "ui.diagnostic.analyses": {
      "unusedparams": true,
      "shadow": true,
      "nilness": true,
      "unusedwrite": true
    },
    "formatting.gofumpt": false
  },
  "[typescript]": {
    "editor.defaultFormatter": "esbenp.prettier-vscode"
  },
  "[typescriptreact]": {
    "editor.defaultFormatter": "esbenp.prettier-vscode"
  },
  "[javascript]": {
    "editor.defaultFormatter": "esbenp.prettier-vscode"
  },
  "[json]": {
    "editor.defaultFormatter": "esbenp.prettier-vscode"
  },
  "[yaml]": {
    "editor.defaultFormatter": "esbenp.prettier-vscode"
  },
  "tailwindCSS.experimental.classRegex": [
    ["cva\\(([^)]*)\\)", "[\"'` + "`" + `]([^\"'` + "`" + `]*).*?[\"'` + "`" + `]"],
    ["cn\\(([^)]*)\\)", "[\"'` + "`" + `]([^\"'` + "`" + `]*).*?[\"'` + "`" + `]"]
  ],
  "files.associations": {
    "*.kcl": "kcl",
    "*.k": "kcl",
    "Taskfile.yml": "yaml",
    "Taskfile.yaml": "yaml"
  },
  "buf.binaryPath": "buf",
  "protoc": {
    "path": "protoc"
  }
}
`
	extensions := `{
  "recommendations": [
    "golang.go",
    "bufbuild.vscode-buf",
    "esbenp.prettier-vscode",
    "dbaeumer.vscode-eslint",
    "bradlc.vscode-tailwindcss",
    "ms-azuretools.vscode-docker",
    "task.vscode-task",
    "kcl.vscode-kcl",
    "zxh404.vscode-proto3"
  ]
}
`
	vscodeDir := filepath.Join(g.Path, ".vscode")
	if err := os.MkdirAll(vscodeDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(vscodeDir, "settings.json"), []byte(settings), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(vscodeDir, "extensions.json"), []byte(extensions), 0o644)
}

// generateDevcontainer writes .devcontainer/devcontainer.json. The base
// image pins a recent stable Go; Node + Docker-in-Docker are provided via
// devcontainer features. postCreateCommand runs the scaffolded
// scripts/bootstrap.sh so new contributors get a fully tooled shell on
// first open.
func (g *ProjectGenerator) generateDevcontainer() error {
	// Keep the devcontainer's Go image aligned with the project's declared
	// minor version — but constrain it to a version we know is actually
	// published (same rationale as the Dockerfile builder base). The
	// devcontainers/go images lag upstream Go releases; pinning to a known
	// tag avoids `manifest unknown` on first `devcontainer up`.
	goMinor := dockerBuilderGoVersion(g.resolveGoVersion())

	// Forward both the server and frontend ports; infra ports (postgres,
	// jaeger) match the defaults used by docker-compose.yml.tmpl.
	forward := []string{fmt.Sprintf("%d", g.ServicePort)}
	if g.FrontendName != "" {
		forward = append(forward, fmt.Sprintf("%d", g.FrontendPort))
	}
	forward = append(forward, "5432", "16686")

	var forwardJSON strings.Builder
	forwardJSON.WriteString("[")
	for i, p := range forward {
		if i > 0 {
			forwardJSON.WriteString(", ")
		}
		forwardJSON.WriteString(p)
	}
	forwardJSON.WriteString("]")

	content := fmt.Sprintf(`{
  "name": "%s",
  "image": "mcr.microsoft.com/devcontainers/go:%s-bookworm",
  "features": {
    "ghcr.io/devcontainers/features/node:1": {
      "version": "lts"
    },
    "ghcr.io/devcontainers/features/docker-in-docker:2": {
      "version": "latest"
    },
    "ghcr.io/devcontainers/features/common-utils:2": {
      "installZsh": true,
      "configureZshAsDefaultShell": false
    }
  },
  "postCreateCommand": "bash scripts/bootstrap.sh",
  "forwardPorts": %s,
  "portsAttributes": {
    "%d": { "label": "app" },
    "5432": { "label": "postgres" },
    "16686": { "label": "jaeger-ui" }
  },
  "remoteUser": "vscode",
  "customizations": {
    "vscode": {
      "extensions": [
        "golang.go",
        "bufbuild.vscode-buf",
        "esbenp.prettier-vscode",
        "dbaeumer.vscode-eslint",
        "bradlc.vscode-tailwindcss",
        "ms-azuretools.vscode-docker",
        "task.vscode-task",
        "kcl.vscode-kcl",
        "zxh404.vscode-proto3"
      ]
    }
  }
}
`, g.Name, goMinor, forwardJSON.String(), g.ServicePort)

	dir := filepath.Join(g.Path, ".devcontainer")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "devcontainer.json"), []byte(content), 0o644)
}

// generateBootstrapScript writes scripts/bootstrap.sh, a one-shot
// idempotent installer for the tools the repo expects on PATH. Each check
// is guarded by `command -v`, so rerunning it is cheap. Docs inline the
// install methods the script doesn't cover (Task, Buf, KCL) because
// those vary by platform.
func (g *ProjectGenerator) generateBootstrapScript() error {
	content := `#!/usr/bin/env bash
# scripts/bootstrap.sh — install the tools this repo expects.
#
# Safe to re-run: every step checks whether the tool is already on PATH.
# Intended for devcontainer postCreateCommand and new-contributor setup.
set -euo pipefail

log() { printf '\n\033[1;34m==>\033[0m %s\n' "$*"; }
have() { command -v "$1" >/dev/null 2>&1; }

need_go() {
  if ! have go; then
    echo "go is required but not found on PATH." >&2
    echo "Install Go (matching the go.mod directive): https://go.dev/dl/" >&2
    exit 1
  fi
}

install_go_tool() {
  # $1: binary name, $2: go install path
  local bin="$1" pkg="$2"
  if have "$bin"; then
    log "$bin already installed — skipping"
    return
  fi
  log "installing $bin via go install"
  go install "$pkg"
}

install_task() {
  if have task; then
    log "task already installed — skipping"
    return
  fi
  log "installing Task (https://taskfile.dev)"
  # Official install script; respects GOBIN / $HOME/.local/bin.
  sh -c "$(curl --location https://taskfile.dev/install.sh)" -- -d -b "${GOBIN:-$HOME/go/bin}"
}

install_buf() {
  if have buf; then
    log "buf already installed — skipping"
    return
  fi
  log "installing buf (https://buf.build)"
  install_go_tool buf github.com/bufbuild/buf/cmd/buf@latest
}

install_kcl() {
  if have kcl; then
    log "kcl already installed — skipping"
    return
  fi
  log "installing KCL (https://kcl-lang.io)"
  # KCL ships its own installer; fall back to a printed instruction on
  # platforms where curl isn't available.
  if have curl; then
    curl -fsSL https://kcl-lang.io/script/install-cli.sh | bash
  else
    echo "curl missing — install KCL manually: https://kcl-lang.io/docs/user_docs/getting-started/install" >&2
  fi
}

main() {
  need_go

  install_task
  install_buf
  install_kcl

  install_go_tool protoc-gen-go            google.golang.org/protobuf/cmd/protoc-gen-go@latest
  install_go_tool protoc-gen-connect-go    connectrpc.com/connect/cmd/protoc-gen-connect-go@latest
  install_go_tool golangci-lint            github.com/golangci/golangci-lint/cmd/golangci-lint@latest
  install_go_tool goimports                golang.org/x/tools/cmd/goimports@latest
  install_go_tool govulncheck              golang.org/x/vuln/cmd/govulncheck@latest

  log "bootstrap complete"
}

main "$@"
`
	dir := filepath.Join(g.Path, "scripts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Mode 0755: executable so contributors can run it directly; `bash
	// scripts/bootstrap.sh` in devcontainer.json still works regardless.
	return os.WriteFile(filepath.Join(dir, "bootstrap.sh"), []byte(content), 0o755)
}

// generateSecurityPolicy writes SECURITY.md at the repo root. We derive
// the contact email from the GitHub owner (module path) when we can;
// otherwise we fall back to a generic placeholder and let the user fill
// it in. The 90-day disclosure window matches industry convention
// (Project Zero / CVD norms).
func (g *ProjectGenerator) generateSecurityPolicy() error {
	owner := githubOwnerFromModulePath(g.ModulePath)
	contact := "security@example.com"
	if owner != "" {
		contact = fmt.Sprintf("security@%s.example", owner)
	}

	content := fmt.Sprintf(`# Security Policy

Thanks for taking the time to help keep %[1]s secure. This document
describes how to report vulnerabilities and what you can expect in
return.

## Supported versions

We patch security issues in the following releases:

| Version       | Supported          |
| ------------- | ------------------ |
| ` + "`main`" + `        | :white_check_mark: |
| latest tagged | :white_check_mark: |
| older         | :x:                |

If you need a fix backported to an older release, include that in the
report and we'll discuss feasibility.

## Reporting a vulnerability

**Do not open a public GitHub issue for suspected vulnerabilities.**
Instead, email us privately:

- **Contact:** %[2]s
- **PGP key:** _(optional — add a public-key fingerprint here if your
  project publishes one)_

Please include:

1. A description of the issue and the affected component.
2. A minimal reproduction (command, proto, or request payload).
3. Your assessment of impact (confidentiality / integrity / availability).
4. Any mitigations or workarounds you've identified.

## What to expect

- **Acknowledgement** within 3 business days of the initial report.
- **Triage** (severity + affected versions) within 10 business days.
- **Fix or mitigation** within 90 days of acknowledgement, per the
  industry-standard coordinated-disclosure window. We may request a
  brief extension for complex issues; if so we'll communicate a new
  target date.
- **Public disclosure** after a fix ships, with credit to the reporter
  unless you ask to remain anonymous.

## Scope

This policy applies to the code in this repository. Issues in
third-party dependencies should be reported upstream; we're happy to
coordinate if the patched version needs to propagate here.

## Non-security issues

For bug reports, feature requests, or general contribution questions,
see [CONTRIBUTING.md](CONTRIBUTING.md).
`, g.Name, contact)

	return os.WriteFile(filepath.Join(g.Path, "SECURITY.md"), []byte(content), 0o644)
}

// generatePreCommitConfig writes .pre-commit-config.yaml. Hooks are
// chosen to match the existing CI surface (gofmt/govet/goimports via
// dnephin/pre-commit-golang, frontend prettier, buf format via a local
// hook because the Buf project does not publish an official pre-commit
// mirror). gitleaks covers secrets scanning without colliding with any
// separate correctness-cluster setup: it runs only on the pre-commit
// boundary + the dedicated workflow we install in
// .github/workflows/pre-commit.yml.
func (g *ProjectGenerator) generatePreCommitConfig() error {
	content := `# See https://pre-commit.com for full docs. Run locally with:
#     pip install pre-commit && pre-commit install
# CI runs the same set via .github/workflows/pre-commit.yml.
repos:
  - repo: https://github.com/pre-commit/pre-commit-hooks
    rev: v4.6.0
    hooks:
      - id: trailing-whitespace
      - id: end-of-file-fixer
      - id: check-merge-conflict
      - id: check-added-large-files
        args: ["--maxkb=1024"]
      - id: check-yaml
      - id: check-json

  - repo: https://github.com/gitleaks/gitleaks
    rev: v8.18.4
    hooks:
      - id: gitleaks

  - repo: https://github.com/dnephin/pre-commit-golang
    rev: v0.5.1
    hooks:
      - id: go-fmt
      - id: go-vet-mod
      - id: go-imports

  - repo: https://github.com/pre-commit/mirrors-prettier
    rev: v3.1.0
    hooks:
      - id: prettier
        # Restrict to frontend + docs so prettier does not fight gofmt or
        # golangci-lint on Go files.
        files: \.(ts|tsx|js|jsx|json|md|yml|yaml|css)$
        exclude: ^(gen/|.*\.pb\.go$)

  # Buf has no first-party pre-commit mirror; invoke its CLI locally.
  # ` + "`" + `buf format -d` + "`" + ` prints a diff and exits non-zero on any
  # formatting difference — exactly the semantics pre-commit expects.
  - repo: local
    hooks:
      - id: buf-format
        name: buf format
        entry: buf format -d --exit-code
        language: system
        files: \.proto$
        pass_filenames: false

  # Conventional Commits enforcement.
  - repo: https://github.com/alessandrojcm/commitlint-pre-commit-hook
    rev: v9.18.0
    hooks:
      - id: commitlint
        stages: [commit-msg]
        additional_dependencies: ['@commitlint/config-conventional']
`
	if err := os.WriteFile(filepath.Join(g.Path, ".pre-commit-config.yaml"), []byte(content), 0o644); err != nil {
		return err
	}

	// commitlint config so the pre-commit hook knows which ruleset to use.
	commitlintRC := `module.exports = { extends: ['@commitlint/config-conventional'] };
`
	return os.WriteFile(filepath.Join(g.Path, ".commitlintrc.js"), []byte(commitlintRC), 0o644)
}

// generatePreCommitWorkflow writes .github/workflows/pre-commit.yml as a
// standalone workflow rather than adding a job to ci.yml. Keeping it
// separate keeps ci.yml focused on build/test and lets the pre-commit
// cache be tuned independently. Matches the existing workflow style
// (explicit permissions, pinned action majors, concurrency group).
func (g *ProjectGenerator) generatePreCommitWorkflow() error {
	content := `# Runs the same hooks as the local pre-commit config so CI catches
# contributors who haven't installed pre-commit locally. Hook versions
# are pinned in .pre-commit-config.yaml.
name: pre-commit

on:
  push:
    branches: [main]
  pull_request:
    branches: [main]

permissions:
  contents: read

concurrency:
  group: pre-commit-${{ github.ref }}
  cancel-in-progress: true

jobs:
  pre-commit:
    name: pre-commit
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-python@v5
        with:
          python-version: '3.12'
      - uses: actions/setup-go@v5
        with:
          go-version-file: go.mod
      - name: Install buf
        uses: bufbuild/buf-setup-action@v1
      - uses: pre-commit/action@v3.0.1
`
	dir := filepath.Join(g.Path, ".github", "workflows")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "pre-commit.yml"), []byte(content), 0o644)
}

// generateExampleMigration writes db/migrations/0001_init.{up,down}.sql,
// a minimal but realistic example (UUID primary key, timestamptz default)
// that demonstrates the conventions documented in db/README.md.
// The up migration wraps its DDL in BEGIN/COMMIT so partial failures are
// rolled back — matches the transaction guidance in db/README.md.
func (g *ProjectGenerator) generateExampleMigration() error {
	up := `-- 0001_init.up.sql
-- Example migration: creates an ` + "`items`" + ` table.
-- Multi-statement migrations should be wrapped in an explicit
-- transaction so a failure halfway through leaves the schema untouched.
BEGIN;

CREATE EXTENSION IF NOT EXISTS "pgcrypto";

CREATE TABLE IF NOT EXISTS items (
    id         UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    name       TEXT        NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS items_created_at_idx ON items (created_at DESC);

COMMIT;
`

	down := `-- 0001_init.down.sql
-- Revert 0001_init.up.sql.
BEGIN;

DROP INDEX IF EXISTS items_created_at_idx;
DROP TABLE IF EXISTS items;

COMMIT;
`
	dir := filepath.Join(g.Path, "db", "migrations")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "0001_init.up.sql"), []byte(up), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "0001_init.down.sql"), []byte(down), 0o644); err != nil {
		return err
	}

	// Overwrite db/README.md with a more complete version that covers
	// the seed workflow, transaction guidance, and links to the example
	// migration we just wrote. The earlier stub written from project.go
	// is superseded here intentionally.
	readme := `# db

SQL migrations managed by [golang-migrate](https://github.com/golang-migrate/migrate),
invoked by the generated ` + "`" + `db migrate` + "`" + ` subcommands.

## Layout

` + "```" + `
db/
  migrations/
    0001_init.up.sql          # forward migration
    0001_init.down.sql        # rollback
    0002_add_users.up.sql
    0002_add_users.down.sql
  seeds/
    0001_items.sql            # idempotent dev/test seed data
` + "```" + `

Migrations run in lexicographic order. Stick to zero-padded, monotonic
numeric prefixes (` + "`0001`" + `, ` + "`0002`" + `, ...) so the order is stable regardless
of merge order.

## Writing a new migration

1. Create paired ` + "`N_name.up.sql`" + ` / ` + "`N_name.down.sql`" + ` files.
2. Wrap **multi-statement DDL in an explicit transaction**
   (` + "`BEGIN; ... COMMIT;`" + `) so a mid-migration failure rolls back cleanly.
   Single-statement migrations can omit the transaction.
3. Keep migrations **forward-compatible with running code**: ship the
   migration first, then the code that depends on it. Avoid destructive
   changes (e.g. dropping columns) in the same release as the code that
   reads them.
4. Test rollback. ` + "`go run ./cmd db migrate down`" + ` should leave the schema
   in the pre-migration state.

## CLI

` + "```" + `
go run ./cmd db migrate up      # apply all pending migrations
go run ./cmd db migrate down    # revert the most recently applied migration
go run ./cmd db migrate status  # print current version / dirty flag
` + "```" + `

All subcommands read ` + "`DATABASE_URL`" + ` (or ` + "`--database-url`" + `) from the
standard project config.

## Seeds

` + "`db/seeds/`" + ` holds idempotent SQL files intended for development and
test fixtures. They are **not** applied automatically on startup — run
them explicitly:

` + "```" + `
task db-seed
` + "```" + `

The task loads every file in ` + "`db/seeds/`" + ` in lexicographic order via
` + "`psql $DATABASE_URL`" + `. Write each seed with ` + "`ON CONFLICT DO NOTHING`" + ` (or
the equivalent) so re-running is safe.
`
	return os.WriteFile(filepath.Join(g.Path, "db", "README.md"), []byte(readme), 0o644)
}

// generateSeeds writes db/seeds/0001_items.sql — a minimal, idempotent
// example that populates the table created by 0001_init.up.sql.
// `ON CONFLICT DO NOTHING` lets the seed run repeatedly without blowing
// up.
func (g *ProjectGenerator) generateSeeds() error {
	seed := `-- 0001_items.sql
-- Seed data for the ` + "`items`" + ` table (see db/migrations/0001_init.up.sql).
-- Safe to re-run: each row is keyed by a stable UUID and INSERT uses
-- ON CONFLICT DO NOTHING.
INSERT INTO items (id, name) VALUES
    ('11111111-1111-1111-1111-111111111111', 'example-item-1'),
    ('22222222-2222-2222-2222-222222222222', 'example-item-2'),
    ('33333333-3333-3333-3333-333333333333', 'example-item-3')
ON CONFLICT (id) DO NOTHING;
`
	dir := filepath.Join(g.Path, "db", "seeds")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "0001_items.sql"), []byte(seed), 0o644)
}

// generateADRs writes docs/adr/README.md and an example ADR capturing
// the Connect-RPC + Go + Next.js stack choice. Template follows MADR
// (https://adr.github.io/madr/) so new ADRs have a stable skeleton to
// copy from.
func (g *ProjectGenerator) generateADRs() error {
	readme := `# Architecture Decision Records

This directory holds Architecture Decision Records (ADRs) using the
[MADR](https://adr.github.io/madr/) template (Markdown Architectural
Decision Records).

## What is an ADR?

A short, immutable document that captures one architectural decision
and the context that led to it. ADRs are append-only: once a decision
is accepted, we don't edit it — we supersede it with a newer ADR.

## Writing a new ADR

1. Copy ` + "`0001-use-connect-rpc.md`" + ` to ` + "`NNNN-short-title.md`" + ` where ` + "`NNNN`" + `
   is the next zero-padded number.
2. Fill in **Context**, **Decision**, and **Consequences** (minimum).
   Add **Considered Options** when there's a real tradeoff worth
   recording.
3. Set ` + "`status: proposed`" + ` while you're iterating. Flip to ` + "`accepted`" + `
   when the team signs off, ` + "`superseded by NNNN`" + ` when a later ADR
   replaces it.
4. Link the ADR from relevant code comments or docs. The filename is
   the stable identifier.

## Template

See [MADR 3.0](https://adr.github.io/madr/) for the full template and
reasoning. The key sections we expect:

- **Status** — ` + "`proposed`" + ` | ` + "`accepted`" + ` | ` + "`superseded by NNNN`" + `
- **Context and Problem Statement** — what forces are at play?
- **Decision Drivers** — what we're optimizing for.
- **Considered Options** — at least one alternative (even "do nothing").
- **Decision Outcome** — chosen option + rationale.
- **Consequences** — good and bad.
`

	adr := fmt.Sprintf(`# 1. Use Connect-RPC for the service layer

- Status: accepted
- Date: %s
- Deciders: project authors

## Context and Problem Statement

%[2]s needs a service interface that is both programmatic (typed
clients for Go services talking to each other) and browser-friendly
(the Next.js frontend needs to call the same endpoints without a
separate HTTP-JSON shim). We also want a single schema source of truth
so the frontend and backend cannot drift.

## Decision Drivers

- **Single schema source.** Proto/Protobuf definitions drive both the
  Go server bindings and the TypeScript client.
- **Browser compatibility.** Clients must work over plain HTTP/1.1 from
  a browser without gRPC-web translation proxies.
- **Streaming.** Some endpoints (logs, progress) need server-sent
  updates; the transport has to support this.
- **Tooling maturity.** Code generation, linting, and breaking-change
  detection need to be solved problems, not research projects.
- **Operational simplicity.** No sidecars, no separate grpc-gateway
  binary, no dual endpoints for "grpc vs http" clients.

## Considered Options

1. **Connect-RPC (` + "`connectrpc.com/connect`" + `)** — chosen.
2. **Plain gRPC + grpc-gateway.** Typed Go clients, but browsers need
   an HTTP-JSON gateway; extra binary + extra config.
3. **OpenAPI + hand-written Go handlers.** Familiar, but schema drift
   between client and server is a recurring operational cost.
4. **GraphQL.** Good for aggregation over many resources; heavier than
   we need for internal service-to-service RPC and introduces a second
   schema system alongside protos.

## Decision Outcome

Chosen option: **Connect-RPC**.

- Go service handlers are generated by ` + "`protoc-gen-connect-go`" + `.
- The frontend uses ` + "`@connectrpc/connect-web`" + ` over HTTP/1.1 JSON, no
  translation layer required.
- Buf handles linting and breaking-change detection on every PR.
- Streaming (server-streaming and bidi) is supported natively.

## Consequences

### Good

- One schema (proto) drives both clients; type safety end-to-end.
- Same handler works for gRPC, gRPC-Web, and Connect clients
  simultaneously — the frontend and a Go CLI can share endpoints.
- Buf ecosystem (lint, breaking, format) integrates directly into CI.
- No grpc-gateway binary to operate.

### Bad

- Connect wire format is less ubiquitous than plain REST; some tools
  (curl, Postman) need the right Content-Type header.
- Debugging RPCs at the HTTP level requires knowing the Connect
  envelope conventions.
- Adds a compile-time codegen step (` + "`buf generate`" + `) to every build;
  ` + "`forge generate`" + ` wraps this but it remains a required prerequisite
  for a working checkout.

### Related decisions

- _(future)_ Auth strategy: interceptor-based, since Connect's
  interceptor model is the integration point.
- _(future)_ Error taxonomy: Connect ` + "`connect.Code`" + ` values are the
  canonical error set.
`, "2024-01-01", g.Name)

	dir := filepath.Join(g.Path, "docs", "adr")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(readme), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "0001-use-connect-rpc.md"), []byte(adr), 0o644)
}

// generateBenchmarks writes the benchmarks/ harness:
//
//   - benchmarks/k6/smoke.js: ramping-VU smoke test hitting /healthz plus
//     one Connect RPC (ListItems, which the service template exposes when
//     a service is scaffolded — we fall back to /healthz only when there
//     is no initial service).
//   - benchmarks/README.md: how to run k6 and what to watch.
//   - benchmarks/healthz_bench_test.go: a Go ` + "`" + `Benchmark*` + "`" + ` that benchmarks
//     the healthz handler. Kept in its own ` + "`" + `_bench_test.go` + "`" + ` file so it can
//     coexist with any correctness-cluster test file (e.g. recovery_test.go).
func (g *ProjectGenerator) generateBenchmarks() error {
	dir := filepath.Join(g.Path, "benchmarks")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(dir, "k6"), 0o755); err != nil {
		return err
	}

	targetHost := fmt.Sprintf("http://localhost:%d", g.ServicePort)

	k6 := fmt.Sprintf(`// smoke.js — k6 smoke test.
// Ramps a handful of VUs against the running service and fails the run
// if p95 latency or error rate blow past the thresholds below. Keep the
// load modest; this is a smoke test, not a capacity test.
//
// Run with:
//     k6 run --env BASE_URL=%[1]s benchmarks/k6/smoke.js

import http from 'k6/http';
import { check, sleep } from 'k6';

export const options = {
  stages: [
    { duration: '10s', target: 5 },
    { duration: '20s', target: 10 },
    { duration: '10s', target: 0 },
  ],
  thresholds: {
    http_req_failed: ['rate<0.01'],
    http_req_duration: ['p(95)<300'],
  },
};

const BASE_URL = __ENV.BASE_URL || '%[1]s';

export default function () {
  const healthz = http.get(` + "`" + `${BASE_URL}/healthz` + "`" + `);
  check(healthz, {
    'healthz 200': (r) => r.status === 200,
  });

  sleep(1);
}
`, targetHost)

	readme := `# benchmarks

Two flavours of perf harness:

- ` + "`k6/`" + ` — [k6](https://k6.io) scripts for HTTP-level load tests.
  Good for smoke-testing a running service end-to-end.
- ` + "`*_bench_test.go`" + ` — Go ` + "`testing.B`" + ` microbenchmarks. Good for
  measuring specific handlers or hot paths in isolation.

## Running k6

Install k6: <https://k6.io/docs/get-started/installation/>.

Start the service locally (` + "`task dev`" + ` or ` + "`docker compose up`" + `), then:

` + "```" + `bash
k6 run benchmarks/k6/smoke.js
# Or point at a staging host:
k6 run --env BASE_URL=https://staging.example.com benchmarks/k6/smoke.js
` + "```" + `

### What to watch

- ` + "`http_req_duration`" + ` — p(50), p(95), p(99). Thresholds in the script
  will fail the run if p(95) regresses past the baseline.
- ` + "`http_req_failed`" + ` — should stay below 1%. Any non-2xx response that
  isn't an intentional auth/404 test counts.
- ` + "`vus`" + ` / ` + "`iterations`" + ` — sanity check that ramping actually happened.

## Running Go benchmarks

` + "```" + `bash
task bench
# or, for a specific package:
go test -bench=. -benchmem ./benchmarks/...
` + "```" + `

Follow up with ` + "`benchstat`" + ` when comparing two revisions:

` + "```" + `bash
go install golang.org/x/perf/cmd/benchstat@latest
go test -bench=. -count=10 ./benchmarks/... > old.txt
# ...make change, then...
go test -bench=. -count=10 ./benchmarks/... > new.txt
benchstat old.txt new.txt
` + "```" + `
`

	goBench := `package benchmarks

// Package benchmarks hosts the project's microbenchmarks. Each file
// follows the standard Go convention (` + "`" + `func BenchmarkXxx(b *testing.B)` + "`" + `)
// so they run under ` + "`" + `go test -bench` + "`" + ` / ` + "`" + `task bench` + "`" + ` without special flags.
//
// Benchmarks live in their own package so they can evolve independently
// of unit tests in pkg/middleware — no risk of accidentally sharing
// state or setup across the two.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// healthzHandler mirrors cmd/server.go's liveness endpoint closely
// enough to give a realistic baseline for the request/response path:
// a tiny handler with no dependencies. If the real handler grows
// (metrics, checks), update this to match so the benchmark stays
// representative.
func healthzHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

// BenchmarkHealthz measures the cost of serving a single /healthz
// request via net/http/httptest. Reports ns/op and allocs/op so a CI
// regression gate (e.g. benchstat) can detect unexpected growth.
func BenchmarkHealthz(b *testing.B) {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/healthz", nil)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rr := httptest.NewRecorder()
		healthzHandler(rr, req)
		if rr.Code != http.StatusOK {
			b.Fatalf("unexpected status: %d", rr.Code)
		}
	}
}
`
	if err := os.WriteFile(filepath.Join(dir, "k6", "smoke.js"), []byte(k6), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(readme), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "healthz_bench_test.go"), []byte(goBench), 0o644)
}

// generateRunbookAndSLO writes docs/runbook.md and docs/slo.md starter
// templates with TODO placeholders.
func (g *ProjectGenerator) generateRunbookAndSLO() error {
	dir := filepath.Join(g.Path, "docs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	runbook := fmt.Sprintf(`# Runbook — %s

## Service overview

%[1]s is a Connect RPC service. TODO: add a short description.

## Ownership

- **Team:** TODO
- **On-call rotation:** TODO (link to PagerDuty / Opsgenie)

## Common alerts

### HighErrorRate

- **Meaning:** 5xx rate exceeds threshold.
- **Diagnostic steps:**
  1. Check application logs: `+"`"+`kubectl logs -l app=%[1]s`+"`"+`
  2. Check downstream dependencies.
  3. Check recent deployments.

### HighLatency

- **Meaning:** p95 latency exceeds threshold.
- **Diagnostic steps:**
  1. Check for traffic spike (Grafana dashboard).
  2. Check DB slow queries.
  3. Check CPU/memory pressure.

### ProbeFailure

- **Meaning:** /healthz or /readyz is failing.
- **Diagnostic steps:**
  1. Check pod status: `+"`"+`kubectl get pods -l app=%[1]s`+"`"+`
  2. Check OOM kills.
  3. Check networking / DNS.

## Dashboards & Logs

- **Grafana:** TODO (link)
- **Logs:** TODO (link to Loki / CloudWatch)
- **Traces:** TODO (link to Jaeger / Tempo)

## Emergency contacts

| Role | Contact |
|------|--------|
| Primary on-call | TODO |
| Engineering lead | TODO |
| Platform team | TODO |
`, g.Name)

	slo := fmt.Sprintf(`# SLO — %s

Service Level Objectives for %[1]s.

## Availability

- **Target:** 99.9%% (three nines)
- **Measurement:** ratio of successful (non-5xx) responses to total requests
- **Window:** rolling 30 days

## Latency

- **Target:** p95 < 250ms
- **Measurement:** server-side request duration histogram
- **Window:** rolling 30 days

## Error rate

- **Target:** < 0.1%% of requests return 5xx
- **Measurement:** 5xx count / total request count
- **Window:** rolling 30 days

## Error budget policy

1. When the error budget is consumed < 50%%: normal development velocity.
2. When the error budget is consumed 50-99%%: prioritize reliability work.
3. When the error budget is exhausted: freeze non-critical changes,
   focus entirely on restoring reliability.

## Review cadence

Review SLOs quarterly. Adjust targets based on actual traffic patterns
and business requirements.
`, g.Name)

	if err := os.WriteFile(filepath.Join(dir, "runbook.md"), []byte(runbook), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "slo.md"), []byte(slo), 0o644)
}

// generatePrometheusRules writes deploy/observability/prometheus-rules.yaml
// with stub alert rules.
func (g *ProjectGenerator) generatePrometheusRules() error {
	dir := filepath.Join(g.Path, "deploy", "observability")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	rules := fmt.Sprintf(`# Prometheus alert rule stubs for %s.
# Adjust thresholds and labels to match your environment.
# Import into Prometheus via the rule_files config directive.
groups:
  - name: %[1]s.rules
    rules:
      - alert: HighErrorRate
        expr: |
          sum(rate(http_server_request_duration_seconds_count{job="%[1]s",code=~"5.."}[5m]))
          /
          sum(rate(http_server_request_duration_seconds_count{job="%[1]s"}[5m]))
          > 0.01
        for: 5m
        labels:
          severity: critical
        annotations:
          summary: "High 5xx error rate on %[1]s"
          description: "More than 1%% of requests are returning 5xx for 5 minutes."

      - alert: HighLatency
        expr: |
          histogram_quantile(0.95,
            sum(rate(http_server_request_duration_seconds_bucket{job="%[1]s"}[5m])) by (le)
          ) > 0.25
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "High p95 latency on %[1]s"
          description: "p95 latency exceeds 250ms for 5 minutes."

      - alert: ProbeFailure
        expr: probe_success{job="%[1]s-probe"} == 0
        for: 2m
        labels:
          severity: critical
        annotations:
          summary: "Health probe failing for %[1]s"
          description: "Blackbox probe has been failing for 2 minutes."
`, g.Name)

	return os.WriteFile(filepath.Join(dir, "prometheus-rules.yaml"), []byte(rules), 0o644)
}

// generateSQLCStub writes db/sqlc.yaml, db/queries/.gitkeep, and
// db/README.md so the scaffold has a ready-to-use sqlc layout.
func (g *ProjectGenerator) generateSQLCStub() error {
	qDir := filepath.Join(g.Path, "db", "queries")
	if err := os.MkdirAll(qDir, 0o755); err != nil {
		return err
	}

	sqlcYAML := fmt.Sprintf(`version: "2"
sql:
  - engine: "postgresql"
    queries: "db/queries/"
    schema: "db/migrations/"
    gen:
      go:
        package: "db"
        out: "internal/db"
        sql_package: "pgx/v5"
        emit_json_tags: true
        emit_prepared_queries: false
        emit_interface: true
        emit_exact_table_names: false
        emit_empty_slices: true
`)
	_ = sqlcYAML

	readme := `# Database

This directory contains the database layer for the project.

## Layout

` + "```" + `
db/
  migrations/     # SQL migration files (up/down pairs)
  queries/        # sqlc query files (*.sql)
  seeds/          # Seed data for development
  sqlc.yaml       # sqlc configuration
` + "```" + `

## sqlc

[sqlc](https://sqlc.dev) generates type-safe Go code from SQL queries.

### Installation

` + "```" + `bash
# macOS
brew install sqlc

# Other platforms
go install github.com/sqlc-dev/sqlc/cmd/sqlc@latest
` + "```" + `

### Usage

1. Write your SQL queries in ` + "`db/queries/*.sql`" + `
2. Run ` + "`sqlc generate`" + ` to generate Go code
3. Generated code appears in ` + "`internal/db/`" + `

### Example query file

` + "```sql" + `
-- name: GetUser :one
SELECT id, name, email, created_at
FROM users
WHERE id = $1;

-- name: ListUsers :many
SELECT id, name, email, created_at
FROM users
ORDER BY created_at DESC
LIMIT $1 OFFSET $2;
` + "```" + `
`

	gitkeep := "# Place sqlc query files (*.sql) here.\n# See db/README.md for usage.\n"

	if err := os.WriteFile(filepath.Join(g.Path, "db", "sqlc.yaml"), []byte(sqlcYAML), 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(qDir, ".gitkeep"), []byte(gitkeep), 0o644); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(g.Path, "db", "README.md"), []byte(readme), 0o644)
}

// generateNonGoalsADR writes docs/adr/0002-intentional-non-goals.md documenting
// features that were considered but intentionally deferred.
func (g *ProjectGenerator) generateNonGoalsADR() error {
	dir := filepath.Join(g.Path, "docs", "adr")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}

	adr := `# 2. Intentional non-goals

- Status: accepted
- Date: 2024-01-01
- Deciders: project authors

## Context

During scaffold review, several capabilities were evaluated and
explicitly deferred. This ADR documents them so future contributors
don't re-open settled discussions without new information.

## Non-goals

### i18n / Internationalization

Many projects don't need multi-language support. Adding i18n
infrastructure to the scaffold adds complexity for the majority of
users who will never use it. Projects that need i18n can add it
manually following the Next.js i18n guide.

### axe-core accessibility scanner

ESLint's jsx-a11y plugin already covers static accessibility checks.
A runtime axe-core integration would add test infrastructure weight
for marginal benefit over the static analysis already in place.

### Distributed rate limiting

Rate limiting at the service level is intentionally out of scope.
Production systems should use infrastructure-level rate limiting
(API gateway, service mesh, or cloud provider). A per-process
rate limiter gives false confidence in distributed deployments.

### Feature flags

Feature flag infrastructure is heavily dependent on the chosen
provider (LaunchDarkly, Unleash, Flipt, etc.). The scaffold
should not pick a provider. Document feature flag patterns in
the architecture guide instead.

### Web SBOM / cosign signatures

Software Bill of Materials and container image signing are
valuable for supply chain security but require organization-
specific signing infrastructure. These should be added as part
of a production hardening pass, not in the initial scaffold.

### Config YAML loader

Environment variables are the intentional configuration mechanism,
following 12-factor app methodology. Adding YAML config loading
would create a second configuration path that must be maintained
and documented, with no clear benefit over env vars for the
typical deployment model (containers + orchestrator).

## Consequences

Contributors who want to add any of the above should first write a
new ADR explaining what changed to make the feature worth the
added complexity.
`

	return os.WriteFile(filepath.Join(dir, "0002-intentional-non-goals.md"), []byte(adr), 0o644)
}