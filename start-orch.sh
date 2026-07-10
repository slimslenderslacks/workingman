#!/usr/bin/env bash
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"

# 1Password's SSH agent holds the commit-signing key. A GUI/login shell's
# default SSH_AUTH_SOCK points at the macOS system agent, which lacks that key,
# so orch (and the sandboxes it forwards the agent into) couldn't sign commits.
# Point it at the 1Password agent when that socket exists. orch defaults this
# too, but exporting it here also covers running the binary directly.
OP_AGENT="$HOME/Library/Group Containers/2BUA8C4S2C.com.1password/t/agent.sock"
if [ -S "$OP_AGENT" ]; then
  export SSH_AUTH_SOCK="$OP_AGENT"
fi

"$HERE/orch" \
  --root ~/orch \
  --audit-log ~/orch/audit.log \
  --acp-kit "${ACP_KIT:-$HERE/../acp-kit}" \
  --acp-wrapper "$HERE/acp-wrapper"
