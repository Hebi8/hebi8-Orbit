#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HUB_BIN="${HUB_BIN:-$ROOT_DIR/bin/h8o-hub}"
RUNNER_BIN="${RUNNER_BIN:-$ROOT_DIR/bin/h8o-runner}"
CLI_BIN="${CLI_BIN:-$ROOT_DIR/bin/h8o}"
AGY_BIN="${AGY_BIN:-$(command -v agy || true)}"

if [ ! -x "$HUB_BIN" ] || [ ! -x "$RUNNER_BIN" ] || [ ! -x "$CLI_BIN" ]; then
  echo "error: build bin/h8o-hub, bin/h8o-runner, and bin/h8o first (make build)" >&2
  exit 1
fi
if [ -z "$AGY_BIN" ] || [ ! -x "$AGY_BIN" ]; then
  echo "error: agy binary not found in PATH" >&2
  exit 1
fi

TMP_ROOT="$(mktemp -d /tmp/orbit-p1-XXXXXX)"
HUB_PID=""
RUNNER_PID=""

cleanup() {
  local rc=$?
  if [ -n "$RUNNER_PID" ] && kill -0 "$RUNNER_PID" 2>/dev/null; then
    kill -TERM "$RUNNER_PID" 2>/dev/null || true
    for _ in $(seq 1 20); do
      if ! kill -0 "$RUNNER_PID" 2>/dev/null; then
        break
      fi
      sleep 0.2
    done
    if kill -0 "$RUNNER_PID" 2>/dev/null; then
      kill -KILL "$RUNNER_PID" 2>/dev/null || true
    fi
    wait "$RUNNER_PID" 2>/dev/null || true
  fi
  if [ -n "$HUB_PID" ] && kill -0 "$HUB_PID" 2>/dev/null; then
    kill -TERM "$HUB_PID" 2>/dev/null || true
    for _ in $(seq 1 20); do
      if ! kill -0 "$HUB_PID" 2>/dev/null; then
        break
      fi
      sleep 0.2
    done
    if kill -0 "$HUB_PID" 2>/dev/null; then
      kill -KILL "$HUB_PID" 2>/dev/null || true
    fi
    wait "$HUB_PID" 2>/dev/null || true
  fi
  if [ "$rc" -ne 0 ]; then
    echo "--- hub.log ---" >&2
    cat "$TMP_ROOT/hub.log" >&2 || true
    echo "--- runner.log ---" >&2
    cat "$TMP_ROOT/runner.log" >&2 || true
  fi
  chmod -R u+w "$TMP_ROOT" 2>/dev/null || true
  rm -rf "$TMP_ROOT"
  exit "$rc"
}
trap cleanup EXIT

HUB_PORT="$(python3 -c '
import socket
s = socket.socket()
s.bind(("127.0.0.1", 0))
p = s.getsockname()[1]
s.close()
print(p)
')"

CFG_DIR="$TMP_ROOT/config"
DATA_DIR="$TMP_ROOT/data"
WORK_REPO="$TMP_ROOT/work-repo"
SOCK_PATH="$TMP_ROOT/runner.sock"
mkdir -p "$CFG_DIR" "$DATA_DIR" "$WORK_REPO"

read -r RUNNER_TOKEN RUNNER_TOKEN_HASH < <(python3 -c '
import hashlib, secrets
tok = "smoke-runner-" + secrets.token_hex(16)
print(tok, "sha256:" + hashlib.sha256(tok.encode()).hexdigest())
')

cat > "$CFG_DIR/machines.yaml" <<YAML
schemaVersion: 1
machines:
  hub-node:
    displayName: "hub-node"
    transport: local_ws
    tokenHash: "$RUNNER_TOKEN_HASH"
    tailscaleHost: "orbit.example.ts.net"
    capabilities: [primary-dev]
    adapters:
      agy:
        bin: "$AGY_BIN"
        defaultArgs: ["acp", "--stdio", "--process-per-session"]
        maxLiveWorkers: 4
        idleCloseSeconds: 600
YAML

cat > "$CFG_DIR/projects.yaml" <<YAML
schemaVersion: 1
projects:
  demo-app:
    displayName: "demo-app"
    repoIdentity: "github.com/example-org/demo-app"
    defaultBranch: "main"
    defaultCapability: "primary-dev"
    defaultAdapter: "agy"
    defaultModelConfig:
      model: "gemini-4-neon"
      thought_level: "low"
      mode: "auto"
    ideScheme: "cursor"
    anchors:
      primary-dev:
        machine: "hub-node"
        repoPath: "$WORK_REPO"
        worktreeRoot: "$DATA_DIR/worktrees/demo-app"
YAML

# 1. Initialize isolated temp git repository
export GIT_AUTHOR_NAME="Orbit Smoke"
export GIT_AUTHOR_EMAIL="smoke@example.com"
export GIT_COMMITTER_NAME="Orbit Smoke"
export GIT_COMMITTER_EMAIL="smoke@example.com"

git -C "$WORK_REPO" init -b main >/dev/null
cat > "$WORK_REPO/app.go" <<'EOF'
package main

import "fmt"

func main() {
	fmt.Println("orbit phase1")
}
EOF
git -C "$WORK_REPO" add -A
git -C "$WORK_REPO" commit -m "initial commit" >/dev/null

# 2. Launch temporary orbit-hub
HUB_URL="http://127.0.0.1:${HUB_PORT}"
HUB_WS="ws://127.0.0.1:${HUB_PORT}/ws/runner"

"$HUB_BIN" \
  --config-dir "$CFG_DIR" \
  --data-dir "$DATA_DIR" \
  --listen "127.0.0.1:${HUB_PORT}" \
  --hub-origin "$HUB_URL" \
  --dev-open \
  --ryugi-poll-seconds 0 \
  >"$TMP_ROOT/hub.log" 2>&1 &
HUB_PID=$!

for _ in $(seq 1 40); do
  if curl -sf "${HUB_URL}/api/health" >/dev/null 2>&1; then
    break
  fi
  sleep 0.25
done
curl -sf "${HUB_URL}/api/health" >/dev/null
echo "[OK] h8o-hub healthy at ${HUB_URL}"

# 2b. Verify Runner channel security (B5), adapter guard (B18), and WS ticket (S6)
NO_TOK_STATUS="$(curl -s -o /dev/null -w '%{http_code}' "${HUB_URL}/ws/runner")"
if [ "$NO_TOK_STATUS" != "401" ]; then
  echo "FAIL: expected 401 for unauthenticated /ws/runner, got ${NO_TOK_STATUS}" >&2
  exit 1
fi
ORIGIN_STATUS="$(curl -s -o /dev/null -w '%{http_code}' \
  -H "Authorization: Bearer ${RUNNER_TOKEN}" \
  -H "Origin: http://evil.example" \
  "${HUB_URL}/ws/runner")"
if [ "$ORIGIN_STATUS" != "403" ]; then
  echo "FAIL: expected 403 for /ws/runner with browser Origin header, got ${ORIGIN_STATUS}" >&2
  exit 1
fi
CLAUDE_STATUS="$(curl -s -o /dev/null -w '%{http_code}' -X POST "${HUB_URL}/api/projects/demo-app/tasks" \
  -H "Content-Type: application/json" \
  -H "X-Orbit-Request: 1" \
  -d '{"title":"unsupported","goal":"noop","adapter":"claude"}')"
if [ "$CLAUDE_STATUS" != "501" ]; then
  echo "FAIL: expected 501 for unimplemented adapter=claude, got ${CLAUDE_STATUS}" >&2
  exit 1
fi
WS_TICKET="$(curl -sf -X POST "${HUB_URL}/api/ws-ticket" -H "X-Orbit-Request: 1")"
python3 -c 'import json, sys; d = json.loads(sys.argv[1]); assert d.get("ticket") and d.get("expiresAt", 0) > 0, d' "$WS_TICKET"
echo "[OK] security & API guards verified (/ws/runner 401/403, adapter=claude 501, /api/ws-ticket)"

# 3. Launch temporary h8o-runner with real agy acp
"$RUNNER_BIN" \
  --machine-id hub-node \
  --hub-ws "$HUB_WS" \
  --hub-http "$HUB_URL" \
  --token "$RUNNER_TOKEN" \
  --config-dir "$CFG_DIR" \
  --data-dir "$DATA_DIR" \
  --sock "$SOCK_PATH" \
  --agy-bin "$AGY_BIN" \
  --h8o-bin-dir "$ROOT_DIR/bin" \
  >"$TMP_ROOT/runner.log" 2>&1 &
RUNNER_PID=$!

# Wait for runner.hello -> hub.welcome handshake
RUNNER_READY=0
for _ in $(seq 1 60); do
  BOOT="$(curl -sf "${HUB_URL}/api/bootstrap")"
  if python3 -c '
import json, sys
d = json.loads(sys.argv[1])
for m in d["machines"]:
    if m["machineId"] == "hub-node" and m["status"] == "online" and m.get("runnerVersion"):
        sys.exit(0)
sys.exit(1)
' "$BOOT"; then
    RUNNER_READY=1
    break
  fi
  sleep 0.25
done
if [ "$RUNNER_READY" -ne 1 ]; then
  echo "FAIL: h8o-runner did not register online with hub" >&2
  exit 1
fi
echo "[OK] h8o-runner connected to hub via /ws/runner (real agy ACP initialized)"

# 4. Create task via REST API -> real agy edits app.go in isolated worktree and runs `h8o deliver --final`
CREATE_PAYLOAD="$(python3 -c '
import json
print(json.dumps({
    "title": "Phase 1 Smoke Greet Helper",
    "goal": "Edit app.go in this worktree to add a exported function `Greet() string` that returns \"hello orbit\". Then run the shell command: h8o deliver --summary \"Added Greet helper\" --final. Do not spawn subagents or swarm workers; stop right after h8o deliver succeeds."
}))
')"

CREATE_RESP="$(curl -sf -X POST "${HUB_URL}/api/projects/demo-app/tasks" \
  -H "Content-Type: application/json" \
  -H "X-Orbit-Request: 1" \
  -d "$CREATE_PAYLOAD")"

read -r TASK_ID ATTEMPT_ID DISPATCHED < <(python3 -c '
import json, sys
d = json.loads(sys.argv[1])
print(d["task"]["taskId"], d["attempt"]["attemptId"], str(d["dispatched"]).lower())
' "$CREATE_RESP")

if [ "$DISPATCHED" != "true" ]; then
  echo "FAIL: task was not dispatched to runner: $CREATE_RESP" >&2
  exit 1
fi
echo "[OK] created & dispatched task=${TASK_ID} attempt=${ATTEMPT_ID}"

# 5. Poll task timeline until attempt reaches terminal state (timeout 120s)
FINAL_STATE=""
for _ in $(seq 1 240); do
  TL="$(curl -sf "${HUB_URL}/api/tasks/${TASK_ID}/timeline")"
  FINAL_STATE="$(python3 -c '
import json, sys
d = json.loads(sys.argv[1])
atts = d.get("attempts") or []
print(atts[-1]["state"] if atts else "")
' "$TL")"
  case "$FINAL_STATE" in
    succeeded|failed|cancelled|expired|needs_verification)
      break
      ;;
  esac
  sleep 0.5
done

if [ "$FINAL_STATE" != "succeeded" ]; then
  echo "FAIL: expected attempt state=succeeded, got state=${FINAL_STATE}" >&2
  curl -sf "${HUB_URL}/api/tasks/${TASK_ID}/timeline" >&2 || true
  exit 1
fi

TL="$(curl -sf "${HUB_URL}/api/tasks/${TASK_ID}/timeline")"
read -r COMP_SRC DLV_ID DLV_HEAD < <(python3 -c '
import json, sys
d = json.loads(sys.argv[1])
att = d["attempts"][-1]
dlvs = d.get("deliveries") or []
dlv_id = dlvs[-1]["deliveryId"] if dlvs else ""
dlv_head = dlvs[-1]["headSha"] if dlvs else ""
print(att.get("completionSource") or "", dlv_id, dlv_head)
' "$TL")

if [ -z "$DLV_ID" ]; then
  echo "FAIL: agy did not produce a delivery via h8o deliver --final: $TL" >&2
  exit 1
fi
echo "[OK] real agy attempt succeeded (completionSource=${COMP_SRC}, delivery=${DLV_ID}, head=${DLV_HEAD:0:7})"

# 6. Verify Diff contains Greet helper
DIFF_JSON="$(curl -sf "${HUB_URL}/api/deliveries/${DLV_ID}/diff")"
python3 -c '
import json, sys
d = json.loads(sys.argv[1])
assert len(d["files"]) >= 1, f"expected >=1 changed file, got {d}"
diff_text = d.get("diffText", "")
assert "Greet" in diff_text, f"expected Greet in diffText: {diff_text}"
' "$DIFF_JSON"
echo "[OK] delivery diff verified non-empty with Greet helper"

# 7. Verify streaming raw_acp events persisted lazily
RAW_JSON="$(curl -sf "${HUB_URL}/api/attempts/${ATTEMPT_ID}/raw")"
RAW_COUNT="$(python3 -c '
import json, sys
d = json.loads(sys.argv[1])
evs = d.get("events") or []
assert len(evs) > 0, "expected streamed raw_acp events"
print(len(evs))
' "$RAW_JSON")"
echo "[OK] persisted ${RAW_COUNT} streaming raw_acp events in SQLite"

# 8. Verify original repo working tree has ZERO pollution
if [ -n "$(git -C "$WORK_REPO" status --porcelain)" ]; then
  echo "FAIL: original repo working tree was polluted!" >&2
  exit 1
fi
echo "[OK] original repository working tree has zero pollution"

# 9. Verify `h8o brief` (Brief Me)
BRIEF_JSON="$("$CLI_BIN" brief --hub "$HUB_URL" --json)"
python3 -c '
import json, sys
d = json.loads(sys.argv[1])
assert d["snapshotSeq"] > 0, d
assert len(d["recentDeliveries"]) >= 1, d
assert any(x["deliveryId"] == sys.argv[2] for x in d["recentDeliveries"]), d
' "$BRIEF_JSON" "$DLV_ID"
echo "[OK] h8o brief (POST /api/pa/brief) verified with live delivery"

echo "=== PHASE 1 REAL AGY ACP SMOKE PASSED ==="
