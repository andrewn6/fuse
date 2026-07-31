#!/usr/bin/env bash
# End-to-end smoke test of the Fuse fc-agent contract against the local host agent.
set -euo pipefail
FC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# Token is written to .env by fc-agent.sh (legacy: .fc-agent.env). Prefer .env.
ENV_FILE="$FC_DIR/.env"
[ -f "$ENV_FILE" ] || ENV_FILE="$FC_DIR/.fc-agent.env"
[ -f "$ENV_FILE" ] || { echo "no agent env file found at $FC_DIR/.env — run ./fc-agent.sh start first" >&2; exit 1; }
# shellcheck disable=SC1090
source "$ENV_FILE"
BASE="http://127.0.0.1:${FC_AGENT_PORT:-8090}"
AUTH=(-H "Authorization: Bearer $FC_AGENT_TOKEN" -H "Content-Type: application/json")
NAME="smoke-$(date +%s)"

say() { printf '\n\033[1;36m== %s ==\033[0m\n' "$*"; }

j() { curl -fsS "${AUTH[@]}" "$@"; }

say "health"
j "$BASE/healthz"; echo

say "capacity"
j "$BASE/v1/capacity"; echo

say "create $NAME"
j -X POST "$BASE/v1/vm" -d "{\"name\":\"$NAME\",\"cpus\":1,\"memory_mb\":512,\"storage_gb\":1,\"region\":\"local\"}"
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

say "snapshot"
SNAP=$(j -X POST "$BASE/v1/vm/$NAME/snapshot" -d '{"comment":"smoke","include_ram":false}')
echo "$SNAP"
SNAP_ID=$(echo "$SNAP" | python3 -c 'import sys,json; print(json.loads(sys.stdin.read())["snapshot_id"])')

say "list snapshots"
j "$BASE/v1/vm/$NAME/snapshots"; echo

say "exec: modify file then restore"
j -X POST "$BASE/v1/vm/$NAME/exec" -d '{"cmd":["/bin/sh","-lc","echo CHANGED > /root/hello.txt"]}' >/dev/null
j -X POST "$BASE/v1/vm/$NAME/restore" -d "{\"snapshot_id\":\"$SNAP_ID\",\"include_ram\":false}"
echo
say "exec: verify restore rolled back the file"
RESP=$(j -X POST "$BASE/v1/vm/$NAME/exec" -d '{"cmd":["/bin/sh","-lc","cat /root/hello.txt"]}')
echo "$RESP" | python3 -c 'import sys,json,base64; r=json.loads(sys.stdin.read()); sys.stdout.write(base64.b64decode(r["stdout"]).decode())'

say "destroy"
curl -fsS -o /dev/null -w "delete: %{http_code}\n" "${AUTH[@]}" -X DELETE "$BASE/v1/vm/$NAME"

say "list after destroy (prefix=smoke)"
j "$BASE/v1/vm?prefix=smoke"; echo

printf '\n\033[1;32mOK\033[0m\n'
