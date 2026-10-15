#!/bin/sh
# Package pre-remove. Only on removal (deb: "remove"; rpm: 0), not upgrade:
# free this server's license seat and stop the service.
if [ "$1" = remove ] || [ "$1" = 0 ]; then
  /usr/bin/hotfixd license deactivate >/dev/null 2>&1 || true
  if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    systemctl disable --now hotfixd >/dev/null 2>&1 || true
  fi
fi
exit 0
