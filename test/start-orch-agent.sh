#!/usr/bin/env bash
# Exercises start-orch.sh --check-agent with a stub `op` and a throwaway key.
set -euo pipefail
HERE="$(cd "$(dirname "$0")/.." && pwd)"
T="$(mktemp -d)"; trap 'rm -rf "$T"' EXIT
mkdir "$T/bin"
ssh-keygen -q -t ed25519 -N '' -f "$T/key" -C test
cat > "$T/bin/op" <<STUB
#!/usr/bin/env bash
[ "\$1" = read ] || exit 2
cat "$T/key"
STUB
chmod +x "$T/bin/op"
fail() { echo "FAIL: $*" >&2; exit 1; }

out="$(PATH="$T/bin:$PATH" TMPDIR="$T" ORCH_SSH_KEY_REF=op://x/y/z "$HERE/start-orch.sh" --check-agent)"
sock="$(sed -n 's/^SSH_AUTH_SOCK=//p' <<<"$out")"
pid="$(sed -n 's/^SSH_AGENT_PID=//p' <<<"$out")"
[ -n "$sock" ] && [ -n "$pid" ] || fail "no sock/pid reported: $out"
case "$sock" in "$T"/orch-ssh.*/agent.sock) ;; *) fail "SSH_AUTH_SOCK not the private agent: $sock" ;; esac
grep -q 'ED25519' <<<"$out" || fail "key not loaded"
grep -q 'PRIVATE KEY' <<<"$out" && fail "private key leaked"
! kill -0 "$pid" 2>/dev/null || fail "agent $pid still running"
[ ! -e "$sock" ] && [ ! -e "$(dirname "$sock")" ] || fail "socket dir not removed"

# Failure path: op fails -> startup aborts, nothing left behind.
printf '#!/bin/sh\nexit 1\n' > "$T/bin/op"
if PATH="$T/bin:$PATH" TMPDIR="$T" "$HERE/start-orch.sh" --check-agent >/dev/null 2>&1; then fail "expected abort when op fails"; fi
[ -z "$(ls -d "$T"/orch-ssh.* 2>/dev/null)" ] || fail "leftover agent dir after failure"
echo "PASS"
