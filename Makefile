# Forge top-level Makefile.
#
# Most day-to-day work goes through `go test ./...` and `go build`. This
# Makefile collects the few orchestrated checks that don't fit there —
# notably the real-k3d ingress smoke that drives a freshly-scaffolded
# project through `forge dev cluster up` + Traefik + curl.

.PHONY: e2e-ingress test build help

help:
	@echo "Targets:"
	@echo "  build         go build ./cmd/forge -> ./forge"
	@echo "  test          go test ./..."
	@echo "  e2e-ingress   real k3d Gateway API ingress smoke test"

build:
	go build -o forge ./cmd/forge

test:
	go test ./...

# Real-k3d smoke test for the Gateway API ingress story.
# Requires k3d, kubectl, curl, go, kcl, docker on PATH. See the
# script header for details and exit codes.
e2e-ingress:
	bash scripts/e2e-ingress.sh
