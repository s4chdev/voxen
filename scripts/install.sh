#!/bin/sh
# voxen install helper.
#
# Run this from inside an extracted release archive:
#
#   ./install.sh                 install for the current user, no root needed
#   sudo ./install.sh            install system-wide to /usr/local/bin
#   ./install.sh --service       also install the background supervisor
#
# It is a thin wrapper around "voxen install", which does the actual work. The
# wrapper exists so a user who just extracted an archive has an obvious next step
# without reading documentation.
set -eu

here=$(cd "$(dirname "$0")" && pwd)
cd "$here"

if [ ! -x ./voxen ]; then
	echo "error: ./voxen not found in $here" >&2
	echo "       run this script from inside an extracted release archive" >&2
	exit 1
fi

# Pass every argument through. The binary decides prefix, service and force; this
# script only checks the environment and explains what will happen.
case "${1:-}" in
	-h|--help|help)
		sed -n '2,12p' "$0" | sed 's/^# \{0,1\}//'
		exit 0
		;;
esac

if [ "$(id -u 2>/dev/null || echo 1)" -eq 0 ]; then
	echo "Installing system-wide (running as root)."
else
	echo "Installing for the current user."
fi

# exec so the exit code is the binary's, and so a signal reaches it directly.
exec ./voxen install "$@"
