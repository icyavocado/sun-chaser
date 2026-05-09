#!/usr/bin/env bash
# dev-server.sh — build and (re)start the sun-chaser dev server.
# Compiles to .sun-chaser-bin so the PID we store is the real process,
# not a go-run wrapper. Kills both the tracked PID and any orphan holding
# the port before starting fresh.

set -euo pipefail

PID_FILE=".server.pid"
BIN=".sun-chaser-bin"
PORT="${PORT:-8080}"

# 1. Kill the previously tracked process.
if [ -f "$PID_FILE" ]; then
  OLD_PID=$(cat "$PID_FILE")
  if kill -0 "$OLD_PID" 2>/dev/null; then
    echo "Stopping server (PID $OLD_PID)..."
    kill "$OLD_PID"
    for _ in 1 2 3; do
      kill -0 "$OLD_PID" 2>/dev/null || break
      sleep 1
    done
  fi
  rm -f "$PID_FILE"
fi

# 2. Kill any orphan still holding the port.
ORPHAN=$(ss -tlnp "sport = :$PORT" 2>/dev/null \
  | awk 'match($0,/pid=([0-9]+)/,a){print a[1]}' | head -1)
if [ -n "$ORPHAN" ]; then
  echo "Killing orphan on :$PORT (PID $ORPHAN)..."
  kill "$ORPHAN" 2>/dev/null || true
  sleep 1
fi

# 3. Build.
echo "Building..."
mise exec -- go build -o "$BIN" .

# 4. Run the binary directly so $! is the server's own PID.
echo "Starting sun-chaser on :$PORT ..."
PORT="$PORT" "./$BIN" &
echo $! > "$PID_FILE"
echo "Server PID $(cat "$PID_FILE") written to $PID_FILE"
