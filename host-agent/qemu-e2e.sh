#!/usr/bin/env bash
# Full Fuse GPU deploy test: boots the real ./bin/fuse binary, registers a QEMU
# host, deploys an environment that requests a GPU, and asserts the GPU is
# visible in the guest (nvidia-smi). Sibling to fc-e2e.sh.
#
# Hardware-gated: needs a reachable QEMU host agent (in ../.env as
# QEMU_BASE_URL / QEMU_TOKEN) with a bindable, vfio-bound GPU. Without one, the
# script logs SKIP and exits 0 -- it never passes silently and never fails CI
# on a box that has no GPU.
#
# Enable with FUSE_GPU_E2E=1. Exit 0 = passed or cleanly skipped. Any failed
# assertion exits non-zero.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

PORT="${FUSE_E2E_PORT:-18081}"
BASE="http://127.0.0.1:${PORT}"
BIN="$REPO_ROOT/bin/fuse"
TASK="gpu-e2e-$$"
HOST_ID="gpu-e2e-host-$$"
GPU_KIND="${FUSE_GPU_KIND:-a100}"

pass() { printf '\033[1;32m  ✓ %s\033[0m\n' "$*"; }
step() { printf '\033[1;36m== %s ==\033[0m\n' "$*"; }
skip() { printf '\033[1;33m  ⊘ SKIP: %s\033[0m\n' "$*"; exit 0; }
die()  { printf '\033[1;31m  ✗ %s\033[0m\n' "$*" >&2; exit 1; }

# code METHOD URL [json-body] -> echoes "<body>\n<http_code>"
http() {
  local method="$1" url="$2" body="${3:-}"
  if [ -n "$body" ]; then
    curl -sS -m 60 -X "$method" "$url" -H 'Content-Type: application/json' -d "$body" -w $'\n%{http_code}'
  else
    curl -sS -m 60 -X "$method" "$url" -w $'\n%{http_code}'
  fi
}
qemu_http() {
  local method="$1" url="$2" body="${3:-}"
  local auth=(-H "Authorization: Bearer ${QEMU_TOKEN}")
  if [ -n "$body" ]; then
    curl -sS -m 60 -X "$method" "$url" "${auth[@]}" -H 'Content-Type: application/json' -d "$body" -w $'\n%{http_code}'
  else
    curl -sS -m 60 -X "$method" "$url" "${auth[@]}" -w $'\n%{http_code}'
  fi
}
code_of() { printf '%s' "$1" | tail -n1; }
body_of() { printf '%s' "$1" | sed '$d'; }
# crude JSON string field extractor (no jq dependency)
field() { printf '%s' "$1" | grep -oE "\"$2\"[[:space:]]*:[[:space:]]*\"[^\"]*\"" | head -n1 | sed -E "s/.*:[[:space:]]*\"([^\"]*)\"/\1/"; }

# -- Gate: this test only runs when explicitly enabled with a GPU host present.
step "gate"
[ "${FUSE_GPU_E2E:-0}" = "1" ] || skip "FUSE_GPU_E2E != 1 (set it to run the gpu e2e)"
[ -f "$REPO_ROOT/.env" ] || skip "no .env with QEMU_BASE_URL / QEMU_TOKEN"
# shellcheck disable=SC1091
set -a; source "$REPO_ROOT/.env"; set +a
[ -n "${QEMU_BASE_URL:-}" ] || skip "QEMU_BASE_URL unset in .env (no qemu gpu host to target)"
[ -n "${QEMU_TOKEN:-}" ] || skip "QEMU_TOKEN unset in .env"
# Confirm the qemu host agent is reachable before we bother booting fuse.
if ! curl -sS -m 5 "${QEMU_BASE_URL%/}/healthz" >/dev/null 2>&1; then
  skip "qemu host agent at $QEMU_BASE_URL not reachable"
fi
pass "gpu host agent reachable at $QEMU_BASE_URL"

step "build"
if command -v go >/dev/null 2>&1; then
  go build -o "$BIN" ./orchestrator
  pass "built $BIN"
elif [ -x "$BIN" ]; then
  pass "using prebuilt $BIN (Go not installed on this host)"
else
  die "Go not installed and no prebuilt binary at $BIN"
fi

# A token-encryption key makes Boot generate per-VM TLS creds + auth token.
ENVV=(ORCH_LISTEN=":${PORT}" "TOKEN_ENCRYPTION_KEY=$(openssl rand -hex 32)")

step "boot fuse"
env "${ENVV[@]}" "$BIN" >/tmp/fuse-gpu-e2e.log 2>&1 &
SRV_PID=$!
cleanup() {
  # Best-effort teardown: destroy the env and deregister the host before exit.
  [ -n "${ID:-}" ] && curl -sS -m 10 -X DELETE "$BASE/v1/environments/$ID" >/dev/null 2>&1 || true
  curl -sS -m 10 -X DELETE "$BASE/v1/hosts/$HOST_ID" >/dev/null 2>&1 || true
  kill "$SRV_PID" 2>/dev/null || true
  wait "$SRV_PID" 2>/dev/null || true
}
trap cleanup EXIT

# Wait for readiness.
for i in $(seq 1 50); do
  if curl -sS -m 2 "$BASE/ready" >/dev/null 2>&1; then break; fi
  if ! kill -0 "$SRV_PID" 2>/dev/null; then echo "--- server log ---"; cat /tmp/fuse-gpu-e2e.log; die "server exited during boot"; fi
  sleep 0.2
  [ "$i" = 50 ] && { cat /tmp/fuse-gpu-e2e.log; die "server never became ready"; }
done
pass "server up on $BASE (pid $SRV_PID)"

step "register qemu gpu host"
R=$(http POST "$BASE/v1/hosts" "{\"id\":\"$HOST_ID\",\"url\":\"$QEMU_BASE_URL\",\"token\":\"${QEMU_TOKEN:-}\",\"backend\":\"qemu\",\"capacity\":{\"cpus\":8,\"ram_mb\":16384,\"storage_gb\":100,\"vm_count\":4,\"gpus\":1,\"gpu_kind\":\"$GPU_KIND\"}}")
[ "$(code_of "$R")" = "201" ] || { echo "$(body_of "$R")"; die "register host != 201 (got $(code_of "$R"))"; }
pass "registered qemu host $HOST_ID (gpus=1 kind=$GPU_KIND)"

step "deploy gpu environment"
R=$(http POST "$BASE/v1/environments" "{\"task_id\":\"$TASK\",\"spec\":{\"cpus\":4,\"ram_mb\":8192,\"storage_gb\":20,\"gpus\":1,\"gpu_kind\":\"$GPU_KIND\"}}")
[ "$(code_of "$R")" = "201" ] || { echo "$(body_of "$R")"; die "create != 201 (got $(code_of "$R")); gpu env may not have scheduled"; }
ID=$(field "$(body_of "$R")" id)
STATE=$(field "$(body_of "$R")" state)
[ -n "$ID" ] || die "no id in create response"
[ "$STATE" = "running" ] || die "create state=$STATE want running"
pass "deployed $ID on the gpu host (state=$STATE)"

step "assert placed on the qemu host"
R=$(http GET "$BASE/v1/environments/$ID"); [ "$(code_of "$R")" = "200" ] || die "get != 200"
HOST=$(field "$(body_of "$R")" host_id)
[ "$HOST" = "$HOST_ID" ] || die "env host_id=$HOST want $HOST_ID (gpu env landed on the wrong host)"
pass "env is on $HOST_ID"

step "nvidia-smi in guest"
R=$(qemu_http POST "${QEMU_BASE_URL%/}/v1/vm/$ID/exec" '{"cmd":["nvidia-smi","-L"]}')
[ "$(code_of "$R")" = "200" ] || { echo "$(body_of "$R")"; die "exec nvidia-smi != 200"; }
# do_exec returns base64 stdout; decode and confirm a GPU is listed.
OUT_B64=$(field "$(body_of "$R")" stdout)
OUT=$(printf '%s' "$OUT_B64" | base64 -d 2>/dev/null || printf '%s' "$OUT_B64")
printf '%s' "$OUT" | grep -qi 'GPU 0' || { echo "$OUT"; die "nvidia-smi did not list a GPU in the guest"; }
pass "guest sees a GPU: $(printf '%s' "$OUT" | head -n1)"

step "snapshot is refused (d4)"
R=$(http POST "$BASE/v1/environments/$ID/snapshots" '{"comment":"should-fail"}')
CODE=$(code_of "$R")
[ "$CODE" != "201" ] || die "snapshot of a gpu env unexpectedly succeeded"
pass "snapshot refused for gpu env (got $CODE)"

step "destroy"
R=$(http DELETE "$BASE/v1/environments/$ID"); [ "$(code_of "$R")" = "204" ] || die "destroy != 204 (got $(code_of "$R"))"
R=$(qemu_http GET "${QEMU_BASE_URL%/}/v1/vm/$ID")
[ "$(code_of "$R")" = "404" ] || die "qemu vm still exists after orchestrator destroy"
ID=""  # clear so cleanup doesn't double-delete
pass "destroy 204 and qemu vm removed"

printf '\n\033[1;32m✓ FUSE GPU DEPLOY TEST PASSED (qemu host %s)\033[0m\n' "$QEMU_BASE_URL"
