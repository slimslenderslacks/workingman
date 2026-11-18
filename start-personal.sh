#!/usr/bin/env bash
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"

# start.sh with the signing key read from the "workingman-personal" vault
# instead of the default "workingman-okta". The 1Password service account
# must have read access to that vault. Arguments pass through to start.sh.
export ORCH_SSH_KEY_REF="${ORCH_SSH_KEY_REF:-op://workingman-personal/SSH github/private key?ssh-format=openssh}"

exec "$HERE/start.sh" "$@"
