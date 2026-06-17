#!/usr/bin/env bash
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"

"$HERE/orch" \
  --root ~/orch \
  --audit-log ~/orch/audit.log \
  --acp-kit "${ACP_KIT:-$HERE/../acp-kit}" \
  --acp-wrapper "$HERE/acp-wrapper"
