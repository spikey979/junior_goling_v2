#!/usr/bin/env bash
set -euo pipefail

# Simple dev entrypoint that watches for changes in templates and .env,
# (re)loads environment from /app/.env, and restarts the binary.

APP_BIN="/app/bin/fileapi"
ENV_FILE="/app/.env"
WATCH_DIR="/app/web"

install_inotify() {
  if ! command -v inotifywait >/dev/null 2>&1; then
    apt-get update >/dev/null 2>&1 || true
    apt-get install -y --no-install-recommends inotify-tools >/dev/null 2>&1 || true
    rm -rf /var/lib/apt/lists/* || true
  fi
}

export_env() {
  if [ -f "$ENV_FILE" ]; then
    set -a
    # shellcheck disable=SC1090
    source "$ENV_FILE"
    set +a
  fi
}

start_app() {
  # Fail fast if binary missing
  if [ ! -x "$APP_BIN" ]; then
    echo "[dev][fatal] binary not found at $APP_BIN. Build it with: ./scripts/rebuild_in_container.sh" >&2
    exit 1
  fi
  export_env
  echo "[dev] starting app..."
  "$APP_BIN" &
  APP_PID=$!
}

stop_app() {
  if [ -n "${APP_PID:-}" ] && kill -0 "$APP_PID" 2>/dev/null; then
    echo "[dev] stopping app pid=$APP_PID..."
    kill "$APP_PID" || true
    wait "$APP_PID" || true
  fi
}

main() {
  install_inotify
  start_app
  while true; do
    inotifywait -e modify,create,delete,move -r "$WATCH_DIR" "$ENV_FILE" >/dev/null 2>&1 || true
    echo "[dev] change detected in templates or .env; restarting..."
    stop_app
    start_app
  done
}

main "$@"
