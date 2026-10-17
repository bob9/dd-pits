#!/bin/sh
# dd-pits.app's entry point. It starts dd-pits in the background and returns,
# so double-clicking the app again works: if dd-pits is already running, it
# just reopens the Status page.
DIR=$(cd "$(dirname "$0")" && pwd)
PORT=${DD_PITS_PORT:-8088}
if curl -fs -m 2 -o /dev/null "http://127.0.0.1:$PORT/status"; then
  [ -n "$DD_PITS_NO_BROWSER" ] || open "http://localhost:$PORT/status"
  exit 0
fi
# Its log goes to ~/Library/Application Support/dd-pits/dd-pits.log.
nohup "$DIR/dd-pits" >/dev/null 2>&1 &
exit 0
