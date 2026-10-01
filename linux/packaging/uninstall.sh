#!/bin/sh
# Removes a per-user Hotfix install made by install.sh.
#
#   ./uninstall.sh          remove the app (keeps your settings and log)
#   ./uninstall.sh --purge  …and delete settings and logs too
set -eu

purge=0
for arg in "$@"; do
    case "$arg" in
        --purge) purge=1 ;;
        -h|--help)
            sed -n '2,5p' "$0" | sed 's/^# \{0,1\}//'
            exit 0 ;;
        *)
            echo "uninstall.sh: unknown option: $arg" >&2
            exit 2 ;;
    esac
done

data_dir="${XDG_DATA_HOME:-$HOME/.local/share}"
config_dir="${XDG_CONFIG_HOME:-$HOME/.config}"
state_dir="${XDG_STATE_HOME:-$HOME/.local/state}"
exe="$HOME/.local/bin/hotfix"

# Stop a running instance (matched by executable path, never by name alone).
if command -v pkill >/dev/null 2>&1; then
    pkill -u "$(id -u)" -f "^$exe\$" 2>/dev/null || true
fi

rm -f "$exe" \
      "$data_dir/applications/hotfix.desktop" \
      "$data_dir/icons/hicolor/scalable/apps/hotfix.svg" \
      "$config_dir/autostart/hotfix.desktop"
update-desktop-database -q "$data_dir/applications" 2>/dev/null || true

if [ "$purge" -eq 1 ]; then
    rm -rf "$config_dir/hotfix" "$state_dir/hotfix"
    echo "Hotfix removed, including settings and logs."
else
    echo "Hotfix removed. Settings kept in $config_dir/hotfix (use --purge to delete)."
fi
