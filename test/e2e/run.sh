#!/usr/bin/env bash
# End-to-end tests that drive a real orchestrator through Fusefiles.
#
# Unlike the Go unit tests, which stop at the compiler, every case here creates
# a real environment on a real host and then reads the guest back to check that
# what the Fusefile asked for is what the guest got.
#
#   ./test/e2e/run.sh                # every case
#   ./test/e2e/run.sh env files      # only cases whose name matches
#
# The CLI must already be pointed at an orchestrator (`fuse connect`) with a
# host selected (`fuse host <id>`). Override the binary when testing a build
# that is not the one on PATH:
#
#   FUSE_BIN=/tmp/fuse-main ./test/e2e/run.sh
#
# Every environment is created with a task id under E2E_PREFIX and destroyed on
# exit, including on interrupt. Nothing outside that prefix is touched.
set -uo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
FF="$HERE/fusefiles"
FUSE="${FUSE_BIN:-fuse}"
PREFIX="${E2E_PREFIX:-e2e}"

PASS=0; FAIL=0; SKIP=0
FAILED_CASES=()

ok()   { printf '  \033[32mok\033[0m   %s\n' "$1"; PASS=$((PASS + 1)); }
bad()  { printf '  \033[31mFAIL\033[0m %s\n' "$1"; FAIL=$((FAIL + 1)); FAILED_CASES+=("$1"); }
skip() { printf '  \033[33mskip\033[0m %s\n' "$1"; SKIP=$((SKIP + 1)); }
note() { printf '       %s\n' "$1"; }
head_() { printf '\n\033[1m== %s\033[0m\n' "$1"; }

WORK="$(mktemp -d)"

# Only cases matching one of the CLI arguments run. No arguments runs all.
want() {
  [ $# -eq 0 ] && return 0
  local name="$1"; shift
  [ $# -eq 0 ] && return 0
  local pat
  for pat in "$@"; do case "$name" in *"$pat"*) return 0 ;; esac; done
  return 1
}
SELECT=("$@")
# bash 3.2 treats an empty array as unset under `set -u`, hence the guard.
selected() { want "$1" ${SELECT[@]+"${SELECT[@]}"}; }

# --- environment lifecycle ---------------------------------------------------

# Every id this script creates is fuse-<prefix>-<case>, which is what cleanup
# keys off. A task id is the only handle `fuse up` gives us over the env id.
task_id() { printf '%s-%s' "$PREFIX" "$1"; }
env_id()  { printf 'fuse-%s-%s' "$PREFIX" "$1"; }

destroy_one() { "$FUSE" environment destroy "$1" --yes >/dev/null 2>&1; }

cleanup() {
  printf '\n-- cleanup --\n'
  local ids
  ids="$("$FUSE" -o json environment list 2>/dev/null \
    | grep -o "\"id\": *\"fuse-$PREFIX-[^\"]*\"" \
    | sed 's/.*"\(fuse-'"$PREFIX"'-[^"]*\)"/\1/')"
  if [ -z "$ids" ]; then
    printf '   nothing to clean\n'
  else
    local id
    for id in $ids; do
      printf '   destroying %s\n' "$id"
      destroy_one "$id"
    done
  fi
  rm -rf "$WORK"
}
trap cleanup EXIT INT TERM

# up <case> <fusefile> [extra up flags...] - create and wait. Logs to
# $WORK/<case>.log and returns the CLI's exit status.
up() {
  local case_name="$1" file="$2"; shift 2
  "$FUSE" up -f "$file" --task-id "$(task_id "$case_name")" "$@" \
    >"$WORK/$case_name.log" 2>&1
}

# guest <case> <shell command> - run a command inside the guest, print stdout.
guest() {
  local case_name="$1"; shift
  "$FUSE" environment exec "$(env_id "$case_name")" -- sh -lc "$*" 2>/dev/null
}

# expect <label> <expected> <actual>
expect() {
  if [ "$2" = "$3" ]; then
    ok "$1"
  else
    bad "$1"
    note "expected: $2"
    note "actual:   $3"
  fi
}

# contains <label> <needle> <haystack>
contains() {
  case "$3" in
    *"$2"*) ok "$1" ;;
    *) bad "$1"; note "wanted substring: $2"; note "in: $(printf '%s' "$3" | tr '\n' ' ')" ;;
  esac
}

# --- preflight ---------------------------------------------------------------

head_ "preflight"
if ! command -v "$FUSE" >/dev/null 2>&1 && [ ! -x "$FUSE" ]; then
  printf 'fuse binary not found: %s\n' "$FUSE" >&2
  exit 1
fi
note "binary:  $FUSE ($("$FUSE" -v 2>&1))"

STATUS="$("$FUSE" status 2>&1)"
if printf '%s' "$STATUS" | grep -q "no active context"; then
  printf 'no active context. run `fuse connect <url>` first.\n' >&2
  exit 1
fi
note "context: $(printf '%s' "$STATUS" | head -1)"

# A stale active_host makes environment list return nothing and still exit 0,
# which would make every assertion below fail for the same uninformative reason.
if ! printf '%s' "$STATUS" | grep -q "active host"; then
  printf 'no active host. run `fuse host <id>` first.\n' >&2
  exit 1
fi
note "host:    $(printf '%s' "$STATUS" | grep 'active host' | tr -s ' ')"

# The env block is resolved orchestrator side, so a CLI that can parse it is
# necessary but not sufficient; an orchestrator older than #173 drops it.
if ! "$FUSE" validate -f "$FF/Fusefile.env-secrets" >/dev/null 2>&1; then
  printf 'this fuse CLI cannot parse the env block (pre-#173). build from main.\n' >&2
  exit 1
fi

# --- 1. baseline -------------------------------------------------------------

if selected baseline; then
  head_ "baseline: schedule, boot, build, run, workspace"
  if up baseline "$FF/Fusefile.baseline"; then
    ok "environment created"
    expect "build steps ran"          "build ran"  "$(guest baseline 'cat /workspace/build.marker')"
    expect "run script ran"           "run ran"    "$(guest baseline 'cat /workspace/run.marker')"
    expect "build runs in workspace"  "/workspace" "$(guest baseline 'cat /workspace/cwd.marker')"
    # The scoped step declared workdir /tmp, so its relative write landed there
    # and the directory must not have leaked into the following step.
    expect "step workdir is scoped"   "scoped"     "$(guest baseline 'cat /tmp/scoped.marker')"
    expect "workdir did not leak"     "absent"     "$(guest baseline '[ -e /workspace/scoped.marker ] && echo present || echo absent')"
  else
    bad "environment created"; note "$(tail -5 "$WORK/baseline.log")"
  fi
fi

# --- 2. top-level env block + secrets ----------------------------------------

if selected env; then
  head_ "env block: literals, secret refs, /fuse/env"
  TOKEN="tok-$$-abcdef"
  if up env-secrets "$FF/Fusefile.env-secrets" --secret "api_token=$TOKEN"; then
    ok "environment created with secret supplied"
    expect "literal env reaches run"       "production" "$(guest env-secrets 'cat /workspace/run-mode.txt')"
    expect "secret env resolves in run"    "$TOKEN"     "$(guest env-secrets 'cat /workspace/run-token.txt')"
    expect "literal env reaches build"     "production" "$(guest env-secrets 'cat /workspace/build-mode.txt')"
    expect "secret env resolves in build"  "$TOKEN"     "$(guest env-secrets 'cat /workspace/build-token.txt')"

    # /fuse/env is the mechanism the whole block rests on.
    expect "/fuse/env exists"  "yes" "$(guest env-secrets '[ -f /fuse/env ] && echo yes || echo no')"

    # It holds resolved secrets in cleartext, so the other-bits of its mode
    # decide whether an unprivileged process in the guest can read them. The
    # last octal digit carries the read bit for 4, 5, 6 and 7.
    MODE="$(guest env-secrets 'stat -c %a /fuse/env')"
    case "$MODE" in
      *[4567]) bad "/fuse/env is not world readable"
               note "mode is $MODE: any process in the guest can read the secret"
               note "same applies to /fuse/auth-token, the guest agent credential" ;;
      *)       ok "/fuse/env is not world readable" ;;
    esac

    # The security property: the secret must not be recoverable from the
    # startup script, which is public on the host for the length of the boot.
    SCRIPT="$("$FUSE" compile -f "$FF/Fusefile.env-secrets" --only startup-script 2>/dev/null)"
    case "$SCRIPT" in
      *api_token*|*API_TOKEN=*) bad "secret name/value stays out of the startup script" ;;
      *) ok "secret name/value stays out of the startup script" ;;
    esac
    contains "startup script sources /fuse/env" ". /fuse/env" "$SCRIPT"
  else
    bad "environment created with secret supplied"; note "$(tail -5 "$WORK/env-secrets.log")"
  fi
fi

# --- 3. env block under the layer cache --------------------------------------

if selected cache-env; then
  head_ "env block + cache: does the build path see env?"
  note "schema documents env as set for every build step; compile.go keeps"
  note "/fuse/env out of BuildScript. this records which one is true."
  TOKEN="tok-$$-cached"
  if up cache-env "$FF/Fusefile.cache-env" --secret "api_token=$TOKEN"; then
    ok "environment created"
    CACHED_MODE="$(guest cache-env 'cat /workspace/cached-build-mode.txt')"
    BUILD_MODE="$(guest cache-env 'cat /workspace/build-mode.txt')"
    BUILD_TOK="$(guest cache-env 'cat /workspace/build-token.txt')"
    expect "literal env reaches run (cache on)" "production" "$(guest cache-env 'cat /workspace/run-mode.txt')"
    expect "literal env reaches uncacheable build steps" "production" "$BUILD_MODE"
    # The cacheable step is the one that goes through BuildScript.
    if [ "$CACHED_MODE" = "production" ]; then
      ok "literal env reaches cacheable build steps"
    else
      bad "literal env reaches cacheable build steps"
      note "cacheable step saw APP_MODE=$CACHED_MODE; uncacheable saw $BUILD_MODE"
      note "=> schema documents env for every build step, but the cached build"
      note "   path does not source /fuse/env. one of the two is wrong."
    fi
    if [ "$BUILD_TOK" = "$TOKEN" ]; then
      ok "secret env reaches cached build steps"
    else
      bad "secret env reaches cached build steps"
      note "build step saw API_TOKEN=$BUILD_TOK"
    fi
  else
    bad "environment created"; note "$(tail -5 "$WORK/cache-env.log")"
  fi
fi

# --- 4. files, copy, workspace ----------------------------------------------

if selected files; then
  head_ "files, copy, workspace resolution"
  if up files-copy "$FF/Fusefile.files-copy"; then
    ok "environment created"
    REPORT="$(guest files-copy 'cat /srv/work/report.txt')"
    contains "workspace honoured"            "workspace=/srv/work"   "$REPORT"
    contains "absolute file materialized"    "config=listen:"        "$REPORT"
    contains "relative file resolves to ws"  "relative=relative ok"  "$REPORT"
    contains "file mode applied"             "mode=600"              "$REPORT"
    contains "copy landed"                   "copied=v1.2.3"         "$REPORT"
    contains "copy walked directories"       "nested=helper ok"      "$REPORT"
    contains "chmod in build fixed the bit"  "entrypoint=entrypoint ok" "$REPORT"
  else
    bad "environment created"; note "$(tail -5 "$WORK/files-copy.log")"
  fi
fi

# --- 5. build cache and snapshots -------------------------------------------

if selected cache; then
  head_ "layer cache, fuse build, --from-build"

  PLAN="$("$FUSE" up -f "$FF/Fusefile.cache" --plan 2>&1)"
  if [ $? -eq 0 ]; then
    ok "--plan prints a cache plan without creating anything"
    note "$(printf '%s' "$PLAN" | tr '\n' ' ' | cut -c1-140)"
  else
    bad "--plan prints a cache plan without creating anything"; note "$PLAN"
  fi

  # First build populates the cache; the second must reuse it. layer3 stamps the
  # time it actually ran, so an unchanged stamp across two builds is reuse.
  BUILD1="$("$FUSE" build -f "$FF/Fusefile.cache" --name "$PREFIX-cache-1" 2>&1)"
  if [ $? -eq 0 ]; then
    ok "fuse build produced an artifact"
    SNAP="$(printf '%s' "$BUILD1" | grep -oE '[a-z0-9-]*snap[a-z0-9-]*|[0-9a-f]{8,}' | tail -1)"
    note "artifact: ${SNAP:-<could not parse id>}"

    BUILD2="$("$FUSE" build -f "$FF/Fusefile.cache" --name "$PREFIX-cache-2" 2>&1)"
    if [ $? -eq 0 ]; then
      if printf '%s' "$BUILD2" | grep -qiE "cached|reused|hit"; then
        ok "second build reports cache reuse"
      else
        bad "second build reports cache reuse"
        note "no cache/reuse/hit in output: $(printf '%s' "$BUILD2" | tr '\n' ' ' | cut -c1-160)"
      fi
    else
      bad "second build succeeded"; note "$(printf '%s' "$BUILD2" | tail -3)"
    fi

    if [ -n "$SNAP" ]; then
      if up from-build "$FF/Fusefile.cache" --from-build "$SNAP"; then
        ok "--from-build boots the artifact"
        expect "build output present without rebuilding" "layer one" \
          "$(guest from-build 'cat /var/lib/e2e/layer1.txt')"
      else
        bad "--from-build boots the artifact"; note "$(tail -5 "$WORK/from-build.log")"
      fi
    else
      skip "--from-build (no artifact id parsed)"
    fi
  else
    bad "fuse build produced an artifact"; note "$(printf '%s' "$BUILD1" | tail -5)"
  fi
fi

# --- 6. healthcheck and expose ----------------------------------------------

if selected health; then
  head_ "healthcheck verdict and expose"
  if up healthcheck "$FF/Fusefile.healthcheck"; then
    ok "environment created"
    expect "server is listening in guest" "healthy" \
      "$(guest healthcheck 'curl -sS -m 5 http://127.0.0.1:8080/health')"

    # fused writes its verdict to a file the orchestrator polls on a tick.
    expect "guest agent wrote a verdict" "passing" \
      "$(guest healthcheck 'sed -n "s/.*\"state\":\"\([a-z]*\)\".*/\1/p" /fuse/health.json')"

    # The verdict has to reach the control plane, not just be true in the guest.
    # Health is populated by a reconcile tick, not by create, so poll: asserting
    # straight after create reads a response that legitimately has no health key
    # yet and would report a bug that is not there.
    HEALTH=""
    for _ in 1 2 3 4 5 6 7 8 9 10 11 12; do
      DETAIL="$("$FUSE" -o json environment get "$(env_id healthcheck)" 2>/dev/null)"
      HEALTH="$(printf '%s' "$DETAIL" | tr -d ' \n' | grep -o '"health":{[^}]*}')"
      [ -n "$HEALTH" ] && break
      sleep 5
    done
    if [ -n "$HEALTH" ]; then
      ok "verdict reaches the orchestrator api"
      contains "reported state is passing" '"state":"passing"' "$HEALTH"
    else
      bad "verdict reaches the orchestrator api"
      note "no health key after 60s of polling"
    fi

    # expose publishes the port outside the guest: dial it from here, which is
    # the only assertion that proves the mapping rather than the listener.
    URL="$(printf '%s' "$DETAIL" | tr -d ' \n' | grep -o '"endpoints":\[{[^]]*' | grep -o '"url":"[^"]*"' | head -1 | sed 's/"url":"\(.*\)"/\1/')"
    if [ -n "$URL" ]; then
      note "published endpoint: $URL"
      BODY="$(curl -sS -m 10 "http://$URL/health" 2>/dev/null)"
      expect "exposed port is reachable from outside" "healthy" "$BODY"
    else
      bad "expose published an endpoint"
      note "no endpoints in: $(printf '%s' "$DETAIL" | tr '\n' ' ' | cut -c1-160)"
    fi
  else
    bad "environment created"; note "$(tail -5 "$WORK/healthcheck.log")"
  fi
fi

# --- 7. services -------------------------------------------------------------

if selected services; then
  head_ "services: compose on podman, service env secrets"
  if up services "$FF/Fusefile.services" --secret "redis_args=--maxmemory 64mb"; then
    ok "environment created"
    expect "run script ran" "services env up" "$(guest services 'cat /workspace/services.marker')"
    expect "compose provider present" "yes" \
      "$(guest services '[ -x /usr/local/bin/docker-compose ] && echo yes || echo no')"
    expect "compose project written" "yes" \
      "$(guest services '[ -f /fuse/compose.yaml ] && echo yes || echo no')"

    # The container is pulled and started by the guest agent at boot, so give it
    # a bounded window rather than asserting immediately.
    READY=no
    for _ in 1 2 3 4 5 6 7 8 9 10 11 12; do
      if [ "$(guest services 'podman ps --format "{{.Image}}" 2>/dev/null | grep -c redis')" != "0" ]; then
        READY=yes; break
      fi
      sleep 5
    done
    if [ "$READY" = "yes" ]; then
      ok "redis container is running"
      expect "redis answers" "PONG" "$(guest services 'podman exec $(podman ps -q --filter ancestor=docker.io/library/redis:7-alpine | head -1) redis-cli ping 2>/dev/null')"
    else
      bad "redis container is running"
      note "podman ps: $(guest services 'podman ps -a --format "{{.Image}} {{.Status}}" 2>&1' | tr '\n' ';')"
      note "compose log: $(guest services 'journalctl -u fused --no-pager 2>/dev/null | grep -i compose | tail -3' | tr '\n' ';')"
    fi
  else
    bad "environment created"
    if grep -q "docker.sock" "$WORK/services.log" 2>/dev/null; then
      note "KNOWN BUG: compose dials /var/run/docker.sock, which does not exist."
      note "the guest runs podman, whose socket is /run/podman/podman.sock, and"
      note "nothing sets DOCKER_HOST. fc-bake-rootfs.sh enables podman.socket"
      note "'because docker-compose needs the API', but the last hop was missed."
      note "fix: set DOCKER_HOST in the compose_up command, fc-agent.py:1187."
      note "verified by hand: with DOCKER_HOST set, the same compose file works."
      note "note the blast radius: this fails the whole create, not just the"
      note "service, so any Fusefile with services: cannot start an environment."
    else
      note "$(tail -8 "$WORK/services.log")"
    fi
  fi
fi

# --- 8. negative cases -------------------------------------------------------

if selected negative; then
  head_ "negative: the refusals"
  NEG="$FF/negative"

  # Parse-time. These must never reach an orchestrator.
  "$FUSE" validate -f "$NEG/Fusefile.both-build-setup" >/dev/null 2>&1 \
    && bad "build and setup together is rejected" \
    || ok "build and setup together is rejected"

  "$FUSE" validate -f "$NEG/Fusefile.bad-paths" >/dev/null 2>&1 \
    && bad "copy into /fuse is rejected" \
    || ok "copy into /fuse is rejected"

  # Client side, before any network call.
  OUT="$("$FUSE" validate -f "$NEG/Fusefile.missing-secret" --check-secrets 2>&1)"
  [ $? -ne 0 ] \
    && ok "missing secret fails --check-secrets" \
    || { bad "missing secret fails --check-secrets"; note "$OUT"; }

  OUT="$("$FUSE" up -f "$NEG/Fusefile.missing-secret" --task-id "$(task_id missing-secret)" 2>&1)"
  if [ $? -ne 0 ]; then
    ok "up refuses a missing secret"
    contains "refusal names the secret" "db_url" "$OUT"
  else
    bad "up refuses a missing secret"
    destroy_one "$(env_id missing-secret)"
  fi

  # Scheduler side. --dry-run would not exercise the scheduler, so these really
  # do post a create; the assertion is that it comes back refused.
  OUT="$("$FUSE" up -f "$NEG/Fusefile.overprovision" --task-id "$(task_id overprovision)" 2>&1)"
  if [ $? -ne 0 ]; then
    ok "over-provisioned cpu is refused"
    note "$(printf '%s' "$OUT" | tail -1 | cut -c1-120)"
  else
    bad "over-provisioned cpu is refused"
    note "created an environment asking for 512 cpus"
    destroy_one "$(env_id overprovision)"
  fi

  OUT="$("$FUSE" up -f "$NEG/Fusefile.gpu" --task-id "$(task_id gpu)" 2>&1)"
  if [ $? -ne 0 ]; then
    ok "gpu request on a gpu-less fleet is refused"
    # The refusal is correct; the reason given is not. This spec asks for 2 CPU
    # of the 12 free, so a bare capacity message sends the reader to look at
    # cpu/ram/disk when the gpu is what actually failed. scheduler.go already
    # splits out a label mismatch as "a selector mistake, not a capacity
    # shortfall" for exactly this reason; gpu has no such branch.
    if printf '%s' "$OUT" | grep -qi "gpu"; then
      ok "refusal names the gpu as the reason"
    else
      bad "refusal names the gpu as the reason"
      note "reported a capacity shortfall for a spec that fits: $(printf '%s' "$OUT" | grep -o 'need [^,]*' | head -1)"
      note "the host had 12 cpu free; the gpu is never mentioned"
      note "scheduler.go:591 builds this message from cpu/ram/storage only"
    fi
  else
    bad "gpu request on a gpu-less fleet is refused"
    note "scheduled a gpu workload onto a host with no gpu"
    destroy_one "$(env_id gpu)"
  fi

  OUT="$("$FUSE" up -f "$NEG/Fusefile.badhost" --task-id "$(task_id badhost)" 2>&1)"
  if [ $? -ne 0 ]; then
    ok "placement.host gate is not relaxed"
    note "$(printf '%s' "$OUT" | tail -1 | cut -c1-120)"
  else
    bad "placement.host gate is not relaxed"
    destroy_one "$(env_id badhost)"
  fi

  # A run that never returns must fail on the startup timeout.
  OUT="$("$FUSE" up -f "$NEG/Fusefile.blocking-run" --task-id "$(task_id blocking)" 2>&1)"
  if [ $? -ne 0 ]; then
    ok "a blocking run fails on the startup timeout"
    note "$(printf '%s' "$OUT" | tail -1 | cut -c1-120)"
  else
    bad "a blocking run fails on the startup timeout"
    destroy_one "$(env_id blocking)"
  fi
fi

# --- summary -----------------------------------------------------------------

printf '\n\033[1m%d passed, %d failed, %d skipped\033[0m\n' "$PASS" "$FAIL" "$SKIP"
if [ "$FAIL" -gt 0 ]; then
  printf 'failed:\n'
  for c in "${FAILED_CASES[@]}"; do printf '  - %s\n' "$c"; done
fi
[ "$FAIL" -eq 0 ]
