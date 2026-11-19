#!/usr/bin/env bash
# Run an isolated two-profile voxen session for manual testing.
#
#   scripts/demo.sh
#
# Creates two profiles under a temporary directory, initialises both identities,
# prints each fingerprint, and leaves one daemon running in the foreground. Use
# Ctrl-C to stop; the temporary directory is removed on exit.
#
# This is a phase-1 demo: it exercises identity, the daemon, IPC and the CLI. It
# does not place a call, because audio and media arrive in phases 2 and 3.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")/.."

if ! command -v go >/dev/null 2>&1; then
  echo "go not found in PATH" >&2
  exit 1
fi

workdir=$(mktemp -d "${TMPDIR:-/tmp}/voxen-demo.XXXXXX")
cleanup() {
  if [[ -f "$workdir/alice/voxen.pid" ]]; then
    VOXEN_CONFIG_DIR="$workdir/alice/config" \
    VOXEN_STATE_DIR="$workdir/alice/state" \
    VOXEN_RUNTIME_DIR="$workdir/alice/run" \
      ./bin/voxen stop >/dev/null 2>&1 || true
  fi
  rm -rf "$workdir"
}
trap cleanup EXIT INT TERM

echo "building..."
go build -trimpath -o bin/voxen ./cmd/voxen
go build -trimpath -o bin/voxend ./cmd/voxend

profile() {
  local name=$1
  mkdir -p "$workdir/$name"/{config,state,run}
  VOXEN_CONFIG_DIR="$workdir/$name/config" \
  VOXEN_STATE_DIR="$workdir/$name/state" \
  VOXEN_RUNTIME_DIR="$workdir/$name/run" \
    ./bin/voxen init --name "$name" >"$workdir/$name/init.log"
}

echo "initialising alice and bob in $workdir"
profile alice
profile bob

alice_fp=$(grep -A1 'your fingerprint' "$workdir/alice/init.log" | tail -1 | xargs)
bob_fp=$(grep -A1 'your fingerprint' "$workdir/bob/init.log" | tail -1 | xargs)

printf '\nalice: %s\nbob:   %s\n\n' "$alice_fp" "$bob_fp"

echo "starting alice's daemon in the foreground (Ctrl-C to stop)"
VOXEN_CONFIG_DIR="$workdir/alice/config" \
VOXEN_STATE_DIR="$workdir/alice/state" \
VOXEN_RUNTIME_DIR="$workdir/alice/run" \
  ./bin/voxen start --foreground
