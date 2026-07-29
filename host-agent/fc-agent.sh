#!/usr/bin/env bash
# start/stop/env wrapper for the Fuse Firecracker host agent.
set -euo pipefail
FC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PORT="${FC_AGENT_PORT:-8090}"
ENV_FILE="$FC_DIR/.env"
LEGACY_ENV_FILE="$FC_DIR/.fc-agent.env"
PID_FILE="$FC_DIR/.fc-agent.pid"
LOG="$FC_DIR/fc-agent.log"
# co-located orchestrator (control plane) install targets; ORCH_BIN overridable
ORCH_BIN="${ORCH_BIN:-/usr/local/bin/orchestrator}"
ORCH_DEFAULTS=/etc/default/orchestrator
ORCH_UNIT=/etc/systemd/system/orchestrator.service

# >>> fuse release-asset checksum verification >>>
# This block is embedded, not sourced: scripts/install-orchestrator.sh is
# documented as curl-able and run standalone on a fresh host, so a sibling
# library file is not guaranteed to exist on disk. Keep the copies in
# host-agent/fc-update.sh, host-agent/fc-agent.sh and
# scripts/install-orchestrator.sh byte-identical;
# host-agent/test-verify-checksum.sh asserts that and exercises the helper.

# sha256_of <file> - print the file's lowercase sha256, or fail if no tool.
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | cut -d' ' -f1
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | cut -d' ' -f1
  else
    return 1
  fi
}

# verify_asset <file> <asset-name> <checksums-file>
# Fails closed. A missing checksum tool, a missing or empty checksums file, a
# missing entry for this exact asset name, or a digest mismatch all return
# non-zero, so the caller must not install, extract, or execute the asset.
verify_asset() {
  local file="$1" name="$2" sums="$3" want="" got="" sum path
  if ! command -v sha256sum >/dev/null 2>&1 && ! command -v shasum >/dev/null 2>&1; then
    echo "checksum verification needs sha256sum or shasum; refusing $name" >&2
    return 1
  fi
  if [ ! -f "$file" ]; then
    echo "asset $name is missing; nothing to verify" >&2
    return 1
  fi
  if [ ! -s "$sums" ]; then
    echo "checksums.txt is missing or empty; refusing $name" >&2
    return 1
  fi
  # goreleaser writes "<sha256>  <asset>"; binary-mode tools prefix the name "*".
  while read -r sum path; do
    case "$path" in
      "$name"|"*$name") want="$sum"; break ;;
    esac
  done < "$sums"
  if [ -z "$want" ]; then
    echo "no checksum entry for $name in checksums.txt; refusing it" >&2
    return 1
  fi
  got="$(sha256_of "$file")" || return 1
  if [ "$want" != "$got" ]; then
    echo "checksum mismatch for $name: want $want, got $got" >&2
    return 1
  fi
}
# <<< fuse release-asset checksum verification <<<

cmd="${1:-start}"
public_ip() { curl -fsS ifconfig.me 2>/dev/null || hostname -I | awk '{print $1}'; }

ensure_token() {
  if [ ! -f "$ENV_FILE" ] && [ -f "$LEGACY_ENV_FILE" ]; then
    mv "$LEGACY_ENV_FILE" "$ENV_FILE"
  fi
  if [ ! -f "$ENV_FILE" ]; then
    (umask 077 && echo "FC_AGENT_TOKEN=$(openssl rand -hex 32)" > "$ENV_FILE")
  fi
  chmod 600 "$ENV_FILE"
}

case "$cmd" in
  start)
    if [ -f "$PID_FILE" ] && kill -0 "$(cat "$PID_FILE")" 2>/dev/null; then
      echo "[fc-agent] already running (pid $(cat "$PID_FILE"))"
    else
      ensure_token
      # shellcheck disable=SC1090
      source "$ENV_FILE"
      FC_AGENT_TOKEN="$FC_AGENT_TOKEN" FC_AGENT_PORT="$PORT" FC_DIR="$FC_DIR" \
        nohup python3 "$FC_DIR/fc-agent.py" >"$LOG" 2>&1 &
      echo $! > "$PID_FILE"
      sleep 0.5
    fi
    sudo -n iptables -C INPUT -p tcp --dport "$PORT" -j ACCEPT 2>/dev/null \
      || sudo -n iptables -I INPUT -p tcp --dport "$PORT" -j ACCEPT
    "$0" env
    ;;
  stop)
    [ -f "$PID_FILE" ] && kill "$(cat "$PID_FILE")" 2>/dev/null || true
    rm -f "$PID_FILE"
    pkill -f fc-agent.py 2>/dev/null || true
    sudo -n iptables -D INPUT -p tcp --dport "$PORT" -j ACCEPT 2>/dev/null || true
    echo "[fc-agent] stopped"
    ;;
  restart)
    "$0" stop; "$0" start ;;
  log)
    tail -n 200 -f "$LOG" ;;
  install-service)
    ensure_token
    USER_NAME=$(id -un)
    UNIT=/etc/systemd/system/fc-agent.service
    sudo -n tee "$UNIT" >/dev/null <<EOF
[Unit]
Description=Fuse Firecracker host agent
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=root
WorkingDirectory=$FC_DIR
EnvironmentFile=$ENV_FILE
Environment=FC_DIR=$FC_DIR
Environment=FC_AGENT_PORT=$PORT
ExecStart=/usr/bin/python3 $FC_DIR/fc-agent.py
Restart=on-failure
RestartSec=3
StandardOutput=append:$LOG
StandardError=append:$LOG

[Install]
WantedBy=multi-user.target
EOF
    sudo -n systemctl daemon-reload
    sudo -n systemctl enable --now fc-agent.service
    sudo -n systemctl status --no-pager fc-agent.service | head -15
    "$0" env
    ;;
  uninstall-service)
    sudo -n systemctl disable --now fc-agent.service 2>/dev/null || true
    sudo -n rm -f /etc/systemd/system/fc-agent.service
    sudo -n systemctl daemon-reload
    echo "[fc-agent] service removed"
    ;;
  install-updater)
    # Weekly self-host auto-update via systemd timer: fc-update.sh pulls the
    # latest GitHub release of folsomintel/fuse, refreshes fused, re-bakes, and
    # restarts the agent. Public repo — no token required. An optional
    # .fc-updater.env (e.g. GH_TOKEN=... to dodge API rate limits, or
    # FUSE_ORCH_SERVICE/FUSE_ORCH_BIN to also update a co-located orchestrator)
    # is sourced if present.
    UPDATER_ENV="$FC_DIR/.fc-updater.env"
    ENVLINE=""
    [ -f "$UPDATER_ENV" ] && ENVLINE="EnvironmentFile=$UPDATER_ENV"
    sudo -n tee /etc/systemd/system/fc-update.service >/dev/null <<EOF
[Unit]
Description=Fuse self-host updater (pull latest release, rebake, restart)
After=network-online.target
Wants=network-online.target

[Service]
Type=oneshot
User=root
WorkingDirectory=$FC_DIR
$ENVLINE
ExecStart=$FC_DIR/fc-update.sh
EOF
    sudo -n tee /etc/systemd/system/fc-update.timer >/dev/null <<EOF
[Unit]
Description=Weekly Fuse update check

[Timer]
OnCalendar=Mon *-*-* 04:00:00
Persistent=true
RandomizedDelaySec=30min

[Install]
WantedBy=timers.target
EOF
    sudo -n systemctl daemon-reload
    sudo -n systemctl enable --now fc-update.timer
    sudo -n systemctl list-timers fc-update.timer --no-pager | head -5
    echo "[fc-agent] weekly updater installed (Mon 04:00 UTC, ±30min jitter)"
    ;;
  uninstall-updater)
    sudo -n systemctl disable --now fc-update.timer 2>/dev/null || true
    sudo -n rm -f /etc/systemd/system/fc-update.{service,timer}
    sudo -n systemctl daemon-reload
    echo "[fc-agent] updater removed"
    ;;
  env)
    ensure_token
    # shellcheck disable=SC1090
    source "$ENV_FILE"
    IP=$(public_ip)
    echo
    echo "# ---- Fuse orchestrator env ----"
    echo "FIRECRACKER_BASE_URL=http://${IP}:${PORT}"
    echo "FIRECRACKER_TOKEN=${FC_AGENT_TOKEN}"
    echo "# --------------------------------"
    ;;
  install-orchestrator)
    # Co-locate the Fuse orchestrator (control plane) next to this agent.
    # Topology is just config: FIRECRACKER_BASE_URL defaults to loopback here,
    # but the same binary runs anywhere you point it. Production defaults are
    # auth-on + Postgres; you fill DATABASE_URL before the first start.
    ensure_token
    # shellcheck disable=SC1090
    source "$ENV_FILE"   # FC_AGENT_TOKEN
    case "$(uname -m)" in
      x86_64)        ASSET_ARCH="x86_64" ;;
      aarch64|arm64) ASSET_ARCH="arm64" ;;
      *) echo "[orch] unsupported arch $(uname -m)" >&2; exit 1 ;;
    esac

    # 1. resolve the orchestrator binary: ORCH_BIN_SRC, then ./orchestrator next
    #    to this script, then an existing install, else the latest release.
    if [ -n "${ORCH_BIN_SRC:-}" ] && [ -x "$ORCH_BIN_SRC" ]; then
      sudo -n install -m0755 "$ORCH_BIN_SRC" "$ORCH_BIN"
    elif [ -x "$FC_DIR/orchestrator" ]; then
      sudo -n install -m0755 "$FC_DIR/orchestrator" "$ORCH_BIN"
    elif [ -x "$ORCH_BIN" ]; then
      echo "[orch] reusing existing $ORCH_BIN"
    else
      REPO="${FUSE_REPO:-folsomintel/fuse}"
      TAG=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
        | grep -oE '"tag_name"[[:space:]]*:[[:space:]]*"[^"]+"' | head -n1 \
        | sed -E 's/.*"([^"]+)"$/\1/')
      [ -n "$TAG" ] || { echo "[orch] no release found; set ORCH_BIN_SRC=/path/to/orchestrator" >&2; exit 1; }
      TARBALL="fuse_Linux_${ASSET_ARCH}.tar.gz"
      TGZ=$(mktemp); EXDIR=$(mktemp -d); SUMS=$(mktemp)
      echo "[orch] downloading $TARBALL ($TAG)"
      curl -fsSL -o "$TGZ" "https://github.com/$REPO/releases/download/$TAG/$TARBALL" \
        || { echo "[orch] download failed" >&2; exit 1; }
      # Verify before extracting or installing; see scripts/lib/release-verify.sh.
      curl -fsSL -o "$SUMS" "https://github.com/$REPO/releases/download/$TAG/checksums.txt" \
        || { rm -rf "$TGZ" "$EXDIR" "$SUMS"; echo "[orch] release $TAG publishes no checksums.txt - refusing unverified assets" >&2; exit 1; }
      verify_asset "$TGZ" "$TARBALL" "$SUMS" \
        || { rm -rf "$TGZ" "$EXDIR" "$SUMS"; echo "[orch] checksum verification failed for $TARBALL ($TAG)" >&2; exit 1; }
      rm -f "$SUMS"
      echo "[orch] verified $TARBALL against checksums.txt"
      tar -xzf "$TGZ" -C "$EXDIR" orchestrator
      sudo -n install -m0755 "$EXDIR/orchestrator" "$ORCH_BIN"
      rm -rf "$TGZ" "$EXDIR"
    fi
    echo "[orch] binary -> $ORCH_BIN ($("$ORCH_BIN" --version 2>/dev/null || echo '?'))"

    # 2. write /etc/default/orchestrator only if absent (never clobber edits).
    #    FIRECRACKER_TOKEN matches this host's agent; auth + encryption keys are
    #    generated; DATABASE_URL is left as a placeholder for you to fill.
    if [ ! -f "$ORCH_DEFAULTS" ]; then
      sudo -n tee "$ORCH_DEFAULTS" >/dev/null <<EOF
# /etc/default/orchestrator - Fuse orchestrator config. Edit, then:
#   sudo systemctl restart orchestrator
# Topology is just the agent URL: loopback = co-located, remote = split.
FIRECRACKER_BASE_URL=http://127.0.0.1:${PORT}
FIRECRACKER_TOKEN=${FC_AGENT_TOKEN}
ORCH_LISTEN=:8080

# State: Postgres (survives restarts). Auto-filled by \`fc-agent.sh bootstrap\`;
# otherwise fill this in before the first start.
DATABASE_URL=${DATABASE_URL:-postgres://USER:PASSWORD@HOST:5432/fuse_orchestrator?sslmode=require}

# Auth: required, fail-closed. Set the UI's FUSE_TOKEN to the SAME value.
ORCH_REQUIRE_AUTH=true
ORCH_AUTH_TOKEN=$(openssl rand -hex 32)

# Encrypt per-host agent tokens at rest in Postgres.
TOKEN_ENCRYPTION_KEY=$(openssl rand -hex 32)

# TLS: terminate at a reverse proxy (recommended). To terminate here instead:
# ORCH_TLS_CERT=/etc/fuse/tls/orchestrator.crt
# ORCH_TLS_KEY=/etc/fuse/tls/orchestrator.key

# Optional: restrict the source networks that may reach the API.
# ORCH_ALLOWED_CIDRS=10.0.0.0/8,100.64.0.0/10
EOF
      sudo -n chmod 600 "$ORCH_DEFAULTS"
      echo "[orch] wrote $ORCH_DEFAULTS (token + keys filled; DATABASE_URL is a placeholder)"
    else
      echo "[orch] $ORCH_DEFAULTS exists - leaving it untouched"
    fi

    # 3. install the systemd unit.
    sudo -n tee "$ORCH_UNIT" >/dev/null <<EOF
[Unit]
Description=Fuse orchestrator (control plane)
After=network-online.target fc-agent.service
Wants=network-online.target

[Service]
Type=simple
User=root
EnvironmentFile=$ORCH_DEFAULTS
ExecStart=$ORCH_BIN
Restart=on-failure
RestartSec=3

[Install]
WantedBy=multi-user.target
EOF
    sudo -n systemctl daemon-reload
    sudo -n systemctl enable orchestrator.service

    # 4. only start once DATABASE_URL is real - auth-on + a placeholder DB would
    #    just crash-loop.
    if grep -q 'USER:PASSWORD@HOST' "$ORCH_DEFAULTS"; then
      echo "[orch] enabled but NOT started - edit $ORCH_DEFAULTS (set DATABASE_URL), then:"
      echo "       sudo systemctl start orchestrator"
    else
      sudo -n systemctl restart orchestrator.service
      sudo -n systemctl status --no-pager orchestrator.service | head -15
    fi

    # 5. print the UI wiring + the updater hint.
    TOK=$(grep -E '^ORCH_AUTH_TOKEN=' "$ORCH_DEFAULTS" | cut -d= -f2- || true)
    echo
    echo "# ---- point the Fuse UI (fuse-frontend) at this orchestrator ----"
    echo "FUSE_BASE_URL=http://$(public_ip):8080   # or your TLS proxy URL"
    echo "FUSE_TOKEN=${TOK}"
    echo "# ----------------------------------------------------------------"
    echo "[orch] tip: add FUSE_ORCH_SERVICE=orchestrator.service and FUSE_ORCH_BIN=$ORCH_BIN"
    echo "       to $FC_DIR/.fc-updater.env so the weekly updater keeps it current"
    ;;
  uninstall-orchestrator)
    sudo -n systemctl disable --now orchestrator.service 2>/dev/null || true
    sudo -n rm -f "$ORCH_UNIT"
    sudo -n systemctl daemon-reload
    echo "[orch] orchestrator service removed (left $ORCH_DEFAULTS and $ORCH_BIN in place)"
    ;;
  bootstrap)
    # One-command host bring-up. Composes the pieces below into a working host +
    # control plane, then registers itself and prints the connect line. Idempotent
    # and re-runnable. Run with sudo: sudo ./fc-agent.sh bootstrap
    #   flags: --no-updater (skip the weekly auto-update timer)
    #          --no-register (don't self-register with the orchestrator)
    shift || true
    NO_UPDATER=0; NO_REGISTER=0
    for a in "$@"; do
      case "$a" in
        --no-updater)  NO_UPDATER=1 ;;
        --no-register) NO_REGISTER=1 ;;
        *) echo "[bootstrap] unknown flag: $a (want --no-updater / --no-register)" >&2; exit 1 ;;
      esac
    done

    echo "[bootstrap] 1/8 host dependencies"
    "$FC_DIR/fc-deps.sh"
    echo "[bootstrap] 2/8 firecracker + kernel + base rootfs"
    "$FC_DIR/fc-install.sh"
    echo "[bootstrap] 3/8 host agent service"
    "$0" install-service
    echo "[bootstrap] 4/8 postgres"
    DB_URL="$("$FC_DIR/fc-postgres.sh")"
    echo "[bootstrap] 5/8 orchestrator service"
    # if an earlier run left the placeholder DATABASE_URL, fill it in now.
    if [ -f "$ORCH_DEFAULTS" ] && sudo -n grep -q 'USER:PASSWORD@HOST' "$ORCH_DEFAULTS"; then
      sudo -n sed -i "s#^DATABASE_URL=.*#DATABASE_URL=${DB_URL}#" "$ORCH_DEFAULTS"
      sudo -n systemctl restart orchestrator.service 2>/dev/null || true
    fi
    DATABASE_URL="$DB_URL" "$0" install-orchestrator
    if [ "$NO_UPDATER" != "1" ]; then
      echo "[bootstrap] 6/8 auto-update timer"
      UPDATER_ENV="$FC_DIR/.fc-updater.env"
      touch "$UPDATER_ENV"
      grep -q '^FUSE_ORCH_SERVICE=' "$UPDATER_ENV" || echo "FUSE_ORCH_SERVICE=orchestrator.service" >> "$UPDATER_ENV"
      grep -q '^FUSE_ORCH_BIN=' "$UPDATER_ENV"     || echo "FUSE_ORCH_BIN=$ORCH_BIN" >> "$UPDATER_ENV"
      "$0" install-updater
    else
      echo "[bootstrap] 6/8 skipping auto-update timer (--no-updater)"
    fi
    echo "[bootstrap] 7/8 guest agent (fused) + rootfs bake"
    # fc-update.sh (run bare, no FUSE_ORCH_* here) fetches the latest fused release
    # asset and bakes it into the rootfs. Non-fatal: the control plane is already up.
    "$FC_DIR/fc-update.sh" || echo "[bootstrap]  ! fused/bake step failed - control plane is up, but new VMs lack the guest toolchain until it's fixed" >&2

    # read back the generated orchestrator token + this host's agent token.
    TOK=$(sudo -n grep -E '^ORCH_AUTH_TOKEN=' "$ORCH_DEFAULTS" | cut -d= -f2-)
    # shellcheck disable=SC1090
    [ -f "$ENV_FILE" ] && source "$ENV_FILE"   # FC_AGENT_TOKEN

    if [ "$NO_REGISTER" != "1" ] && [ -n "$TOK" ]; then
      echo "[bootstrap] 8/8 register this host with the orchestrator"
      # wait for the api (migrations run on first boot).
      for _ in $(seq 1 30); do
        curl -fsS -H "Authorization: Bearer $TOK" http://127.0.0.1:8080/v1/hosts >/dev/null 2>&1 && break
        sleep 1
      done
      CPUS=$(nproc 2>/dev/null || echo 4)
      RAM_MB=$(( $(getconf _PHYS_PAGES 2>/dev/null || echo 2097152) * $(getconf PAGE_SIZE 2>/dev/null || echo 4096) / 1048576 ))
      STORAGE_GB=$(df -Pk / 2>/dev/null | awk 'NR==2{printf "%d", $4/1048576}'); [ -n "$STORAGE_GB" ] || STORAGE_GB=100
      if curl -fsS -X POST http://127.0.0.1:8080/v1/hosts \
           -H "Authorization: Bearer $TOK" -H 'Content-Type: application/json' \
           -d "{\"id\":\"local-fc\",\"url\":\"http://127.0.0.1:${PORT}\",\"token\":\"${FC_AGENT_TOKEN:-}\",\"region\":\"local\",\"backend\":\"firecracker\",\"capacity\":{\"cpus\":${CPUS},\"ram_mb\":${RAM_MB},\"storage_gb\":${STORAGE_GB},\"vm_count\":$(( CPUS * 4 ))}}" \
           >/dev/null 2>&1; then
        echo "[bootstrap]  ✓ registered host local-fc (${CPUS} cpu / ${RAM_MB} MB / ${STORAGE_GB} GB)"
      else
        echo "[bootstrap]  ! host register skipped (already registered, or orchestrator not ready)"
      fi
    else
      echo "[bootstrap] 8/8 skipping self-registration"
    fi

    IP=$(public_ip)
    echo
    echo "======================================================================"
    echo " fuse host ready"
    echo "   orchestrator : http://${IP}:8080"
    if [ -n "$TOK" ]; then
      echo "   token        : ${TOK}"
      echo
      echo "   drive it from your laptop:"
      echo "     fuse connect http://${IP}:8080 --token ${TOK}"
    else
      echo "   token        : (not started - see $ORCH_DEFAULTS, then: sudo systemctl start orchestrator)"
    fi
    echo "======================================================================"
    ;;
  *)
    echo "usage: $0 {start|stop|restart|log|env|bootstrap|install-service|uninstall-service|install-updater|uninstall-updater|install-orchestrator|uninstall-orchestrator}" >&2; exit 1 ;;
esac
