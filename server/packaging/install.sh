#!/bin/sh
# Installs (or upgrades) Hotfix Server from this archive (Linux or macOS):
# copies hotfixd to /usr/local/bin and installs and starts the service.
#
#   sudo ./install.sh              # 14-day free trial
#   sudo ./install.sh HFXS-…       # and activate your license key
set -e
if [ "$(id -u)" != 0 ]; then
  echo "Run with sudo: sudo ./install.sh" >&2
  exit 1
fi
cd "$(dirname "$0")"
BIN=/usr/local/bin/hotfixd

upgrade=
if [ -x "$BIN" ] && "$BIN" status 2>/dev/null | head -1 | grep -Eq "running|stopped"; then
  upgrade=1
  "$BIN" stop >/dev/null 2>&1 || true
fi

mkdir -p /usr/local/bin
cp hotfixd "$BIN.new"
chmod 0755 "$BIN.new"
mv -f "$BIN.new" "$BIN"
case "$(uname)" in
  Darwin) xattr -d com.apple.quarantine "$BIN" 2>/dev/null || true ;;
  Linux) command -v restorecon >/dev/null 2>&1 && restorecon "$BIN" 2>/dev/null || true ;;
esac

if [ -n "$upgrade" ]; then
  "$BIN" start
  echo "Hotfix Server upgraded to $("$BIN" version)."
else
  "$BIN" install
fi
if [ -n "$1" ]; then
  "$BIN" license activate "$1"
fi
