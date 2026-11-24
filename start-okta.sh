#!/usr/bin/env bash
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"

export WORKINGMAN_EXTRA_CA_CERTS=~/.workingman/extra-ca.pem

exec "$HERE/start.sh" "$@"
