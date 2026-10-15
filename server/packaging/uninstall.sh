#!/bin/sh
# Removes Hotfix Server installed with install.sh: frees the license seat,
# removes the service and the binary. Settings and logs are kept unless
# --purge is given.
set -e
if [ "$(id -u)" != 0 ]; then
  echo "Run with sudo: sudo ./uninstall.sh [--purge]" >&2
  exit 1
fi
BIN=/usr/local/bin/hotfixd
if [ -x "$BIN" ]; then
  "$BIN" license deactivate >/dev/null 2>&1 || true
  "$BIN" uninstall || true
  rm -f "$BIN"
fi
if [ "$1" = --purge ]; then
  case "$(uname)" in
    Darwin) rm -rf "/Library/Application Support/Hotfix Server" "/Library/Logs/Hotfix Server" ;;
    *) rm -rf /etc/hotfix-server /var/lib/hotfix-server /var/log/hotfix-server ;;
  esac
  echo "Hotfix Server removed, with its settings and logs."
else
  echo "Hotfix Server removed. Settings and logs were kept (run with --purge to delete them)."
fi
