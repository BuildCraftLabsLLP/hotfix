#!/bin/sh
# Package post-install (deb: "configure" [old-version]; rpm: 1 = install, 2 = upgrade).
set -e
mkdir -p /var/lib/hotfix-server /var/log/hotfix-server

if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
  systemctl daemon-reload || true
  if { [ "$1" = configure ] && [ -z "$2" ]; } || [ "$1" = 1 ]; then
    systemctl enable --now hotfixd || true
    cat <<'EOF'

Hotfix Server is running in MONITOR mode: it only reports what it would kill.
  See what it's doing:       hotfixd status
  Start killing:             sudo hotfixd config set mode enforce
  Add your license key:      sudo hotfixd license activate HFXS-...
  (14-day free trial without a key)
EOF
  else
    systemctl try-restart hotfixd || true
  fi
fi
exit 0
