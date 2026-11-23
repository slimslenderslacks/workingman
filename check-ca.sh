#!/usr/bin/env bash
# check-ca.sh — prove that sandboxes on this laptop work with the extra CA
# (the TLS-inspecting VPN's root, docs/sandbox-custom-ca.md).
#
# On a VPN that re-signs github.com and api.anthropic.com with a corporate CA,
# git and the agent fail inside sbx sandboxes until that CA is installed into
# the sandbox's trust store. This script shows that the configured CA is what
# makes them work:
#
#   1. prints each cert in the PEM bundle (subject, issuer, notAfter, SHA-256)
#      and flags expired ones loudly;
#   2. creates a throwaway probe sandbox the way the daemon does (acp-kit kit
#      and image when available; same defaults as sbx-create.sh);
#   3. runs git ls-remote / curl against github.com and api.anthropic.com
#      WITHOUT the CA;
#   4. installs the CA with the daemon's own idempotent step (the
#      WORKINGMAN_EXTRA_CA_EOF heredoc in sbx-create.sh, byte-identical to
#      sbx.ExtraCAScript), twice, to check the second run is a no-op;
#   5. runs the same probes WITH the CA;
#   6. if sbx can stop sandboxes, stops the probe and re-runs the probes
#      without re-installing (informational: the daemon re-installs on every
#      launch anyway);
#   7. prints a summary and removes the probe sandbox (trap).
#
# Exits 0 only when every probe passes with the CA and at least one cert in
# the bundle is unexpired.
#
# Usage:
#   ./check-ca.sh [PEM]     # PEM defaults to $WORKINGMAN_EXTRA_CA_CERTS
#   task check:ca [-- PEM]
#
# Environment (same defaults as sbx-create.sh):
#   ACP_IMAGE   template image (default slimslenderslacks/claude-code-acp:0.90.0; "" for stock)
#   ACP_KIT     acp-kit mixin (default ../acp-kit, else ~/dev/repos/slimslenderslacks/acp-kit; "" for none)
set -euo pipefail

usage() { sed -n '2,/^set -euo/p' "$0" | sed '$d; s/^# \{0,1\}//'; }

PEM=""
for arg in "$@"; do
  case "$arg" in
    -h|--help) usage; exit 0 ;;
    -*) echo "unknown option: $arg (see --help)" >&2; exit 2 ;;
    *) [ -z "$PEM" ] || { echo "only one PEM path may be given" >&2; exit 2; }; PEM="$arg" ;;
  esac
done
PEM="${PEM:-${WORKINGMAN_EXTRA_CA_CERTS:-}}"

log()  { printf '%s\n' "$*"; }
fail() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

HERE="$(cd "$(dirname "$0")" && pwd)"
IMAGE="${ACP_IMAGE-slimslenderslacks/claude-code-acp:0.90.0}"
if [ -f "$HERE/../acp-kit/spec.yaml" ]; then DEFAULT_KIT="$(cd "$HERE/../acp-kit" && pwd)"
else DEFAULT_KIT="$HOME/dev/repos/slimslenderslacks/acp-kit"; fi
KIT="${ACP_KIT-$DEFAULT_KIT}"
[ -z "$KIT" ] || [ -e "$KIT" ] || { log "note: acp-kit not found at $KIT; probing without --kit"; KIT=""; }

GIT_URL="https://github.com/slimslenderslacks/workingman"
PROBES=(git-github curl-github curl-anthropic)
PROBE="ca-probe-$$"

# --- preflight -------------------------------------------------------------
for bin in sbx openssl base64; do command -v "$bin" >/dev/null 2>&1 || fail "'$bin' not found on PATH"; done
[ -n "$PEM" ] || fail "no PEM given and \$WORKINGMAN_EXTRA_CA_CERTS is unset (usage: ./check-ca.sh /path/to/ca.pem)"
[ -r "$PEM" ] || fail "cannot read '$PEM'"
grep -q -- '-----BEGIN CERTIFICATE-----' "$PEM" ||
  fail "no PEM certificate in '$PEM' (DER? convert with: openssl x509 -inform DER -in '$PEM' -out '$PEM.pem')"

# --- 1. what is in the bundle ------------------------------------------------
CERT_DIR="$(mktemp -d)"
PROBE_WS="$(mktemp -d)"
cleanup() {
  sbx rm --force "$PROBE" >/dev/null 2>&1 || true
  rm -rf "$CERT_DIR"
  rmdir "$PROBE_WS" 2>/dev/null || true
}
trap cleanup EXIT

awk -v dir="$CERT_DIR" '
  /-----BEGIN CERTIFICATE-----/ { n++; out = sprintf("%s/%03d.pem", dir, n) }
  out { print > out }
  /-----END CERTIFICATE-----/ { close(out); out = "" }
' "$PEM"

log "extra CA bundle: $PEM"
VALID=0; EXPIRED=0
for f in "$CERT_DIR"/*.pem; do
  openssl x509 -in "$f" -noout >/dev/null 2>&1 || fail "$(basename "$f" .pem)th certificate in '$PEM' does not parse"
  log "  - $(openssl x509 -in "$f" -noout -subject)"
  log "    $(openssl x509 -in "$f" -noout -issuer)"
  log "    $(openssl x509 -in "$f" -noout -enddate)"
  log "    $(openssl x509 -in "$f" -noout -fingerprint -sha256)"
  if openssl x509 -in "$f" -noout -checkend 0 >/dev/null 2>&1; then
    VALID=$((VALID + 1))
  else
    EXPIRED=$((EXPIRED + 1))
    log "    !!!!! EXPIRED — clients will not trust it (see docs/sandbox-custom-ca.md §1.3) !!!!!"
  fi
done
[ "$VALID" -gt 0 ] || log "!!!!! every certificate in $PEM has EXPIRED; expect the WITH-CA probes to fail. Re-export the current root (setup.md, Custom CA) !!!!!"

# The daemon sends only the CERTIFICATE blocks; so do we.
CA_B64=$(sed -n '/-----BEGIN CERTIFICATE-----/,/-----END CERTIFICATE-----/p' "$PEM" | base64 | tr -d '\n')
# The daemon's install script, from the heredoc a Go test keeps byte-identical
# to sbx.ExtraCAScript.
[ -r "$HERE/sbx-create.sh" ] || fail "cannot read $HERE/sbx-create.sh (it holds the install script)"
CA_SCRIPT=$(sed -n "/<<'WORKINGMAN_EXTRA_CA_EOF'\$/,/^WORKINGMAN_EXTRA_CA_EOF\$/p" "$HERE/sbx-create.sh" | sed '1d;$d')
[ -n "$CA_SCRIPT" ] || fail "no WORKINGMAN_EXTRA_CA_EOF heredoc in $HERE/sbx-create.sh"

# --- probes ------------------------------------------------------------------
# run_probe NAME echoes "ok", "cert: <detail>" (a rejected certificate) or
# "fail: <detail>" (anything else: policy, DNS, timeout, no such binary).
run_probe() {
  local script
  case "$1" in
    git-github)
      script='command -v git >/dev/null 2>&1 || { echo "git not installed"; echo "rc=127"; exit 0; }
GIT_TERMINAL_PROMPT=0 git ls-remote "$1" HEAD 2>&1 >/dev/null; echo "rc=$?"'
      set -- "$GIT_URL" ;;
    curl-github)    script='curl -sS -o /dev/null --max-time 20 "$1" 2>&1; echo "rc=$?"'; set -- https://github.com ;;
    curl-anthropic) script='curl -sS -o /dev/null --max-time 20 "$1" 2>&1; echo "rc=$?"'; set -- https://api.anthropic.com ;;
  esac
  local out rc detail
  out="$(sbx exec "$PROBE" -- sh -c "$script" sh "$1" 2>&1 || true)"
  rc="$(printf '%s\n' "$out" | sed -n 's/^rc=//p' | tail -n1)"
  detail="$(printf '%s\n' "$out" | grep -v '^rc=' | tr '\n' ' ' | sed 's/ *$//')"
  if [ "$rc" = 0 ]; then
    echo ok
  elif printf '%s' "$detail" | grep -qi 'certificate'; then
    echo "cert: $detail"
  else
    echo "fail: ${rc:+exit $rc: }$detail"
  fi
}

# Results live in R_<phase>_<probe> variables (macOS's bash 3.2 has no
# associative arrays).
result_var() { printf 'R_%s_%s' "$1" "${2//-/_}"; }
result() { local v; v="$(result_var "$1" "$2")"; printf '%s' "${!v-}"; }
run_all() { # run_all PHASE
  local p r
  for p in "${PROBES[@]}"; do
    r="$(run_probe "$p")"
    printf -v "$(result_var "$1" "$p")" '%s' "$r"
    log "  [$1] $p: $r"
  done
}

# --- 2. probe sandbox, created like the daemon creates one --------------------
args=(create claude --name "$PROBE")
[ -n "$KIT" ] && args+=(--kit "$KIT")
[ -n "$IMAGE" ] && args+=(-t "$IMAGE" --pull missing)
log "creating probe sandbox $PROBE (kit: ${KIT:-<none>}, image: ${IMAGE:-<stock>})..."
sbx "${args[@]}" "$PROBE_WS" >/dev/null || fail "sbx ${args[*]} $PROBE_WS failed — is sandboxd healthy?"
# sbx's network default is deny; grant the probe what real tasks get, so a
# failure here measures TLS trust, not egress policy. Scoped to the probe.
sbx policy allow network --sandbox "$PROBE" "github.com,api.anthropic.com" >/dev/null ||
  log "WARNING: could not allow github.com,api.anthropic.com for $PROBE; probes may fail on policy"

# --- 3. without the CA ---------------------------------------------------------
log "probing WITHOUT the extra CA..."
run_all without

# --- 4. install, twice ---------------------------------------------------------
log "installing $PEM the way the daemon does..."
out="$(sbx exec "$PROBE" -- sh -c "$CA_SCRIPT" sh "$CA_B64" 2>&1)" || fail "CA install failed: $out"
log "  first:  $out"
out="$(sbx exec "$PROBE" -- sh -c "$CA_SCRIPT" sh "$CA_B64" 2>&1)" || fail "second CA install failed: $out"
log "  second: $out"
IDEMPOTENT=ok
printf '%s' "$out" | grep -q 'already installed' || { IDEMPOTENT="NOT idempotent"; log "  WARNING: the second install did not report 'already installed'"; }

# --- 5. with the CA -------------------------------------------------------------
log "probing WITH the extra CA..."
run_all with

# --- 6. survives stop/start? (informational) -----------------------------------
PHASES=(without with)
if sbx stop --help >/dev/null 2>&1; then
  log "stopping $PROBE and re-probing without re-installing (informational)..."
  if sbx stop "$PROBE" >/dev/null 2>&1; then
    run_all restarted
    PHASES+=(restarted)
  else
    log "  sbx stop $PROBE failed; skipping"
  fi
fi

# --- 7. summary -------------------------------------------------------------------
short() { case "$1" in ok) echo ok ;; cert:*) echo CERT-FAIL ;; *) echo FAIL ;; esac; }
log ""
log "summary for $PEM ($VALID valid, $EXPIRED expired cert(s); install: $IDEMPOTENT)"
printf '  %-16s' probe; for ph in "${PHASES[@]}"; do printf ' %-10s' "$ph"; done; printf '\n'
NEEDED=0; WITH_OK=1
for p in "${PROBES[@]}"; do
  printf '  %-16s' "$p"
  for ph in "${PHASES[@]}"; do printf ' %-10s' "$(short "$(result "$ph" "$p")")"; done
  printf '\n'
  [ "$(result without "$p")" = ok ] || NEEDED=1
  [ "$(result with "$p")" = ok ] || WITH_OK=0
done
log ""
if [ "$NEEDED" = 0 ]; then
  log "note: every probe passed WITHOUT the CA — this network does not need an extra CA right now (off the VPN?)."
fi
[ "$WITH_OK" = 1 ] || fail "some probes fail WITH the extra CA: it is the wrong root or expired (see the details above and setup.md, Custom CA)"
[ "$VALID" -gt 0 ] || fail "every certificate in $PEM has expired"
if [ "$NEEDED" = 1 ]; then
  log "OK — the probes fail without the CA and pass with it: sandboxes on this laptop work with $PEM."
else
  log "OK — the probes pass with $PEM installed."
fi
