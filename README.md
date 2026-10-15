# Hotfix

**Keep your machine cool.** Hotfix monitors CPU usage and automatically terminates runaway processes before your fan spins up and your battery drains — on macOS, Windows, and Linux.

Built by [BuildCraft Labs](https://github.com/BuildCraftLabsLLP).

---

## The Problem

Claude extensions, AI tools, and background daemons run rogue — consuming 50–100% CPU for hours, spinning up fans, draining batteries, and generating heat with no visible indication anything is wrong.

## How It Works

1. **Monitor** — Polls all running processes every 5 seconds
2. **Detect** — Any process sustaining CPU above your configured threshold gets flagged
3. **Kill** — After your configured duration, it terminates the offender and notifies you

## Download

| Platform | Download | Requirements |
|----------|----------|-------------|
| **macOS** | [Hotfix.dmg](https://github.com/BuildCraftLabsLLP/hotfix/releases/latest) | macOS 13+ · Apple Silicon or Intel |
| **Windows** | [Hotfix-Setup.exe](https://github.com/BuildCraftLabsLLP/hotfix/releases/latest) | Windows 11 · x64 |
| **Linux** | [Hotfix-…-Linux-x86_64.deb](https://github.com/BuildCraftLabsLLP/hotfix/releases/latest) (also `.rpm`, `.tar.gz`) | x86-64 or arm64 · a desktop with a system tray (StatusNotifierItem) |

> **Windows:** the installer is per-user (no admin) — it installs to `%LOCALAPPDATA%\Programs\Hotfix`, adds an uninstaller to **Apps & features**, and updates itself silently in the background. Delete the downloaded `Hotfix-Setup.exe` once it's installed.

> **Linux:** pick the package for your distro:
> - **Ubuntu, Debian, Mint** — download the `.deb` and double-click it (or `sudo apt install ./Hotfix-…-Linux-x86_64.deb`). Hotfix then appears in the app menu; remove it from the App Center or with `sudo apt remove hotfix`.
> - **Fedora, openSUSE** — the `.rpm`: `sudo dnf install ./Hotfix-…-Linux-x86_64.rpm`.
> - **Any distro, no root** — the `.tar.gz`: extract it and run `./install.sh` (add `--autostart` to start at login). It installs per-user to `~/.local/bin/hotfix` with an app-menu entry; `./uninstall.sh` removes it.
>
> The per-user `.tar.gz` install updates itself silently in the background. The `.deb`/`.rpm` install system-wide, which the app can't overwrite, so it notifies you when a new version is out and you install the new package. See [Linux notes](#linux-notes) for tray and Wayland caveats.

> **macOS:** Not yet notarized — right-click → **Open** on first launch to bypass Gatekeeper.

## Features

- **Native system tray** — Lives in your menu bar / taskbar, no Dock or taskbar icon
- **Configurable threshold** — Set the CPU % that triggers monitoring (default: 80%)
- **Configurable duration** — How long a process must be hot before being killed (default: 60s)
- **Kill on sleep** — Optionally terminate hot processes when the machine sleeps
- **Exclusion list** — Protect specific processes from ever being killed: type a name, or pick from a dropdown of the apps running right now (busiest first, with their CPU use) and every other app installed on the system
- **Desktop notifications** — A notification fires whenever a process is terminated
- **Activity logs** — Events are written to a log file and viewable in-app from Settings (macOS: `~/Library/Logs/Hotfix/hotfix.log`, Windows: `%APPDATA%\Hotfix\hotfix.log`, Linux: `~/.local/state/hotfix/hotfix.log`, opened from the tray's **View Log**)
- **Silent auto-updates** — New versions are downloaded and installed in the background, then the app relaunches — no prompts
- **Crash reporting** — On every platform, a crash opens a pre-filled GitHub issue on the next launch (you review before submitting)
- **Safety exclusions** — System-critical processes are permanently protected and can never be killed

## Configuration

Settings are accessible from the tray icon → **Settings**.

| Setting | Default | Description |
|---------|---------|-------------|
| CPU Threshold | 80% | Processes above this level are monitored |
| Kill After | 60s | Duration above threshold before termination |
| Kill on Sleep | On | Kill hot processes when machine sleeps |
| Exclusions (macOS) | Xcode, swift, clang, node, python3 | Processes never killed |
| Exclusions (Windows) | explorer, svchost, lsass, dwm… | Processes never killed |
| Exclusions (Linux) | _(empty)_ | Processes never killed — the `whitelist` array in `config.json` |

### Linux notes

**Hotfix for Linux is free for 7 days, then a $1/month subscription, per computer.** The trial starts on first launch, needs no card, and the tray shows how many days are left. **Subscribe ($1/month)…** in the tray opens a Razorpay checkout page in your browser; the app unlocks by itself a few seconds after payment. Subscribing during the trial doesn't cut it short — the first payment is taken when the free week ends. The subscription is tied to that one machine (no license key to enter or share) — **Manage Subscription…** shows its status and lets you cancel. Once the trial is over, monitoring is off until you subscribe. A subscribed machine keeps working offline for up to 72 hours between license checks.

The Linux app looks and works like the Windows one: the same tray menu, and **Settings…** opens the same settings popover — protection toggles, CPU threshold and kill-after sliders, exclusions, logs and About. It renders with **WebKitGTK 4.1**, which the `.deb`/`.rpm` install automatically; with the `.tar.gz`, install it yourself (`sudo apt install libwebkit2gtk-4.1-0`) — without it the tray still works and Settings opens `~/.config/hotfix/config.json` instead (changes there are picked up automatically when you save).

- **CPU % is per core**, as in `top`: a process using two full cores reads 200%.
- **Only your own processes** are monitored (Hotfix runs without root). The desktop session itself (systemd, the compositor/X server, PipeWire, D-Bus, …) is permanently protected.
- **Requirements:** a glibc-based distro from about 2022 on (Ubuntu 22.04+, Debian 12+, Fedora, Mint 21+); x86-64 or arm64.
- **Settings popover placement:** it opens in the screen corner next to the tray and closes when you click away (or press Esc). On Wayland the compositor decides where windows go, so there it opens centered.
- **Tray icon:** needs a StatusNotifierItem host. KDE, Cinnamon, XFCE, and most bars (waybar, etc.) have one; GNOME needs the *AppIndicator and KStatusNotifierItem Support* extension (preinstalled on Ubuntu). Without a tray, Hotfix still monitors and can be configured through `config.json`.
- **Protect Active App** needs a way to ask which window is focused: it works on X11 (with `xdotool` or `xprop` installed), sway, and Hyprland. GNOME and KDE **Wayland** sessions don't expose this, so there only XWayland apps are detected — add apps you never want killed to the whitelist instead.
- **Kill on Sleep** uses systemd-logind; it does nothing on non-systemd distros.

## Build from Source

### macOS
Requires Xcode Command Line Tools.

```bash
git clone https://github.com/BuildCraftLabsLLP/hotfix.git
cd hotfix
bash scripts/build.sh
open "dist/Hotfix.dmg"
```

### Windows
Requires Go 1.22+.

```powershell
git clone https://github.com/BuildCraftLabsLLP/hotfix.git
cd hotfix\windows
go generate ./...   # embeds the flame icon + version metadata (resource.syso)
go build -ldflags "-H windowsgui -s -w" -o ..\dist\Hotfix.exe .

# Optional: build the per-user installer (requires Inno Setup 6)
& "C:\Program Files (x86)\Inno Setup 6\ISCC.exe" /DMyAppVersion=1.0.7 installer\hotfix.iss
```

### Linux
Requires Go 1.22+. Pure Go — no CGO, GTK, or other system libraries needed, and it cross-compiles from any OS with `GOOS=linux`.

```bash
git clone https://github.com/BuildCraftLabsLLP/hotfix.git
cd hotfix/linux
go test ./...
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o ../dist/hotfix .
```

A plain local build like this does not enforce the subscription. Release builds do: CI stamps the license public key in with `-X main.licensePublicKey=…` (see [`worker/README.md`](worker/README.md#licensing-linux-subscription)).

## Releasing a New Version

Releasing is fully automated. All binaries are built and attached by CI on every new release.

1. Bump the version in `Sources/Hotfix/UpdateChecker.swift`, `Resources/Info.plist` (short string + build number), `windows/updater.go`, `linux/updater.go`, and the About label in `windows/assets/settings.html`. (The exe metadata in `windows/versioninfo.json` and the installer version are stamped automatically by CI — see step 3. The website's download buttons carry no version — they point at a redirect that always resolves the latest release — so there's nothing to bump there.)
2. Create a GitHub release tagged `v<version>`
3. The `Build` workflow runs on `macos-latest`, `windows-2025`, and `ubuntu-latest` and attaches version+OS-named assets: `Hotfix-v<version>-macOS.dmg`, `Hotfix-v<version>-Windows.exe` (raw exe for the auto-updater), `Hotfix-Setup-v<version>-Windows.exe` (the per-user installer the website links to), and for Linux, per CPU (`x86_64`, `arm64`), `Hotfix-v<version>-Linux-<arch>` (raw binary for the auto-updater) plus `Hotfix-v<version>-Linux-<arch>.tar.gz` (binary + `install.sh`). No site update is needed: the website's download buttons route through a Cloudflare Worker (`worker/`) that always resolves the latest release, and it also counts installs — see the Worker's own README

Users on all platforms will be notified of the update on next launch.

## Repository Structure

```
hotfix/
├── Sources/Hotfix/     # macOS Swift/SwiftUI app
├── Resources/          # macOS Info.plist
├── scripts/            # Build scripts (macOS DMG, icon generation)
├── windows/            # Windows Go app
│   ├── assets/         # Settings HTML, tray/exe icons, gen-icons.ps1
│   ├── installer/      # Inno Setup script (per-user installer)
│   └── *.go            # Go source files
├── linux/              # Linux Go app (pure Go, tray-menu settings)
│   ├── packaging/      # install.sh / uninstall.sh (per-user install)
│   └── *.go            # Go source files
├── icon/               # App icon assets
├── docs/               # Marketing website (GitHub Pages, hotfix.buildcraft.town)
├── worker/             # Cloudflare Worker: /dl/* download counter + /license/* Linux licensing API (Razorpay)
├── landing-page/       # Older marketing site
└── .github/workflows/  # CI (macOS + Windows + Linux builds, site update)
```

## License

MIT © [BuildCraft Labs](https://github.com/BuildCraftLabsLLP)
