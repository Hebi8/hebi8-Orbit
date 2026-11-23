#!/usr/bin/env bash
set -euo pipefail

# Scans repository source files for accidental private information leaks:
# - Tailscale CGNAT IPv4 addresses (100.64.0.0/10)
# - Real Tailnet MagicDNS domains (*.tail<hex>.ts.net)
# - Real non-example email addresses
# - Personal home directory paths (/home/<user>, /Users/<user>) outside safe placeholders
# - Private key blocks

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

FAIL=0

# Collect files to scan (tracked + untracked non-ignored, excluding generated/vendor/build/lock files)
if git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
  mapfile -t FILES < <(git ls-files --cached --others --exclude-standard | grep -vE '^(bin/|web/node_modules/|web/dist/|web/pnpm-lock\.yaml|go\.sum|scripts/check-privacy\.sh$)')
else
  mapfile -t FILES < <(find . -type f -not -path './.git/*' -not -path './bin/*' -not -path './web/node_modules/*' -not -path './web/dist/*' -not -name 'pnpm-lock.yaml' -not -name 'go.sum' -not -path './scripts/check-privacy.sh')
fi

if [ "${#FILES[@]}" -eq 0 ]; then
  echo "check-privacy: no files to scan"
  exit 0
fi

scan_regex() {
  local label="$1"
  local pattern="$2"
  local exclude_pattern="${3:-}"
  local matches=""
  if [ -n "$exclude_pattern" ]; then
    matches="$(grep -nHE "$pattern" "${FILES[@]}" 2>/dev/null | grep -vE "$exclude_pattern" || true)"
  else
    matches="$(grep -nHE "$pattern" "${FILES[@]}" 2>/dev/null || true)"
  fi
  if [ -n "$matches" ]; then
    echo "PRIVACY CHECK FAILED [$label]:"
    echo "$matches"
    FAIL=1
  fi
}

# 1. Tailscale CGNAT IPv4 range 100.64.0.0 - 100.127.255.255
scan_regex "tailscale-ip" '\b100\.(6[4-9]|[7-9][0-9]|1[0-1][0-9]|12[0-7])\.[0-9]{1,3}\.[0-9]{1,3}\b'

# 2. Real Tailnet MagicDNS hostnames (*.tail<hex>.ts.net)
scan_regex "tailnet-magicdns" '[a-zA-Z0-9-]+\.tail[0-9a-f]{4,}\.ts\.net'

# 3. Real email addresses (allow only example.com / example.org / example.net / localhost)
scan_regex "personal-email" '[a-zA-Z0-9._%+-]+@[a-zA-Z0-9.-]+\.[a-zA-Z]{2,}' \
  '@(example\.(com|org|net)|localhost|users\.noreply\.github\.com|react\.dev|tailwindcss\.com)'

# 4. Personal absolute home paths (/home/<user> or /Users/<user>) except explicit placeholder users
scan_regex "personal-home-path" '(/home/|/Users/)[a-zA-Z0-9._-]+' \
  '(/home/|/Users/)(orbit|example|user|dev|runner|alice|bob)\b'

# 5. Private keys
scan_regex "private-key-material" 'BEGIN (RSA |OPENSSH |EC |ED25519 )?PRIVATE KEY'

if [ "$FAIL" -ne 0 ]; then
  echo "check-privacy: FAILED"
  exit 1
fi

echo "check-privacy: OK (${#FILES[@]} files scanned)"
