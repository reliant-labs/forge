#!/usr/bin/env bash
# End-to-end smoke for the Windows CI job: scaffold the smallest project and
# render its dev env. `forge env render` is the proof that the purego KCL
# bridge works — it loads KCL's native library (kcl.dll on Windows) that forge
# extracts on first use. Needs `forge` on PATH. bash is only the CI driver;
# forge itself does not depend on it.
#
# Runs anywhere with a cgo-free forge, so the exact sequence can be dry-run
# locally: CGO_ENABLED=0 go install ./cmd/forge && scripts/ci-windows-smoke.sh
set -euo pipefail

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cd "$work"

# --skip-tools: protoc plugins are not needed to scaffold or render, and
# --service api avoids frontends (no npm). --link-forge: the forge under test
# is built from this checkout, so the scaffold must compile forge from it.
forge project new smoke --kind service --mod github.com/e2e/smoke --service api --skip-tools --link-forge
cd smoke
forge env render dev

# Windows only: render must have extracted the library into the default
# location (README "Install" documents it). KCL_LIB_HOME is deliberately unset
# so this checks the default path.
case "$(uname -s)" in
  MINGW* | MSYS* | CYGWIN*)
    if [[ -n "${KCL_LIB_HOME:-}" ]]; then
      echo "::error::KCL_LIB_HOME is set; this smoke must exercise the default %TEMP%\\kcl path"
      exit 1
    fi
    if [[ ! -f "$TEMP/kcl/kcl.dll" ]]; then
      echo "::error::kcl.dll was not extracted to %TEMP%\\kcl"
      ls -la "$TEMP/kcl" || true
      exit 1
    fi
    echo "kcl.dll present at $TEMP/kcl/kcl.dll"
    ;;
esac
echo "windows smoke ok"
