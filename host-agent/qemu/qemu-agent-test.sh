#!/usr/bin/env bash
# End-to-end smoke test of the qemu-agent HTTP contract.
# Mirrors fc-agent-test.sh except snapshot/restore are expected to fail with
# 501 (GPU passthrough cannot be checkpointed; decision D4).
#
# Requires the agent running (./qemu-agent.py or systemd). Reads token from
# .env (QEMU_AGENT_TOKEN). Hardware-gated: skip cleanly when no agent is up.
set -euo pipefail
QEMU_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENV_FILE="$QEMU_DIR/.env"
[ -f "$ENV_FILE" ] || { echo "no .env at $QEMU_DIR -- set QEMU_AGENT_TOKEN and QEMU_AGENT_PORT" >&2; exit 1; }
# shellcheck disable=SC1090
source "$ENV_FILE"
BASE="http://127.0.0.1:${QEMU_AGENT_PORT:-8091}"
AUTH=(-H "Authorization: Bearer ${QEMU_AGENT_TOKEN}" -H "Content-Type: application/json")
NAME="smoke-$(date +%s)"

say() { printf '\n\033[1;36m== %s ==\033[0m\n' "$*"; }
j()   { curl -fsS "${AUTH[@]}" "$@"; }
jc()  { curl -sS "${AUTH[@]}" "$@"; }  # don't -f so we can check status codes

# gate: agent reachable?
if ! curl -sS -m 5 "$BASE/healthz" >/dev/null 2>&1; then
  echo "SKIP: qemu-agent not reachable at $BASE"
  exit 0
fi

say "health"
j "$BASE/healthz"; echo

say "create $NAME (gpus=1)"
j -X POST "$BASE/v1/vm" -d "{\"name\":\"$NAME\",\"cpus\":1,\"memory_mb\":1024,\"storage_gb\":1,\"region\":\"local\",\"gpus\":1}"
echo

say "get"
j "$BASE/v1/vm/$NAME"; echo

say "list prefix=smoke"
j "$BASE/v1/vm?prefix=smoke"; echo

say "upload /root/hello.txt"
B64=$(printf 'hi from fuse\n' | base64 -w0)
j -X POST "$BASE/v1/vm/$NAME/upload" -d "{\"path\":\"/root/hello.txt\",\"content_b64\":\"$B64\"}"
echo

say "exec: cat uploaded file"
RESP=$(j -X POST "$BASE/v1/vm/$NAME/exec" -d '{"cmd":["/bin/sh","-lc","cat /root/hello.txt && uname -r"]}')
echo "$RESP"
echo "stdout decoded:"
echo "$RESP" | python3 -c 'import sys,json,base64; r=json.loads(sys.stdin.read()); print(base64.b64decode(r["stdout"]).decode(), end="")'

say "snapshot (expect 501)"
SNAP_CODE=$(jc -o /dev/null -w '%{http_code}' -X POST "$BASE/v1/vm/$NAME/snapshot" -d '{"comment":"should-fail"}' || true)
if [ "$SNAP_CODE" = "501" ]; then
  echo "  got 501 (correct: snapshots unsupported for GPU passthrough)"
else
  echo "  FAIL: expected 501, got $SNAP_CODE" >&2; exit 1
fi

say "list snapshots (expect 501)"
LIST_CODE=$(jc -o /dev/null -w '%{http_code}' "$BASE/v1/vm/$NAME/snapshots" || true)
if [ "$LIST_CODE" = "501" ]; then
  echo "  got 501 (correct)"
else
  echo "  FAIL: expected 501, got $LIST_CODE" >&2; exit 1
fi

say "destroy"
curl -fsS -o /dev/null -w "delete: %{http_code}\n" "${AUTH[@]}" -X DELETE "$BASE/v1/vm/$NAME"

say "list after destroy (prefix=smoke)"
j "$BASE/v1/vm?prefix=smoke"; echo

printf '\n\033[1;32mOK\033[0m\n'
