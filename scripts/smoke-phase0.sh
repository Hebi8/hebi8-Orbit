#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
HUB_BIN="${HUB_BIN:-$ROOT_DIR/bin/h8o-hub}"
CLI_BIN="${CLI_BIN:-$ROOT_DIR/bin/h8o}"
RYUGI_BIN="${RYUGI_BIN:-$(command -v ryugi)}"

if [ ! -x "$HUB_BIN" ] || [ ! -x "$CLI_BIN" ]; then
  echo "error: build bin/h8o-hub and bin/h8o first (make build)" >&2
  exit 1
fi
if [ -z "$RYUGI_BIN" ] || [ ! -x "$RYUGI_BIN" ]; then
  echo "error: ryugi binary not found in PATH" >&2
  exit 1
fi

TMP_ROOT="$(mktemp -d)"
RYUGI_HOME="$TMP_ROOT/ryugi-home"
mkdir -p "$RYUGI_HOME"
HUB_PID=""

cleanup() {
  local rc=$?
  if [ -n "$HUB_PID" ] && kill -0 "$HUB_PID" 2>/dev/null; then
    kill "$HUB_PID" 2>/dev/null || true
    wait "$HUB_PID" 2>/dev/null || true
  fi
  RYUGI_SERVER_HOME="$RYUGI_HOME" "$RYUGI_BIN" stop --format json >/dev/null 2>&1 || true
  chmod -R u+w "$TMP_ROOT" 2>/dev/null || true
  rm -rf "$TMP_ROOT"
  exit "$rc"
}
trap cleanup EXIT

read -r HUB_PORT RYUGI_PORT < <(python3 -c '
import socket
def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    p = s.getsockname()[1]
    s.close()
    return p
print(free_port(), free_port())
')

CFG_DIR="$TMP_ROOT/config"
DATA_DIR="$TMP_ROOT/data"
WORK_REPO="$TMP_ROOT/work-repo"
mkdir -p "$CFG_DIR" "$DATA_DIR" "$WORK_REPO"

cat > "$CFG_DIR/machines.yaml" <<'YAML'
schemaVersion: 1
machines:
  hub-node:
    displayName: "hub-node"
    transport: local_ws
    tailscaleHost: "orbit.example.ts.net"
    capabilities: [primary-dev, ryugi-host]
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
    ideScheme: "cursor"
    anchors:
      primary-dev:
        machine: "hub-node"
        repoPath: "$WORK_REPO"
YAML

# 1. Initialize real git repo with initial commit + .ryugi/artifacts document
export GIT_AUTHOR_NAME="Orbit Smoke"
export GIT_AUTHOR_EMAIL="smoke@example.com"
export GIT_COMMITTER_NAME="Orbit Smoke"
export GIT_COMMITTER_EMAIL="smoke@example.com"

git -C "$WORK_REPO" init -b main >/dev/null
cat > "$WORK_REPO/app.go" <<'EOF'
package main

import "fmt"

func main() {
	fmt.Println("v0")
}
EOF
mkdir -p "$WORK_REPO/.ryugi/artifacts/document/design-spec"
cat > "$WORK_REPO/.ryugi/artifacts/document/design-spec/doc.xml" <<'EOF'
<doc>
  <h1 id="h1">Design Spec v0</h1>
  <p id="p1">Initial paragraph. Pick: <ask id="q1" recommend="a"><option value="a">Option A</option><option value="b">Option B</option></ask></p>
</doc>
EOF
git -C "$WORK_REPO" add -A
git -C "$WORK_REPO" commit -m "initial commit" >/dev/null
INITIAL_BASE_SHA="$(git -C "$WORK_REPO" rev-parse HEAD)"

# 2. Launch real h8o-hub with isolated RYUGI_SERVER_HOME
HUB_URL="http://127.0.0.1:${HUB_PORT}"
"$HUB_BIN" \
  --config-dir "$CFG_DIR" \
  --data-dir "$DATA_DIR" \
  --listen "127.0.0.1:${HUB_PORT}" \
  --hub-origin "$HUB_URL" \
  --dev-open \
  --ryugi-bin "$RYUGI_BIN" \
  --ryugi-server-home "$RYUGI_HOME" \
  --ryugi-port "$RYUGI_PORT" \
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
curl -sf "${HUB_URL}/" | grep -q "hebi8/orbit"

# 3. Make dirty working tree edits + untracked file + external attachment
cat > "$WORK_REPO/app.go" <<'EOF'
package main

import "fmt"

func main() {
	fmt.Println("v1 dirty")
}
EOF
cat > "$WORK_REPO/helper.go" <<'EOF'
package main

func helper() string { return "ok" }
EOF
cat > "$WORK_REPO/.ryugi/artifacts/document/design-spec/doc.xml" <<'EOF'
<doc>
  <h1 id="h1">Design Spec v1</h1>
  <p id="p1">Updated paragraph for review. Pick: <ask id="q1" recommend="a"><option value="a">Option A</option><option value="b">Option B</option></ask></p>
</doc>
EOF
printf 'fake-png-attachment' > "$TMP_ROOT/mockup.png"

STATUS_BEFORE="$(git -C "$WORK_REPO" status --porcelain)"
MIRROR_PATH="$DATA_DIR/git/demo-app.git"

# 4. Delivery v1 (dirty workspace)
V1_JSON="$("$CLI_BIN" deliver \
  --project demo-app \
  --repo "$WORK_REPO" \
  --summary "v1 dirty checkpoint" \
  --attach "$TMP_ROOT/mockup.png:assets/mockup.png" \
  --hub "$HUB_URL" \
  --data-dir "$DATA_DIR" \
  --mirror "$MIRROR_PATH" \
  --json)"

STATUS_AFTER="$(git -C "$WORK_REPO" status --porcelain)"
if [ "$STATUS_BEFORE" != "$STATUS_AFTER" ]; then
  echo "FAIL: repo git status was polluted by h8o deliver!" >&2
  exit 1
fi
echo "[OK] v1 zero-pollution git status verified"

read -r TASK_ID DLV_V1 ID_V1 URL_V1 SYNC_V1 < <(python3 -c '
import json, sys
d = json.loads(sys.argv[1])
b = d["ryugiBindings"][0]
print(d["task"]["taskId"], d["delivery"]["deliveryId"], b["derivedArtifactId"], b["openUrl"], d["delivery"]["ryugiSyncState"])
' "$V1_JSON")

if [ "$SYNC_V1" != "ready" ] || [ "${#ID_V1}" -ne 21 ]; then
  echo "FAIL: unexpected v1 ryugi binding state=$SYNC_V1 id=$ID_V1" >&2
  exit 1
fi
curl -sf "$URL_V1" | grep -q "data-ryugi-artifact-id"
echo "[OK] v1 registered with real ryugi 1.105.8: delivery=$DLV_V1 artifactId=$ID_V1 url=$URL_V1"

# Verify v1 frozen permissions: doc=0444, artifactDir=0755, typeDir=0555, artifacts=0555, .state=0755
SNAP_V1="$DATA_DIR/deliveries/$DLV_V1"
MODE_DOC="$(stat -c '%a' "$SNAP_V1/.ryugi/artifacts/document/design-spec/doc.xml")"
MODE_ARTDIR="$(stat -c '%a' "$SNAP_V1/.ryugi/artifacts/document/design-spec")"
MODE_TYPEDIR="$(stat -c '%a' "$SNAP_V1/.ryugi/artifacts/document")"
MODE_ROOT="$(stat -c '%a' "$SNAP_V1/.ryugi/artifacts")"
MODE_STATE="$(stat -c '%a' "$SNAP_V1/.ryugi/artifacts/.state")"
if [ "$MODE_DOC" != "444" ] || [ "$MODE_ARTDIR" != "755" ] || [ "$MODE_TYPEDIR" != "555" ] || [ "$MODE_ROOT" != "555" ] || [ "$MODE_STATE" != "755" ]; then
  echo "FAIL: unexpected frozen permissions doc=$MODE_DOC artDir=$MODE_ARTDIR typeDir=$MODE_TYPEDIR root=$MODE_ROOT state=$MODE_STATE" >&2
  exit 1
fi
echo "[OK] v1 frozen snapshot permissions verified (doc=0444, artDir=0755, typeDir=0555, root=0555, .state=0755)"

# 5. Commit all changes cleanly, then deliver v2 with explicit --base INITIAL_BASE_SHA
cat > "$WORK_REPO/.ryugi/artifacts/document/design-spec/doc.xml" <<'EOF'
<doc>
  <h1 id="h1">Design Spec v2</h1>
  <p id="p1">Committed clean working tree revision. Pick: <ask id="q1" recommend="a"><option value="a">Option A</option><option value="b">Option B</option></ask></p>
</doc>
EOF
git -C "$WORK_REPO" add -A
git -C "$WORK_REPO" commit -m "agent committed v2 changes" >/dev/null
COMMITTED_HEAD="$(git -C "$WORK_REPO" rev-parse HEAD)"

if [ -n "$(git -C "$WORK_REPO" status --porcelain)" ]; then
  echo "FAIL: expected clean working tree before v2 deliver" >&2
  exit 1
fi

V2_JSON="$("$CLI_BIN" deliver \
  --project demo-app \
  --repo "$WORK_REPO" \
  --base "$INITIAL_BASE_SHA" \
  --task "$TASK_ID" \
  --summary "v2 committed clean tree" \
  --hub "$HUB_URL" \
  --data-dir "$DATA_DIR" \
  --mirror "$MIRROR_PATH" \
  --json)"

read -r DLV_V2 HEAD_V2 ID_V2 URL_V2 SYNC_V2 < <(python3 -c '
import json, sys
d = json.loads(sys.argv[1])
b = d["ryugiBindings"][0]
print(d["delivery"]["deliveryId"], d["delivery"]["headSha"], b["derivedArtifactId"], b["openUrl"], d["delivery"]["ryugiSyncState"])
' "$V2_JSON")

if [ "$HEAD_V2" != "$COMMITTED_HEAD" ]; then
  echo "FAIL: clean working tree should reuse HEAD ($COMMITTED_HEAD), got $HEAD_V2" >&2
  exit 1
fi
if [ "$SYNC_V2" != "ready" ] || [ "${#ID_V2}" -ne 21 ] || [ "$ID_V2" = "$ID_V1" ]; then
  echo "FAIL: v2 artifact-id conflict or bad sync state: v1=$ID_V1 v2=$ID_V2 state=$SYNC_V2" >&2
  exit 1
fi
curl -sf "$URL_V1" >/dev/null
curl -sf "$URL_V2" | grep -q "data-ryugi-artifact-id"
echo "[OK] v2 clean-tree reuse + distinct .artifact-id verified: v1=$ID_V1 v2=$ID_V2"

# Verify v2 Diff against review base SHA is NON-EMPTY
DIFF_V2="$(curl -sf "${HUB_URL}/api/deliveries/${DLV_V2}/diff")"
python3 -c '
import json, sys
d = json.loads(sys.argv[1])
files = d["files"]
assert len(files) == 3, f"expected 3 changed files, got {len(files)}"
assert "v1 dirty" in d["diffText"], "diffText missing expected app.go change"
assert "helper()" in d["diffText"], "diffText missing expected helper.go change"
' "$DIFF_V2"
echo "[OK] v2 dual-baseline diff non-empty (3 changed files verified)"

# 6. Submit Diff annotation TWICE with same Idempotency-Key -> verify idempotency
ANN_BODY='{"source":"diff_drawer","anchor":{"path":"app.go","side":"new","startLine":6,"endLine":6},"body":"Add structured logging here"}'
DIFF_ANN_1="$(curl -sf -X POST "${HUB_URL}/api/deliveries/${DLV_V2}/annotations" \
  -H "Content-Type: application/json" \
  -H "X-Orbit-Request: 1" \
  -H "Idempotency-Key: smoke-diff-key-1" \
  -d "$ANN_BODY")"
DIFF_ANN_2="$(curl -sf -X POST "${HUB_URL}/api/deliveries/${DLV_V2}/annotations" \
  -H "Content-Type: application/json" \
  -H "X-Orbit-Request: 1" \
  -H "Idempotency-Key: smoke-diff-key-1" \
  -d "$ANN_BODY")"

python3 -c '
import json, sys
a1, a2 = json.loads(sys.argv[1]), json.loads(sys.argv[2])
assert a1["created"] is True and a2["created"] is False
assert a1["annotation"]["annotationId"] == a2["annotation"]["annotationId"]
assert a1["annotation"]["projectId"] == "demo-app"
' "$DIFF_ANN_1" "$DIFF_ANN_2"
echo "[OK] Diff annotation persisted & idempotency verified"

# 7. Real Ryugi writes & sync directly on frozen snapshots WITHOUT ANY chmod!
# 7a. On v1 frozen snapshot: prove real ryugi 1.105.8 server atomically writes comments.json & asks without chmod
RYUGI_SERVER_HOME="$RYUGI_HOME" "$RYUGI_BIN" comment add \
  --project-dir "$SNAP_V1" \
  --artifact document/design-spec \
  --block p1 \
  --text "Verified real ryugi server atomic comments.json write" \
  --format json | python3 -c 'import json,sys; d=json.load(sys.stdin); assert d["outcome"]=="success", d'

RYUGI_SERVER_HOME="$RYUGI_HOME" "$RYUGI_BIN" comment answer \
  --project-dir "$SNAP_V1" \
  --artifact document/design-spec \
  --ask q1 \
  --choice b \
  --format json | python3 -c 'import json,sys; d=json.load(sys.stdin); assert d["outcome"]=="success", d'
RYUGI_SERVER_HOME="$RYUGI_HOME" "$RYUGI_BIN" comment submit \
  --project-dir "$SNAP_V1" \
  --artifact document/design-spec \
  --format json | python3 -c 'import json,sys; d=json.load(sys.stdin); assert d["outcome"]=="success", d'

SYNC_V1_1="$(curl -sf -X POST "${HUB_URL}/api/deliveries/${DLV_V1}/ryugi/sync" -H "X-Orbit-Request: 1")"
SYNC_V1_2="$(curl -sf -X POST "${HUB_URL}/api/deliveries/${DLV_V1}/ryugi/sync" -H "X-Orbit-Request: 1")"
python3 -c '
import json, sys
s1, s2 = json.loads(sys.argv[1]), json.loads(sys.argv[2])
assert not s1.get("errors"), f"unexpected v1 sync #1 errors: {s1}"
assert not s2.get("errors"), f"unexpected v1 sync #2 errors: {s2}"
assert s1["newAnnotations"] == 1, f"expected 1 synced ask on v1, got {s1}"
assert s2["newAnnotations"] == 0, f"expected 0 on v1 re-sync, got {s2}"
' "$SYNC_V1_1" "$SYNC_V1_2"
echo "[OK] v1 real ryugi CLI writes (comment add + answer + submit, ZERO chmod) + Hub sync verified"

# 7b. On v2 frozen snapshot: create a human thread as normal user (ZERO chmod!) and verify threads receipt + real comment ack
SNAP_V2="$DATA_DIR/deliveries/$DLV_V2"
SNAP_V2_COMMENTS="$SNAP_V2/.ryugi/artifacts/document/design-spec/comments.json"
cat > "$SNAP_V2_COMMENTS" <<'EOF'
{
  "version": 1,
  "threads": [
    {
      "id": "smoke_th_1",
      "anchor": {
        "blockId": "p1",
        "quote": "Committed clean"
      },
      "status": "open",
      "statusAt": "2026-11-23T20:00:00.000Z",
      "statusBy": "human",
      "messages": [
        {
          "id": "smoke_msg_1",
          "author": "human",
          "name": "Reviewer",
          "text": "Please clarify rollback step in p1",
          "at": "2026-11-23T20:00:00.000Z"
        }
      ]
    }
  ]
}
EOF

SYNC_1="$(curl -sf -X POST "${HUB_URL}/api/deliveries/${DLV_V2}/ryugi/sync" -H "X-Orbit-Request: 1")"
SYNC_2="$(curl -sf -X POST "${HUB_URL}/api/deliveries/${DLV_V2}/ryugi/sync" -H "X-Orbit-Request: 1")"

python3 -c '
import json, sys
s1, s2 = json.loads(sys.argv[1]), json.loads(sys.argv[2])
assert not s1.get("errors"), f"unexpected v2 sync #1 errors: {s1}"
assert not s2.get("errors"), f"unexpected v2 sync #2 errors: {s2}"
assert s1["newAnnotations"] == 1 and s1["receiptsAcked"] == 1, f"unexpected v2 sync #1: {s1}"
assert s2["newAnnotations"] == 0 and s2["receiptsAcked"] == 0, f"unexpected v2 sync #2: {s2}"
' "$SYNC_1" "$SYNC_2"
echo "[OK] v2 human thread sync + SQLite commit-before-ack + real ryugi comment ack (ZERO chmod) verified"

# 8. Verify complete-review endpoint resolves inbox item and both annotation sources exist
DETAIL_V2="$(curl -sf "${HUB_URL}/api/deliveries/${DLV_V2}")"
python3 -c '
import json, sys
d = json.loads(sys.argv[1])
sources = sorted(set(a["source"] for a in d["annotations"]))
assert sources == ["diff_drawer", "ryugi"], f"expected diff_drawer & ryugi annotations, got {sources}"
' "$DETAIL_V2"

curl -sf -X POST "${HUB_URL}/api/deliveries/${DLV_V2}/complete-review" -H "X-Orbit-Request: 1" | \
  python3 -c 'import json,sys; d=json.load(sys.stdin); assert d["inboxState"]=="resolved", d'

"$CLI_BIN" status --hub "$HUB_URL" | grep -q "demo-app"
echo "[OK] Both diff_drawer and ryugi annotations + complete-review + h8o status verified"
echo "=== PHASE 0 E2E SMOKE PASSED ==="
