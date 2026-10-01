#!/bin/sh
# Hotfix per-user installer for Linux. No root needed: everything goes under
# your home directory, which is also what lets Hotfix update itself silently.
#
#   ./install.sh              install the binary, app-menu entry and icon
#   ./install.sh --autostart  …and start Hotfix automatically at login
#
# The --autostart entry is the same file the tray's "Start at Login" toggle
# manages, so either can turn it on or off later.
set -eu

autostart=0
for arg in "$@"; do
    case "$arg" in
        --autostart) autostart=1 ;;
        -h|--help)
            sed -n '2,9p' "$0" | sed 's/^# \{0,1\}//'
            exit 0 ;;
        *)
            echo "install.sh: unknown option: $arg" >&2
            exit 2 ;;
    esac
done

here=$(cd "$(dirname "$0")" && pwd)
bin_dir="$HOME/.local/bin"
data_dir="${XDG_DATA_HOME:-$HOME/.local/share}"
config_dir="${XDG_CONFIG_HOME:-$HOME/.config}"
exe="$bin_dir/hotfix"

if [ ! -f "$here/hotfix" ]; then
    echo "install.sh: 'hotfix' binary not found next to this script" >&2
    exit 1
fi

# Writes a .desktop entry for the installed binary to the path in $1.
write_desktop_entry() {
    mkdir -p "$(dirname "$1")"
    cat > "$1" <<EOF
[Desktop Entry]
Type=Application
Name=Hotfix
Comment=Kills runaway processes before your fans spin up
Exec="$exe"
Icon=hotfix
Terminal=false
Categories=System;Monitor;
X-GNOME-Autostart-enabled=true
EOF
}

mkdir -p "$bin_dir"
# Copy to a temp name and rename, so a running Hotfix is replaced safely.
cp "$here/hotfix" "$exe.new"
chmod 755 "$exe.new"
mv -f "$exe.new" "$exe"

if [ -f "$here/hotfix.svg" ]; then
    icon_dir="$data_dir/icons/hicolor/scalable/apps"
    mkdir -p "$icon_dir"
    cp "$here/hotfix.svg" "$icon_dir/hotfix.svg"
    gtk-update-icon-cache -q -t "$data_dir/icons/hicolor" 2>/dev/null || true
fi

write_desktop_entry "$data_dir/applications/hotfix.desktop"
update-desktop-database -q "$data_dir/applications" 2>/dev/null || true

if [ "$autostart" -eq 1 ]; then
    write_desktop_entry "$config_dir/autostart/hotfix.desktop"
fi

echo "Hotfix installed to $exe"
if [ "$autostart" -eq 1 ]; then
    echo "It will start automatically at login."
fi
echo
echo "Start it now from your app menu, or run:  $exe &"
echo
echo "Note: Hotfix lives in the system tray. On GNOME, tray icons need the"
echo "\"AppIndicator and KStatusNotifierItem Support\" extension (preinstalled on Ubuntu)."
