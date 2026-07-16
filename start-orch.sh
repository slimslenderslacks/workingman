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

  # sbx exposes an SSH agent inside every sandbox at /run/ssh-agent.sock,
  # proxied to whatever agent the *sandboxd daemon* was launched with — NOT the
  # sbx client's and NOT a bind-mounted host socket (a macOS unix socket has no
  # listener reachable across the Docker VM boundary). If sandboxd is already
  # running under launchd's empty system agent, the commit agent's ssh-keygen
  # sees no key and signing fails. Restart it so it adopts the 1Password agent
  # we just exported. This stops running sandboxes; they are recreated on
  # demand, so it's safe at orchestrator startup.
  if command -v sbx >/dev/null 2>&1; then
    sbx daemon stop  >/dev/null 2>&1 || true
    sbx daemon start -d >/dev/null 2>&1 || true
  fi
fi

"$HERE/orch" \
  --root ~/orch \
  --audit-log ~/orch/audit.log \
  --acp-kit "${ACP_KIT:-$HERE/../acp-kit}" \
  --acp-wrapper "$HERE/acp-wrapper"
