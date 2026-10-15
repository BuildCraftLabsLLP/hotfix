# Hotfix

**Keep your machine cool.** Hotfix monitors CPU usage and automatically terminates runaway processes before your fan spins up and your battery drains — on both macOS and Windows.

Built by [BuildCraft Labs](https://github.com/buildcraftlabs).

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
| **macOS** | [Hotfix.dmg](https://github.com/buildcraftlabs/hotfix/releases/latest) | macOS 13+ · Apple Silicon or Intel |
| **Windows** | [Hotfix-Setup.exe](https://github.com/buildcraftlabs/hotfix/releases/latest) | Windows 11 · x64 |

> **Windows:** the installer is per-user (no admin) — it installs to `%LOCALAPPDATA%\Programs\Hotfix`, adds an uninstaller to **Apps & features**, and updates itself silently in the background. Delete the downloaded `Hotfix-Setup.exe` once it's installed.

> **macOS:** Not yet notarized — right-click → **Open** on first launch to bypass Gatekeeper.

## Features

- **Native system tray** — Lives in your menu bar / taskbar, no Dock or taskbar icon
- **Configurable threshold** — Set the CPU % that triggers monitoring (default: 80%)
- **Configurable duration** — How long a process must be hot before being killed (default: 60s)
- **Kill on sleep** — Optionally terminate hot processes when the machine sleeps
- **Exclusion list** — Protect specific processes from ever being killed
- **Desktop notifications** — A notification fires whenever a process is terminated
- **Activity logs** — Events are written to a log file and viewable in-app from Settings (macOS: `~/Library/Logs/Hotfix/hotfix.log`, Windows: `%APPDATA%\Hotfix\hotfix.log`)
- **Silent auto-updates** — New versions are downloaded and installed in the background, then the app relaunches — no prompts
- **Crash reporting** — On both macOS and Windows, a crash opens a pre-filled GitHub issue on the next launch (you review before submitting)
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

## Build from Source

### macOS
Requires Xcode Command Line Tools.

```bash
git clone https://github.com/buildcraftlabs/hotfix.git
cd hotfix
bash scripts/build.sh
open "dist/Hotfix.dmg"
```

### Windows
Requires Go 1.22+.

```powershell
git clone https://github.com/buildcraftlabs/hotfix.git
cd hotfix\windows
go generate ./...   # embeds the flame icon + version metadata (resource.syso)
go build -ldflags "-H windowsgui -s -w" -o ..\dist\Hotfix.exe .

# Optional: build the per-user installer (requires Inno Setup 6)
& "C:\Program Files (x86)\Inno Setup 6\ISCC.exe" /DMyAppVersion=1.0.7 installer\hotfix.iss
```

## Releasing a New Version

Releasing is fully automated. Both binaries are built and attached by CI on every new release.

1. Bump the version in `Sources/Hotfix/UpdateChecker.swift`, `Resources/Info.plist` (short string + build number), `windows/updater.go`, and the About label in `windows/assets/settings.html`. (The exe metadata in `windows/versioninfo.json` and the installer version are stamped automatically by CI — see step 3. The website's download buttons carry no version — they point at a redirect that always resolves the latest release — so there's nothing to bump there.)
2. Create a GitHub release tagged `v<version>`
3. The `Build` workflow runs on `macos-latest` + `windows-2025` and attaches **three** version+OS-named assets: `Hotfix-v<version>-macOS.dmg`, `Hotfix-v<version>-Windows.exe` (raw exe for the auto-updater), and `Hotfix-Setup-v<version>-Windows.exe` (the per-user installer the website links to). No site update is needed: the website's download buttons route through a Cloudflare Worker (`worker/`) that always resolves the latest release, and it also counts installs — see the Worker's own README

Users on both platforms will be notified of the update on next launch.

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
├── icon/               # App icon assets
├── docs/               # Marketing website (GitHub Pages, hotfix.buildcraft.town)
├── worker/             # Cloudflare Worker: /dl/* download-counter + redirect
├── landing-page/       # Older marketing site
└── .github/workflows/  # CI (macOS + Windows builds, site update)
```

## License

MIT © [BuildCraft Labs](https://github.com/buildcraftlabs)
