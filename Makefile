# Forge top-level Makefile.
#
# Most day-to-day work goes through `go test ./...` and `go build`. This
# Makefile collects the few orchestrated checks that don't fit there —
# notably the real-k3d ingress smoke that drives a freshly-scaffolded
# project through `forge cluster up` + Traefik + curl.

.PHONY: e2e-ingress test build dev help

# DevForgeRoot is the buildinfo var the dev-install target stamps with the
# LOCAL forge checkout so `forge project new` can auto-bridge fresh projects
# to this source. Discovered from the builder's own tree at build time —
# nothing machine-specific is committed. Released/CI builds omit the ldflag,
# leaving it empty (no go.work is ever written).
DEV_FORGE_ROOT_LDFLAG := -X github.com/reliant-labs/forge/internal/buildinfo.DevForgeRoot=$(shell git rev-parse --show-toplevel)

help:
	@echo "Targets:"
	@echo "  build         go build ./cmd/forge -> ./forge"
	@echo "  dev           go install forge with DevForgeRoot stamped (contributor loop)"
	@echo "  test          go test ./..."
	@echo "  e2e-ingress   real k3d Gateway API ingress smoke test"

build:
	go build -o forge ./cmd/forge

# Contributor dev loop: install forge into $GOBIN (add $HOME/go/bin to PATH)
# with its own source root stamped in. Then `forge project new` writes a
# gitignored go.work bridging each new project to THIS checkout's forge/pkg,
# so scaffolds build against your in-development forge with no manual
# `go mod edit -replace`. Use plain `make build` / a release install for a
# binary that pins the published forge/pkg instead.
dev:
	go install -ldflags "$(DEV_FORGE_ROOT_LDFLAG)" ./cmd/forge

test:
	go test ./...

# Real-k3d smoke test for the Gateway API ingress story.
# Requires k3d, kubectl, curl, go, kcl, docker on PATH. See the
# script header for details and exit codes.
e2e-ingress:
	bash scripts/e2e-ingress.sh
