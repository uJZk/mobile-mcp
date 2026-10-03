# Magisk / KernelSU / APatch install script

SKIPUNZIP=0
DATA_DIR=/data/adb/mobile-mcp

case "$ARCH" in
  arm64|arm|x64|x86) ;;
  *) abort "! Unsupported architecture: $ARCH" ;;
esac

ui_print "- Installing agent for $ARCH"
mv "$MODPATH/bin/mobile-mcp-agent-$ARCH" "$MODPATH/bin/mobile-mcp-agent" || abort "! Missing agent binary for $ARCH"
rm -f "$MODPATH"/bin/mobile-mcp-agent-*

mkdir -p "$DATA_DIR"
chmod 700 "$DATA_DIR"
if [ ! -f "$DATA_DIR/config" ]; then
  cp "$MODPATH/config.default" "$DATA_DIR/config"
fi
chmod 600 "$DATA_DIR/config"

set_perm_recursive "$MODPATH" 0 0 0755 0644
set_perm "$MODPATH/bin/mobile-mcp-agent" 0 0 0755

TOKEN=$("$MODPATH/bin/mobile-mcp-agent" -token-file "$DATA_DIR/token" -print-token) || abort "! Failed to create token"

ui_print " "
ui_print "- Config: $DATA_DIR/config"
ui_print "- Token:  $TOKEN"
ui_print "  (also stored in $DATA_DIR/token)"
ui_print " "
ui_print "- On your computer, set:"
ui_print "  MOBILEMCP_ANDROID_ROOT_DEVICES=<phone-ip>:8765"
ui_print "  MOBILEMCP_ANDROID_ROOT_TOKEN=$TOKEN"
ui_print "- The agent starts after reboot."
