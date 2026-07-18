#!/usr/bin/env bash
# check-signing.sh — diagnose (and, on confirmation, fix) commit signing inside
# sbx sandboxes.
#
# Background: the orch commit agent SSH-signs commits using the key held by the
# 1Password SSH agent. sbx exposes that key inside every sandbox at
# /run/ssh-agent.sock, but only by proxying whatever agent the *sandboxd daemon*
# was launched with. start-orch.sh points sandboxd at the 1Password agent — but
# only at orch startup. If Docker Desktop later restarts sandboxd (an update, a
# crash, a sleep/wake that bounces the VM), it comes back bound to launchd's
# empty system agent, and every sandbox loses the signing key for the rest of
# the session. This script detects that and offers to re-point sandboxd.
#
# It distinguishes the two failure modes:
#   - 1Password locked/empty  -> unlock 1Password (no sandboxd restart helps).
#   - sandboxd forwarding an empty agent -> restart sandboxd with the 1P agent.
#
# Usage:
#   ./check-signing.sh            # detect; prompt before restarting sandboxd
#   ./check-signing.sh --check    # detect only; never restart (exit 3 if broken)
#   ./check-signing.sh --yes      # detect; restart without prompting (for cron)
set -euo pipefail

OP_AGENT="$HOME/Library/Group Containers/2BUA8C4S2C.com.1password/t/agent.sock"
PROBE="signing-doctor-$$"
ASSUME_YES=0
CHECK_ONLY=0

for arg in "$@"; do
  case "$arg" in
    -y|--yes)   ASSUME_YES=1 ;;
    --check)    CHECK_ONLY=1 ;;
    -h|--help)  sed -n '2,26p' "$0"; exit 0 ;;
    *) echo "unknown argument: $arg" >&2; exit 2 ;;
  esac
done

log()  { printf '%s\n' "$*"; }
fail() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

# --- fix: restart sandboxd bound to the 1Password agent -------------------
# Guarded by a confirmation prompt unless --yes was passed. Stops running
# sandboxes (they are recreated on demand), so we ask first.
restart_sandboxd() {
  if [ "$CHECK_ONLY" -eq 1 ]; then
    log "  (--check: not restarting; re-run without --check to fix)"
    return 1
  fi
  if [ "$ASSUME_YES" -ne 1 ]; then
    if [ ! -t 0 ]; then
      fail "signing is broken but stdin is not a TTY; re-run with --yes to allow the sandboxd restart"
    fi
    printf 'Restart sandboxd with the 1Password agent? This stops running sandboxes (recreated on demand) [y/N] '
    read -r reply
    case "$reply" in
      y|Y|yes|YES) ;;
      *) log "aborted; sandboxd left as-is"; return 1 ;;
    esac
  fi
  log "restarting sandboxd bound to the 1Password agent..."
  sbx daemon stop >/dev/null 2>&1 || true
  SSH_AUTH_SOCK="$OP_AGENT" sbx daemon start -d >/dev/null 2>&1 || fail "sbx daemon start failed"
  return 0
}

# --- preflight ------------------------------------------------------------
command -v sbx >/dev/null 2>&1 || fail "sbx not found on PATH"
[ -S "$OP_AGENT" ] || fail "1Password SSH agent socket not found at $OP_AGENT — is 1Password running with SSH agent enabled?"

# Does the 1Password agent itself hold the key right now? ssh-add -l exits
# 0 (has identities), 1 (none — locked/empty), or 2 (cannot connect).
if SSH_AUTH_SOCK="$OP_AGENT" ssh-add -l >/dev/null 2>&1; then
  :
else
  case $? in
    1) fail "1Password agent has NO identities — it is likely LOCKED. Unlock 1Password; a sandboxd restart will not help." ;;
    *) fail "cannot connect to the 1Password SSH agent at $OP_AGENT." ;;
  esac
fi
log "1Password agent: OK (holds the signing key)"

# --- probe the agent as a sandbox sees it ---------------------------------
# The truth we care about is what ssh-keygen sees inside a sandbox, i.e. what
# sandboxd's forwarder serves at /run/ssh-agent.sock. Probe with a throwaway
# sandbox so the check reflects the real commit-agent path. Always cleaned up.
PROBE_WS="$(mktemp -d)"
cleanup() {
  sbx rm --force "$PROBE" >/dev/null 2>&1 || true
  rmdir "$PROBE_WS" 2>/dev/null || true
}
trap cleanup EXIT

probe_forwarded() {
  # echoes "ok" if the forwarded agent has the key, "empty" if not, "createfail"
  # if the sandbox couldn't even be created (sandboxd itself unhealthy).
  if ! sbx create claude --name "$PROBE" "$PROBE_WS" >/dev/null 2>&1; then
    echo createfail; return
  fi
  local out
  out="$(sbx exec "$PROBE" -- ssh-add -l 2>&1 || true)"
  if printf '%s' "$out" | grep -q "SHA256:"; then echo ok; else echo empty; fi
}

log "probing the agent forwarded into sandboxes (creating a throwaway sandbox)..."
result="$(probe_forwarded)"

case "$result" in
  ok)
    log "sandbox agent: OK — signing key is reachable inside sandboxes. Nothing to fix."
    exit 0
    ;;
  empty)
    log "sandbox agent: BROKEN — the 1Password agent has the key, but sandboxes see an EMPTY agent."
    log "  => sandboxd is forwarding the wrong agent (it was likely restarted since orch launched)."
    ;;
  createfail)
    log "sandbox agent: could not create a probe sandbox — sandboxd may be unhealthy."
    ;;
esac

# Broken. Clean up the (possibly created) probe before touching the daemon so
# the restart isn't fighting a live sandbox, then attempt the fix.
cleanup
trap - EXIT

if ! restart_sandboxd; then
  exit 3
fi

# --- confirm the fix worked ----------------------------------------------
PROBE_WS="$(mktemp -d)"
trap cleanup EXIT
log "re-probing after restart..."
if [ "$(probe_forwarded)" = "ok" ]; then
  log "FIXED — signing key is now reachable inside sandboxes."
  exit 0
fi
fail "still broken after restart — check that sandboxd inherited SSH_AUTH_SOCK=$OP_AGENT"
