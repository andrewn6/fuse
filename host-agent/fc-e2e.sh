#!/usr/bin/env bash
# Full Fuse deploy test: boots the real ./bin/fuse binary and drives a complete
# environment lifecycle over HTTP (create → get → list → snapshot → restore →
# drain → destroy), asserting each step.
#
# Modes:
#   (default)      local — in-memory stub provider, no host/KVM needed.
#   FUSE_E2E_REMOTE=1   deploy real microVMs against the host in ../.env
#                       (FIRECRACKER_BASE_URL / FIRECRACKER_TOKEN). Network +
#                       a Fuse-compatible host required.
#
# Exit 0 = everything works. Any failed assertion exits non-zero.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

PORT="${FUSE_E2E_PORT:-18080}"
BASE="http://127.0.0.1:${PORT}"
BIN="$REPO_ROOT/bin/fuse"
TASK="e2e-$$"

pass() { printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; }
step() { printf '\033[1;36m== %s ==\033[0m\n' "$*"; }
die()  { printf '\033[1;31m  ✗ %s\033[0m\n' "$*" >&2; exit 1; }

# code METHOD URL [json-body] -> echoes "<http_code>\n<body>"
http() {
  local method="$1" url="$2" body="${3:-}"
  if [ -n "$body" ]; then
    curl -sS -m 30 -X "$method" "$url" -H 'Content-Type: application/json' -d "$body" -w $'\n%{http_code}'
  else
    curl -sS -m 30 -X "$method" "$url" -w $'\n%{http_code}'
  fi
}
code_of() { printf '%s' "$1" | tail -n1; }
body_of() { printf '%s' "$1" | sed '$d'; }
# crude JSON string field extractor (no jq dependency)
field() { printf '%s' "$1" | grep -oE "\"$2\"[[:space:]]*:[[:space:]]*\"[^\"]*\"" | head -n1 | sed -E "s/.*:[[:space:]]*\"([^\"]*)\"/\1/"; }

step "build"
if command -v go >/dev/null 2>&1; then
  go build -o "$BIN" ./orchestrator
  pass "built $BIN"
elif [ -x "$BIN" ]; then
  pass "using prebuilt $BIN (Go not installed on this host)"
else
  die "Go not installed and no prebuilt binary at $BIN — cross-build ./orchestrator elsewhere (GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o bin/fuse ./orchestrator) and copy it here"
fi

# Environment for the server process. A token-encryption key makes Boot generate
# per-VM TLS creds + auth token (the full secure deploy path).
ENVV=(ORCH_LISTEN=":${PORT}" "TOKEN_ENCRYPTION_KEY=$(openssl rand -hex 32)")
MODE="stub (in-memory provider)"
if [ "${FUSE_E2E_REMOTE:-0}" = "1" ]; then
  [ -f "$REPO_ROOT/.env" ] || die ".env not found for remote mode"
  # shellcheck disable=SC1091
  set -a; source "$REPO_ROOT/.env"; set +a
  [ -n "${FIRECRACKER_BASE_URL:-}" ] || die "FIRECRACKER_BASE_URL unset in .env"
  ENVV+=("FIRECRACKER_BASE_URL=${FIRECRACKER_BASE_URL}" "FIRECRACKER_TOKEN=${FIRECRACKER_TOKEN:-}")
  MODE="remote ${FIRECRACKER_BASE_URL}"
fi

step "boot fuse — ${MODE}"
env "${ENVV[@]}" "$BIN" >/tmp/fuse-e2e.log 2>&1 &
SRV_PID=$!
cleanup() { kill "$SRV_PID" 2>/dev/null || true; wait "$SRV_PID" 2>/dev/null || true; }
trap cleanup EXIT

# Wait for readiness.
for i in $(seq 1 50); do
  if curl -sS -m 2 "$BASE/ready" >/dev/null 2>&1; then break; fi
  if ! kill -0 "$SRV_PID" 2>/dev/null; then echo "--- server log ---"; cat /tmp/fuse-e2e.log; die "server exited during boot"; fi
  sleep 0.2
  [ "$i" = 50 ] && { cat /tmp/fuse-e2e.log; die "server never became ready"; }
done
pass "server up on $BASE (pid $SRV_PID)"

step "probes"
[ "$(curl -sS -m 5 -o /dev/null -w '%{http_code}' "$BASE/health")" = "200" ] || die "/health not 200"; pass "/health 200"
[ "$(curl -sS -m 5 -o /dev/null -w '%{http_code}' "$BASE/ready")"  = "200" ] || die "/ready not 200";  pass "/ready 200"
[ "$(curl -sS -m 5 -o /dev/null -w '%{http_code}' "$BASE/metrics")" = "200" ] || die "/metrics not 200"; pass "/metrics 200"

step "create (deploy)"
R=$(http POST "$BASE/v1/environments" "{\"task_id\":\"$TASK\",\"spec\":{\"cpus\":2,\"ram_mb\":512,\"storage_gb\":1,\"region\":\"local\"}}")
[ "$(code_of "$R")" = "201" ] || { echo "$(body_of "$R")"; die "create != 201 (got $(code_of "$R"))"; }
ID=$(field "$(body_of "$R")" id)
STATE=$(field "$(body_of "$R")" state)
[ -n "$ID" ] || die "no id in create response"
[ "$STATE" = "running" ] || die "create state=$STATE want running"
pass "deployed $ID (state=$STATE)"

step "get"
R=$(http GET "$BASE/v1/environments/$ID"); [ "$(code_of "$R")" = "200" ] || die "get != 200"
[ "$(field "$(body_of "$R")" state)" = "running" ] || die "get state != running"; pass "get 200 running"

step "list"
R=$(http GET "$BASE/v1/environments?task_id=$TASK"); [ "$(code_of "$R")" = "200" ] || die "list != 200"
printf '%s' "$(body_of "$R")" | grep -q "\"$ID\"" || die "list missing $ID"; pass "list contains $ID"

step "snapshot"
R=$(http POST "$BASE/v1/environments/$ID/snapshots" '{"comment":"e2e"}')
[ "$(code_of "$R")" = "201" ] || { echo "$(body_of "$R")"; die "snapshot != 201"; }
SNAP=$(field "$(body_of "$R")" id); [ -n "$SNAP" ] || die "no snapshot id"; pass "snapshot $SNAP"

step "list snapshots"
R=$(http GET "$BASE/v1/snapshots?vm_id=$ID"); [ "$(code_of "$R")" = "200" ] || die "list snapshots != 200"
printf '%s' "$(body_of "$R")" | grep -q "\"$SNAP\"" || die "snapshot $SNAP not listed"; pass "snapshot listed"

step "restore"
R=$(http POST "$BASE/v1/snapshots/$SNAP?action=restore"); [ "$(code_of "$R")" = "204" ] || die "restore != 204 (got $(code_of "$R"))"; pass "restore 204"

step "drain"
R=$(http POST "$BASE/v1/environments/$ID?action=drain"); [ "$(code_of "$R")" = "200" ] || { echo "$(body_of "$R")"; die "drain != 200"; }
[ "$(field "$(body_of "$R")" state)" = "draining" ] || die "drain state != draining"; pass "drain 200 draining"

step "destroy"
R=$(http DELETE "$BASE/v1/environments/$ID"); [ "$(code_of "$R")" = "204" ] || die "destroy != 204 (got $(code_of "$R"))"; pass "destroy 204"

step "confirm gone"
R=$(http GET "$BASE/v1/environments/$ID"); [ "$(code_of "$R")" = "404" ] || die "get after destroy != 404"; pass "404 after destroy"

printf '\n\033[1;32m✓ FULL FUSE DEPLOY TEST PASSED (%s)\033[0m\n' "$MODE"
