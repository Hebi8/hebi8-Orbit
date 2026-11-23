#!/usr/bin/env bash
set -euo pipefail

# Scans:
# 1. All tracked Git index blobs (via `git ls-files --stage -z` + `git cat-file blob <sha>`),
#    so staged secrets cannot hide behind working-tree edits.
# 2. All untracked non-ignored files in the working tree.
# Uses per-match value allowlists (never whole-line exclusions) and scans itself cleanly.

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

python3 - <<'PY'
import re
import subprocess
import sys
from pathlib import Path

# Construct patterns without literal self-triggering sequences
RE_TAILSCALE_IP = re.compile(r"\b100\.(?:6[4-9]|[7-9]\d|1[0-1]\d|12[0-7])\.\d{1,3}\.\d{1,3}\b")
RE_TS_NET = re.compile(r"\b[a-zA-Z0-9._-]+\." + r"ts\.net\b", re.IGNORECASE)
RE_EMAIL = re.compile(r"\b[a-zA-Z0-9._%+-]+@([a-zA-Z0-9.-]+\.[a-zA-Z]{2,})\b")
RE_HOME_PATH = re.compile(r"/(?:" + "home" + "|" + "Users" + r")/([a-zA-Z0-9._-]+)")
RE_PRIVATE_KEY = re.compile("BEGIN " + r"(?:RSA |OPENSSH |EC |ED25519 )?" + "PRIVATE " + "KEY")

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
    suffix = "example." + "ts.net"
    return d == suffix or d.endswith("." + suffix)

def scan_bytes(label: str, raw: bytes, violations: list[str]) -> bool:
    if b"\x00" in raw:
        return False
    text = raw.decode("utf-8", errors="replace")
    for lineno, line in enumerate(text.splitlines(), 1):
        for m in RE_TAILSCALE_IP.finditer(line):
            violations.append(f"[tailscale-ip] {label}:{lineno}: {m.group(0)}")
        for m in RE_TS_NET.finditer(line):
            val = m.group(0)
            if not is_allowed_ts_net(val):
                violations.append(f"[ts-net-domain] {label}:{lineno}: {val}")
        for m in RE_EMAIL.finditer(line):
            domain = m.group(1).lower()
            if domain not in ALLOWED_EMAIL_DOMAINS:
                violations.append(f"[personal-email] {label}:{lineno}: {m.group(0)}")
        for m in RE_HOME_PATH.finditer(line):
            user = m.group(1)
            if user not in ALLOWED_HOME_USERS:
                violations.append(f"[personal-home-path] {label}:{lineno}: {m.group(0)}")
        for m in RE_PRIVATE_KEY.finditer(line):
            violations.append(f"[private-key] {label}:{lineno}: {m.group(0)}")
    return True

violations: list[str] = []
scanned = 0

# 1. Scan exact staged Git index blobs for every tracked file
stage_res = subprocess.run(["git", "ls-files", "--stage", "-z"], stdout=subprocess.PIPE, check=True)
tracked_paths: set[str] = set()
for entry in stage_res.stdout.split(b"\x00"):
    if not entry:
        continue
    meta, _, path_bytes = entry.partition(b"\t")
    parts = meta.decode("ascii").split()
    if len(parts) < 2:
        continue
    mode, blob_sha = parts[0], parts[1]
    if mode == "160000":
        continue
    rel = path_bytes.decode("utf-8", errors="surrogateescape")
    tracked_paths.add(rel)
    blob = subprocess.run(["git", "cat-file", "blob", blob_sha], stdout=subprocess.PIPE, check=True).stdout
    if scan_bytes(f"index:{rel}", blob, violations):
        scanned += 1
    # Also scan working-tree version if modified locally before staging
    p = Path(rel)
    if p.is_file():
        wt_raw = p.read_bytes()
        if wt_raw != blob:
            scan_bytes(f"worktree:{rel}", wt_raw, violations)

# 2. Scan untracked non-ignored working-tree files
untracked_res = subprocess.run(
    ["git", "ls-files", "-z", "--others", "--exclude-standard"],
    stdout=subprocess.PIPE,
    check=True,
)
for entry in untracked_res.stdout.split(b"\x00"):
    if not entry:
        continue
    rel = entry.decode("utf-8", errors="surrogateescape")
    if rel in tracked_paths:
        continue
    p = Path(rel)
    if not p.is_file():
        continue
    if scan_bytes(f"untracked:{rel}", p.read_bytes(), violations):
        scanned += 1

if violations:
    print("PRIVACY CHECK FAILED:", file=sys.stderr)
    for v in violations:
        print(" ", v, file=sys.stderr)
    sys.exit(1)

print(f"check-privacy: OK ({scanned} index/untracked files scanned)")
PY
