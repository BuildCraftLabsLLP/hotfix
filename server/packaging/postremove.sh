#!/bin/sh
# Package post-remove. "purge" (deb) also deletes state and logs; the config
# in /etc/hotfix-server is a conffile and is purged by dpkg itself.
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
  systemctl daemon-reload >/dev/null 2>&1 || true
fi
if [ "$1" = purge ]; then
  rm -rf /var/lib/hotfix-server /var/log/hotfix-server
fi
exit 0
