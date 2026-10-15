# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## What This Is

Hotfix is a dual-platform (macOS + Windows) system tray app that monitors CPU usage and kills runaway processes before fans spin up. The macOS app is written in Swift/SwiftUI; the Windows app is written in Go.

## Build Commands

### macOS
```bash
# Full build → dist/Hotfix.app + dist/Hotfix.dmg
bash scripts/build.sh

# Swift compile only (no bundling)
swift build -c release --arch arm64 --arch x86_64

# Run directly from build output (no DMG needed)
open dist/Hotfix.app
```

### Windows
```powershell
cd windows
# Embed the flame icon + version metadata (writes resource.syso), then build.
# resource.syso is git-ignored; without `go generate` the local exe has no icon.
go generate ./...
go build -ldflags "-H windowsgui -s -w" -o ..\dist\Hotfix.exe .

# Per-user installer → dist/Hotfix-Setup.exe (needs Inno Setup 6 / ISCC.exe)
& "C:\Program Files (x86)\Inno Setup 6\ISCC.exe" /DMyAppVersion=1.0.7 installer\hotfix.iss
```

> Icons live in `windows/assets/`: a theme-adaptive monochrome flame for the tray
> (`tray_white*.png` / `tray_black*.png`, mirroring the macOS menu-bar flame) and a
> colored `Hotfix.ico` for the .exe/installer. Regenerate all of them with
> `windows/assets/gen-icons.ps1` (PowerShell + .NET, no external deps).

## Release Process

1. Bump the version in: `Sources/Hotfix/UpdateChecker.swift` (`currentVersion`), `Resources/Info.plist` (`CFBundleShortVersionString` **and** bump `CFBundleVersion`), `windows/updater.go` (`currentVersion`), and the hardcoded version label in `windows/assets/settings.html` (About card). `windows/main.go` only references `updater.go`'s `currentVersion`, so no literal there. (Version-comparison fixtures in `windows/updater_test.go` are not app versions — leave them.) The exe-metadata version in `windows/versioninfo.json` and the installer version are **stamped automatically** from the release tag by CI — don't bump them by hand. The site's **download buttons carry no version** — they point at the counting redirect (`hotfix.buildcraft.town/dl/{mac,win}`), which resolves the latest release asset at request time, so there's nothing to bump in `docs/index.html`.
2. Create a GitHub release tagged `v<version>` — the `Build` workflow runs automatically on `macos-latest` and `windows-2025`. Each release gets **three** version+OS-named assets: `Hotfix-v<version>-macOS.dmg`, `Hotfix-v<version>-Windows.exe` (the **raw exe**, downloaded by the in-place auto-updater), and `Hotfix-Setup-v<version>-Windows.exe` (the **per-user installer**, what the website's Windows button links to). There are no longer any plain `Hotfix.dmg` / `Hotfix.exe` assets. Pages serves `docs/` from `main`. (The legacy `update-site` CI job that rewrote versioned download URLs in `docs/index.html` is now a **no-op** — the buttons point at the version-less `/dl/*` redirect; see **Website & download counter** below.)
3. A user-facing feature is **not shipped** until this release is cut and the Build run succeeds with all three assets attached — the website serves only released binaries.

> Logs: macOS → `~/Library/Logs/Hotfix/hotfix.log`; Windows → `%APPDATA%\Hotfix\hotfix.log`. Both surface in Settings via an in-app log viewer. Desktop notifications fire on every successful kill (macOS notification center; Windows WinRT toast).

## Architecture

### macOS (`Sources/Hotfix/`)

The app is a `MenuBarExtra`-only app (no Dock icon). Core objects are singletons shared via `@StateObject`:

- **`ProcessMonitor`** (`@MainActor` singleton) — 5-second `Timer` loop that reads process CPU via `ps`, tracks hot start times in a `[pid → TimeInterval]` dict, kills via `kill()`, and publishes `hotProcesses` / `isKilling` to the UI. Also listens for `NSWorkspace.willSleepNotification` to kill on sleep.
- **`PreferencesManager`** — Wraps `@AppStorage` for all settings (threshold, duration, kill-on-sleep, whitelist). Whitelist is stored as JSON in `UserDefaults`.
- **`UpdateChecker`** — Hits the GitHub releases API on launch; compares semver tags.
- **Open at Login** — The "Open at Login" toggle in Settings registers/unregisters Hotfix as a per-user login item via `SMAppService.mainApp` (`ServiceManagement`, macOS 13+ — no helper bundle or entitlement). `PreferencesManager.launchAtLogin` mirrors `SMAppService.mainApp.status` (the real source of truth) and is re-synced after every `setLaunchAtLogin(_:)` call, so the toggle reflects the actual system state even if registration is denied.
- **`SettingsWindowController`** / **`SettingsView`** — Native SwiftUI settings panel opened from the menu.
- **`MenuBarPopoverView`** — The popover shown when clicking the tray flame icon. Shows hot processes and quick toggles.
- **`Log`** (`Logger.swift`) — Thread-safe file logger (`logf("…")`) that appends to `~/Library/Logs/Hotfix/hotfix.log` (and stderr). Mirrors the Windows logger format. Desktop notifications use `UNUserNotificationCenter` (authorization requested at launch).
- **`CrashReporter`** (`CrashReporter.swift`) — Crash capture mirroring the Windows `crashreport.go`: an uncaught-`NSException` handler and async-signal-safe signal handlers (SIGSEGV/SIGABRT/SIGILL/SIGFPE/SIGBUS/SIGTRAP, covering Swift fatal-error traps) write a marker (`~/Library/Logs/Hotfix/lastcrash.txt`); on the next launch it opens a pre-filled, tokenless GitHub "New Issue" page and clears the marker. Armed in `HotfixApp.init`.

Safety exclusions (kernel_task, WindowServer, Finder, etc.) are hardcoded in `ProcessMonitor` and can never be overridden by user settings.

### Windows (`windows/`)

A single Go binary with `//go:build windows` on every file. No CGO; uses `github.com/getlantern/systray` for the system tray and `github.com/jchv/go-webview2` (pure Go, no CGO) for the settings window. It is **not** an Electron app, and runs **no local HTTP server / no open port**.

- **`main.go`** — Entry point: init logging, load config, hand control to `systray.Run`. Owns the tray icon: embeds the monochrome flame PNGs and picks white vs black at runtime from the `SystemUsesLightTheme` registry value (re-applied live when the user flips light/dark). The `//go:generate` directive here produces `resource.syso` (exe icon + version metadata) from `versioninfo.json` + `assets/Hotfix.ico`.
- **`monitor.go`** — 5-second poll loop using `wmic` CSV output. Tracks hot processes in `hotMap`, calls `taskkill /F` when threshold exceeded. Sleep detection via a PowerShell WMI event subscription.
- **`config.go`** — Reads/writes JSON config from `%APPDATA%\Hotfix\config.json`. Thread-safe via `sync.RWMutex`.
- **`startup.go`** — The "Run at Startup" toggle. Adds/removes a per-user autostart entry under the `HKCU\...\CurrentVersion\Run` key (no admin/UAC), pointing at the running exe (quoted). Uses `golang.org/x/sys/windows/registry` (already a transitive dep). The **registry is the source of truth**: `loadConfig` overwrites `Config.LaunchAtLogin` from the live registry on every load, so the toggle reflects reality even when the installer's `[Tasks] startupicon` (which writes the same value) or the user changed it outside the app. `applySavedConfig` applies the toggle non-fatally — a failure just self-corrects on the next load.
- **`settings_window.go`** — The settings UI: a **WebView2 popover** opened from the tray's "Settings…" item. Loads the embedded `assets/settings.html` directly (`SetHtml`) into a frameless, top-most window anchored at the work-area bottom-right (by the tray), and dismisses on click-away. The page calls Go directly through WebView2 **native bindings** (`hotfixGetConfig` / `hotfixSaveConfig` / `hotfixGetLog` / `hotfixCheckUpdates` / `hotfixOpenLog`) — there is no HTTP server. Save validation + monitor start/stop live in `applySavedConfig`. Runs on a dedicated locked OS thread. If the WebView2 runtime is missing (rare on Win11), it toasts and opens the runtime download page.
- **`updater.go`** — Silent background self-update (mirrors the macOS updater): polls the GitHub releases API ~30s after launch and every 6h, downloads the **raw** `Hotfix-v…-Windows.exe` asset (`pickRawExeURL` skips the `Hotfix-Setup-*` installer), swaps it onto the running exe via a hidden PowerShell, and relaunches — no prompts. This works without elevation because the app installs **per-user** under `%LOCALAPPDATA%\Programs\Hotfix`.
- **`installer/hotfix.iss`** — Inno Setup script for the per-user installer (`PrivilegesRequired=lowest` → installs to `%LOCALAPPDATA%\Programs\Hotfix`, no admin/UAC). Adds a Start-Menu shortcut, an optional run-at-login entry (the `[Tasks] startupicon` — writes the **same** `HKCU\...\Run` value the in-app "Run at Startup" toggle manages; see `startup.go`), and a proper uninstaller in "Apps & features". The downloaded `Hotfix-Setup-*.exe` is freely deletable after install.
- **`notify.go`** — Desktop toast notifications via hidden PowerShell (WinRT `Windows.UI.Notifications` toast, with a `NotifyIcon` balloon-tip fallback). Title/body are passed through env vars to avoid quoting/injection. Called from `notifyKilled` in addition to the tray-label update. File logging is handled by `initLog`/`logf` in `main.go` (writes to `%APPDATA%\Hotfix\hotfix.log`).

All console-spawning child processes (`wmic`, `taskkill`, `powershell`) use `HideWindow: true` in `SysProcAttr` to prevent flash windows (since the binary is built with `-H windowsgui`).

### Website & download counter (`docs/`, `worker/`)

The marketing site lives in `docs/` (served by GitHub Pages from `main` at `hotfix.buildcraft.town`). Its Download buttons point at **`hotfix.buildcraft.town/dl/mac`** and **`/dl/win`**, handled by a Cloudflare Worker in `worker/` (`worker/src/worker.js`, config `worker/wrangler.toml`):

- On each hit it increments a per-platform counter in **Workers KV** (`count:{mac,win}` lifetime totals plus `count:{platform}:YYYY-MM-DD` daily buckets), then **302-redirects to the latest release asset**, resolved live from the GitHub releases API (cached ~300s). So the buttons never need per-release version bumps.
- The app's **silent auto-updater fetches release assets directly and never hits `/dl`**, so these counts approximate **fresh installs**, kept separate from update traffic. It's a fuzzy proxy: it can't tell a new user from a re-download, and KV's eventual consistency can drop the odd concurrent increment.
- **`GET /dl/stats?key=<STATS_TOKEN>`** returns the counters as JSON. `STATS_TOKEN` (and an optional `GITHUB_TOKEN` for higher GitHub-API limits) are Cloudflare **secrets** set via `wrangler secret put` — never committed.
- Deploy with `wrangler deploy` from `worker/` (see `worker/README.md`). Requires `buildcraft.town` DNS proxied through Cloudflare so the `/dl/*` route intercepts before Pages.

## Key Constraints

- **Tests** — Go unit tests live in `windows/*_test.go` (build-tagged `//go:build windows`). They run on the Windows CI runner via `go test ./...`. Swift tests run via `swift test` on the macOS runner. There is no way to execute the Windows tests locally on macOS.
- **Version must be bumped in multiple files** — forgetting one will cause the update checker to behave incorrectly or CI to produce a mismatched binary.
- macOS binary is **not notarized**; users must right-click → Open on first launch.
- Windows build sets `-H windowsgui`, so `fmt.Print` / `log` output goes nowhere — use the file logger (`initLog` / `logf`).
