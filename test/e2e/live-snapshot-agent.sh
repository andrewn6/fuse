#!/usr/bin/env bash
# Agent-level end-to-end tests for live (memory) snapshots.
#
# This is deliberately NOT part of run.sh. That suite drives a real
# orchestrator through the CLI, and live snapshots have no CLI or API surface
# yet -- the host agent learned them first, on purpose, so the wire can be
# exercised before anything upstream depends on it. This script therefore talks
# to fc-agent directly over its own HTTP API.
#
#   FC_AGENT_URL=http://<host>:9000 FC_AGENT_TOKEN=<token> ./test/e2e/live-snapshot-agent.sh
#
# It creates one VM under a name prefixed E2E_PREFIX, and destroys it and every
# snapshot it took on exit, including on interrupt. Nothing outside that prefix
# is touched.
#
# What makes these tests decisive is /dev/shm. It is tmpfs, so it lives in
# guest RAM and never reaches the rootfs. A marker written there survives a
# live restore and cannot survive a disk restore. Checking a file on disk would
# prove nothing: both kinds of snapshot carry the disk.
set -uo pipefail

FC_URL="${FC_AGENT_URL:-}"
FC_TOKEN="${FC_AGENT_TOKEN:-}"
PREFIX="${E2E_PREFIX:-e2e}"
VM="${PREFIX}-livesnap-$$"

PASS=0; FAIL=0
FAILED_CASES=()

ok()    { printf '  \033[32mok\033[0m   %s\n' "$1"; PASS=$((PASS + 1)); }
bad()   { printf '  \033[31mFAIL\033[0m %s\n' "$1"; FAIL=$((FAIL + 1)); FAILED_CASES+=("$1"); }
note()  { printf '       %s\n' "$1"; }
head_() { printf '\n\033[1m== %s\033[0m\n' "$1"; }

if [ -z "$FC_URL" ] || [ -z "$FC_TOKEN" ]; then
  echo "FC_AGENT_URL and FC_AGENT_TOKEN must both be set" >&2
  exit 2
fi

api() { # api <method> <path> [json-body]
  local method="$1" path="$2" body="${3:-}"
  if [ -n "$body" ]; then
    curl -sS -X "$method" "$FC_URL$path" \
      -H "Authorization: Bearer $FC_TOKEN" \
      -H "Content-Type: application/json" -d "$body"
  else
    curl -sS -X "$method" "$FC_URL$path" -H "Authorization: Bearer $FC_TOKEN"
  fi
}

jqr() { python3 -c 'import json,sys; print(json.load(sys.stdin).get(sys.argv[1], ""))' "$1"; }

# guest_sh runs a shell line in the guest and echoes stdout. The agent's exec
# takes an argv array, so the shell is explicit rather than implied.
guest_sh() {
  api POST "/v1/vm/$VM/exec" \
    "$(python3 -c 'import json,sys; print(json.dumps({"cmd":["sh","-c",sys.argv[1]],"timeout_ms":30000}))' "$1")" \
    | python3 -c 'import json,sys; d=json.load(sys.stdin); sys.stdout.write(d.get("stdout",""))'
}

cleanup() {
  head_ "cleanup"
  api DELETE "/v1/vm/$VM" >/dev/null 2>&1 && note "destroyed $VM" || note "no vm to destroy"
}
trap cleanup EXIT INT TERM

head_ "create $VM"
CREATE=$(api POST /v1/vm "{\"name\":\"$VM\",\"cpus\":1,\"memory_mb\":512}")
if [ -z "$(echo "$CREATE" | jqr vm_id)" ]; then
  echo "create failed: $CREATE" >&2
  exit 1
fi
note "created, waiting for guest"
sleep 2

# ---------------------------------------------------------------------------
head_ "live snapshot captures guest memory"

guest_sh 'echo in-ram-at-snapshot > /dev/shm/marker' >/dev/null
guest_sh 'echo on-disk-at-snapshot > /root/diskmarker' >/dev/null
BEFORE_SHM=$(guest_sh 'cat /dev/shm/marker 2>/dev/null' | tr -d '\r\n')
[ "$BEFORE_SHM" = "in-ram-at-snapshot" ] \
  && ok "marker is in tmpfs before snapshot" \
  || bad "could not seed /dev/shm marker (got '$BEFORE_SHM')"

UPTIME_BEFORE=$(guest_sh 'cut -d. -f1 /proc/uptime' | tr -d '\r\n')

SNAP_JSON=$(api POST "/v1/vm/$VM/snapshot" '{"comment":"e2e live","live":true}')
SNAP_LIVE=$(echo "$SNAP_JSON" | jqr snapshot_id)
SNAP_KIND=$(echo "$SNAP_JSON" | jqr kind)
SNAP_SIZE=$(echo "$SNAP_JSON" | jqr size_bytes)

[ "$SNAP_KIND" = "live" ] \
  && ok "snapshot reports kind=live" \
  || bad "snapshot kind is '$SNAP_KIND', want live"

# A live snapshot carries a whole memory image, so it must be larger than the
# 512 MiB of guest RAM alone. This is the cheapest proof the mem file was
# actually written rather than silently skipped.
if [ -n "$SNAP_SIZE" ] && [ "$SNAP_SIZE" -gt $((512 * 1024 * 1024)) ]; then
  ok "size_bytes ($SNAP_SIZE) exceeds guest RAM, memory image was written"
else
  bad "size_bytes is $SNAP_SIZE, too small to contain a 512 MiB memory image"
fi

# ---------------------------------------------------------------------------
head_ "live restore brings memory back"

guest_sh 'rm -f /dev/shm/marker /root/diskmarker' >/dev/null
GONE=$(guest_sh 'cat /dev/shm/marker 2>/dev/null' | tr -d '\r\n')
[ -z "$GONE" ] && ok "marker cleared before restore" || bad "marker survived deletion"

api POST "/v1/vm/$VM/restore" "{\"snapshot_id\":\"$SNAP_LIVE\"}" >/dev/null
sleep 2

AFTER_SHM=$(guest_sh 'cat /dev/shm/marker 2>/dev/null' | tr -d '\r\n')
[ "$AFTER_SHM" = "in-ram-at-snapshot" ] \
  && ok "tmpfs marker restored, guest memory came back" \
  || bad "tmpfs marker is '$AFTER_SHM' after live restore, memory was NOT restored"

AFTER_DISK=$(guest_sh 'cat /root/diskmarker 2>/dev/null' | tr -d '\r\n')
[ "$AFTER_DISK" = "on-disk-at-snapshot" ] \
  && ok "disk marker restored alongside memory" \
  || bad "disk marker is '$AFTER_DISK', disk and memory are out of step"

# A resumed guest did not reboot, so its uptime continues from the snapshot
# rather than restarting near zero. This separates a real resume from a cold
# boot that merely happened to have the right disk.
UPTIME_AFTER=$(guest_sh 'cut -d. -f1 /proc/uptime' | tr -d '\r\n')
if [ -n "$UPTIME_AFTER" ] && [ "$UPTIME_AFTER" -ge "${UPTIME_BEFORE:-0}" ]; then
  ok "uptime continued across restore (${UPTIME_BEFORE}s -> ${UPTIME_AFTER}s), guest resumed"
else
  bad "uptime went ${UPTIME_BEFORE}s -> ${UPTIME_AFTER}s, that is a cold boot not a resume"
fi

# ---------------------------------------------------------------------------
head_ "clock is corrected on resume"

# resync_guest_clock pushes the host's UTC time in after resume. Without it the
# guest wakes at snapshot time and every certificate check in it is wrong.
HOST_EPOCH=$(date -u +%s)
GUEST_EPOCH=$(guest_sh 'date -u +%s' | tr -d '\r\n')
if [ -n "$GUEST_EPOCH" ]; then
  DRIFT=$(( HOST_EPOCH > GUEST_EPOCH ? HOST_EPOCH - GUEST_EPOCH : GUEST_EPOCH - HOST_EPOCH ))
  if [ "$DRIFT" -lt 60 ]; then
    ok "guest clock within ${DRIFT}s of host"
  else
    bad "guest clock is ${DRIFT}s off the host, resync did not take"
  fi
else
  bad "could not read guest clock"
fi

# ---------------------------------------------------------------------------
head_ "the same live snapshot restores twice"

# Firecracker maps the memory file when it loads a snapshot. If it ever mapped
# it shared, the resumed guest's first write would scribble through into the
# artifact and the snapshot would be single-use -- a rollback point you can
# only roll back to once, which is not a rollback point. Restoring the same id
# a second time is the only honest way to find that out.
guest_sh 'rm -f /dev/shm/marker' >/dev/null
api POST "/v1/vm/$VM/restore" "{\"snapshot_id\":\"$SNAP_LIVE\"}" >/dev/null
sleep 2

SECOND=$(guest_sh 'cat /dev/shm/marker 2>/dev/null' | tr -d '\r\n')
[ "$SECOND" = "in-ram-at-snapshot" ] \
  && ok "second restore from the same snapshot works, artifact is reusable" \
  || bad "second restore gave '$SECOND', the memory image did not survive being restored from"

# ---------------------------------------------------------------------------
head_ "disk snapshots still behave as before"

guest_sh 'echo ram-only > /dev/shm/marker2' >/dev/null
DISK_JSON=$(api POST "/v1/vm/$VM/snapshot" '{"comment":"e2e disk"}')
DISK_KIND=$(echo "$DISK_JSON" | jqr kind)
DISK_SNAP=$(echo "$DISK_JSON" | jqr snapshot_id)

[ "$DISK_KIND" = "disk" ] \
  && ok "omitting live still yields a disk snapshot" \
  || bad "default snapshot kind is '$DISK_KIND', want disk"

api POST "/v1/vm/$VM/restore" "{\"snapshot_id\":\"$DISK_SNAP\"}" >/dev/null
sleep 5

DISK_SHM=$(guest_sh 'cat /dev/shm/marker2 2>/dev/null' | tr -d '\r\n')
[ -z "$DISK_SHM" ] \
  && ok "disk restore cold-booted, tmpfs is empty as expected" \
  || bad "tmpfs survived a disk restore ('$DISK_SHM'), that should be impossible"

# ---------------------------------------------------------------------------
printf '\n\033[1m%d passed, %d failed\033[0m\n' "$PASS" "$FAIL"
if [ "$FAIL" -gt 0 ]; then
  for c in "${FAILED_CASES[@]}"; do printf '  \033[31m-\033[0m %s\n' "$c"; done
  exit 1
fi
