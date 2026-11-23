#!/usr/bin/env bash
set -euo pipefail

# Scans all tracked (and untracked non-ignored) repository files using git ls-files -z
# with value-level allowlists (never whole-line exclusions).

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

python3 - <<'PY'
import re
import subprocess
import sys
from pathlib import Path

def list_files() -> list[str]:
    paths: set[str] = set()
    for cmd in (
        ["git", "ls-files", "-z"],
        ["git", "ls-files", "-z", "--others", "--exclude-standard"],
    ):
        res = subprocess.run(cmd, stdout=subprocess.PIPE, check=True)
        for entry in res.stdout.split(b"\x00"):
            if entry:
                paths.add(entry.decode("utf-8", errors="surrogateescape"))
    return sorted(paths)

RE_TAILSCALE_IP = re.compile(r"\b100\.(?:6[4-9]|[7-9]\d|1[0-1]\d|12[0-7])\.\d{1,3}\.\d{1,3}\b")
RE_TS_NET = re.compile(r"\b[a-zA-Z0-9._-]+\.ts\.net\b", re.IGNORECASE)
RE_EMAIL = re.compile(r"\b[a-zA-Z0-9._%+-]+@([a-zA-Z0-9.-]+\.[a-zA-Z]{2,})\b")
RE_HOME_PATH = re.compile(r"/(?:home|Users)/([a-zA-Z0-9._-]+)")
RE_PRIVATE_KEY = re.compile(r"BEGIN (?:RSA |OPENSSH |EC |ED25519 )?PRIVATE KEY")

ALLOWED_EMAIL_DOMAINS = {
    "example.com",
    "example.org",
    "example.net",
    "localhost",
    "users.noreply.github.com",
}
ALLOWED_HOME_USERS = {"orbit", "example", "user", "dev", "runner", "alice", "bob"}

def is_allowed_ts_net(domain: str) -> bool:
    d = domain.lower()
    return d == "example.ts.net" or d.endswith(".example.ts.net")

violations: list[str] = []
scanned = 0

for rel in list_files():
    if rel == "scripts/check-privacy.sh":
        continue
    p = Path(rel)
    if not p.is_file():
        continue
    raw = p.read_bytes()
    if b"\x00" in raw:
        continue
    scanned += 1
    text = raw.decode("utf-8", errors="replace")
    for lineno, line in enumerate(text.splitlines(), 1):
        for m in RE_TAILSCALE_IP.finditer(line):
            violations.append(f"[tailscale-ip] {rel}:{lineno}: {m.group(0)}")
        for m in RE_TS_NET.finditer(line):
            val = m.group(0)
            if not is_allowed_ts_net(val):
                violations.append(f"[ts-net-domain] {rel}:{lineno}: {val}")
        for m in RE_EMAIL.finditer(line):
            domain = m.group(1).lower()
            if domain not in ALLOWED_EMAIL_DOMAINS:
                violations.append(f"[personal-email] {rel}:{lineno}: {m.group(0)}")
        for m in RE_HOME_PATH.finditer(line):
            user = m.group(1)
            if user not in ALLOWED_HOME_USERS:
                violations.append(f"[personal-home-path] {rel}:{lineno}: {m.group(0)}")
        for m in RE_PRIVATE_KEY.finditer(line):
            violations.append(f"[private-key] {rel}:{lineno}: {m.group(0)}")

if violations:
    print("PRIVACY CHECK FAILED:", file=sys.stderr)
    for v in violations:
        print(" ", v, file=sys.stderr)
    sys.exit(1)

print(f"check-privacy: OK ({scanned} files scanned)")
PY
