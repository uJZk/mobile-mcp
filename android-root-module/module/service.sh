#!/system/bin/sh
# Starts the mobile-mcp agent once boot completes and restarts it if it exits.

MODDIR=${0%/*}
DATA_DIR=/data/adb/mobile-mcp
LOG=$DATA_DIR/agent.log

until [ "$(getprop sys.boot_completed)" = "1" ]; do
  sleep 2
done

LISTEN=0.0.0.0:8765
ALLOW_LOCAL=0
[ -f "$DATA_DIR/config" ] && . "$DATA_DIR/config"

export PATH=/system/bin:/system/xbin:/vendor/bin:$PATH

while [ ! -f "$MODDIR/disable" ] && [ ! -f "$MODDIR/remove" ]; do
  # keep the log from growing without bound
  [ -f "$LOG" ] && [ "$(stat -c %s "$LOG")" -gt 1048576 ] && mv -f "$LOG" "$LOG.1"
  "$MODDIR/bin/mobile-mcp-agent" -listen "$LISTEN" -allow-local="$([ "$ALLOW_LOCAL" = "1" ] && echo true || echo false)" -token-file "$DATA_DIR/token" >> "$LOG" 2>&1
  sleep 5
done
