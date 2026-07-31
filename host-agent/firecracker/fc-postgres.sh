#!/usr/bin/env bash
# Provision a local Postgres for the Fuse orchestrator. Idempotent.
#
# Installs the distro postgres, creates a role + database (loopback only), and
# prints a ready-to-use DATABASE_URL on stdout. All logs go to stderr, so the
# caller can capture just the connection string:
#
#   DATABASE_URL="$(sudo ./fc-postgres.sh)"
#
# Env knobs:
#   PG_DB    database name (default fuse_orchestrator)
#   PG_USER  role name     (default fuse)
#   PG_PASS  password       (default: generated once, persisted to /etc/fuse/postgres.pass)
set -euo pipefail

DB="${PG_DB:-fuse_orchestrator}"
ROLE="${PG_USER:-fuse}"
PASS_FILE=/etc/fuse/postgres.pass

log() { printf '\033[1;36m[pg] %s\033[0m\n' "$*" >&2; }
ok()  { printf '\033[1;32m  ✓ %s\033[0m\n' "$*" >&2; }
die() { printf '\033[1;31m  ✗ %s\033[0m\n' "$*" >&2; exit 1; }

# sudo wrapper (no-op if already root).
SUDO=""
if [ "$(id -u)" -ne 0 ]; then
  command -v sudo >/dev/null 2>&1 || die "need root or sudo"
  SUDO="sudo"
fi

# --- 1. install the postgres server (idempotent) ------------------------------
if ! command -v psql >/dev/null 2>&1; then
  if command -v apt-get >/dev/null 2>&1; then
    log "apt: installing postgresql"
    export DEBIAN_FRONTEND=noninteractive
    $SUDO apt-get update -y >&2
    $SUDO apt-get install -y --no-install-recommends postgresql >&2
  elif command -v dnf >/dev/null 2>&1; then
    log "dnf: installing postgresql-server"
    $SUDO dnf install -y postgresql-server postgresql >&2
    $SUDO postgresql-setup --initdb >&2 2>/dev/null || true
  else
    die "no apt-get or dnf; install postgresql manually, then re-run"
  fi
fi

# --- 2. make sure the server is up --------------------------------------------
$SUDO systemctl enable --now postgresql >&2 2>/dev/null \
  || $SUDO service postgresql start >&2 2>/dev/null \
  || die "could not start postgresql"

# run SQL as the postgres superuser (peer auth via the postgres OS user)
psql_su() { $SUDO -u postgres psql -tAc "$1"; }

# --- 3. password: reuse a persisted one, else generate + persist --------------
if [ -f "$PASS_FILE" ]; then
  PASS="$($SUDO cat "$PASS_FILE")"
  log "reusing password from $PASS_FILE"
else
  PASS="${PG_PASS:-$(openssl rand -hex 24)}"
  $SUDO mkdir -p "$(dirname "$PASS_FILE")"
  printf '%s\n' "$PASS" | $SUDO tee "$PASS_FILE" >/dev/null
  $SUDO chmod 600 "$PASS_FILE"
  ok "generated password -> $PASS_FILE"
fi

# --- 4. role (create or sync password) ----------------------------------------
if [ "$(psql_su "SELECT 1 FROM pg_roles WHERE rolname='$ROLE'")" = "1" ]; then
  psql_su "ALTER ROLE \"$ROLE\" WITH LOGIN PASSWORD '$PASS'" >/dev/null
  ok "role $ROLE exists (password synced)"
else
  psql_su "CREATE ROLE \"$ROLE\" WITH LOGIN PASSWORD '$PASS'" >/dev/null
  ok "role $ROLE created"
fi

# --- 5. database owned by the role --------------------------------------------
if [ "$(psql_su "SELECT 1 FROM pg_database WHERE datname='$DB'")" = "1" ]; then
  ok "database $DB exists"
else
  psql_su "CREATE DATABASE \"$DB\" OWNER \"$ROLE\"" >/dev/null
  ok "database $DB created"
fi

# --- 6. emit the connection string (the only thing on stdout) -----------------
# hex password has no url-special characters, so no encoding needed.
printf 'postgres://%s:%s@127.0.0.1:5432/%s?sslmode=disable\n' "$ROLE" "$PASS" "$DB"
