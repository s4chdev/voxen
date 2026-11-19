#!/usr/bin/env bash
# voxen development helper.
#
#   scripts/dev-check.sh          format, vet, unit + integration tests, cross build
#   scripts/dev-check.sh --quick  skip the cross build
#
# Exits non-zero on the first failure so it is safe to use as a pre-push hook.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

quick=0
if [[ "${1:-}" == "--quick" ]]; then
  quick=1
fi

step() { printf '\n\033[1m==> %s\033[0m\n' "$1"; }

command -v go >/dev/null 2>&1 || { echo "go not found in PATH" >&2; exit 1; }

step "gofmt"
unformatted=$(gofmt -l . | grep -v '^$' || true)
if [[ -n "$unformatted" ]]; then
  echo "not gofmt'd:" >&2
  echo "$unformatted" >&2
  exit 1
fi

step "go vet"
go vet ./...

step "go mod verify"
go mod verify

step "unit tests (race)"
go test -race -short ./internal/...

step "security acceptance tests"
go test -race ./tests/security/...

step "integration tests"
go test -timeout 15m ./tests/integration/...

if [[ $quick -eq 0 ]]; then
  step "cross build (six targets)"
  make cross
fi

step "done"
