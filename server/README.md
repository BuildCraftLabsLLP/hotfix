# Hotfix Server (`hotfixd`)

A headless service for **Linux, Windows and macOS** that watches every
process's CPU use and kills (or, in monitor mode, only reports) processes that
stay above a threshold for too long. One CGO-free Go binary per OS/CPU
(x86-64, arm64). Product page: <https://hotfix.buildcraft.town/server.html>.

**Paid:** $1 per server per month, 14-day free trial per server. The code is
public but **not MIT**: this directory is under the [Elastic License 2.0](LICENSE)
(you may not remove or circumvent the license check, or resell it as a service).

## Install

| OS | Command |
|---|---|
| Ubuntu / Debian | `curl -fsSL -o hs.deb https://hotfix.buildcraft.town/dl/server-linux && sudo apt install ./hs.deb` |
| RHEL / Fedora / openSUSE | `curl -fsSL -o hs.rpm https://hotfix.buildcraft.town/dl/server-linux-rpm && sudo dnf install ./hs.rpm` |
| Any Linux | `curl -fsSL https://hotfix.buildcraft.town/dl/server-linux-tar \| tar -xz && sudo ./HotfixServer-*/install.sh` |
| macOS | `curl -fsSL https://hotfix.buildcraft.town/dl/server-macos \| tar -xz && sudo ./HotfixServer-*/install.sh` |
| Windows (admin PowerShell) | download `/dl/server-windows`, unzip, run `install.ps1` |

`install.sh` / `install.ps1` take an optional license key and upgrade an
existing install in place. The `.deb`/`.rpm` enable and start the service.

## Use

```
hotfixd status                      what it's doing: hot processes, recent kills, license
hotfixd check                       measure now; show what would happen to each process
hotfixd config show | path
sudo hotfixd config set <key> <value>   (picked up within seconds; lists are comma-separated)
sudo hotfixd license activate HFXS-…    use a seat of your license on this server
hotfixd license status | deactivate | refresh | reset-identity
sudo hotfixd install | uninstall | start | stop | restart
hotfixd run                         foreground (what the service runs)
```

### Settings (`config.json`)

| Key | Default | Meaning |
|---|---|---|
| `mode` | `monitor` | `monitor` only reports ("would kill"); `enforce` kills |
| `cpu_threshold` | `90` | percent of **one core** (like `top`; may exceed 100) |
| `kill_duration` | `300` | seconds a process must stay above the threshold |
| `term_grace_seconds` | `10` | SIGTERM → SIGKILL delay (Linux/macOS; Windows terminates at once) |
| `whitelist` | `[]` | process names never killed (case-insensitive; `.exe` optional) |
| `protect_users` | `[]` | users whose processes are never killed (e.g. `postgres`, `CORP\svc-sql`) |
| `only_users` | `[]` | if set, only these users' processes are candidates |
| `skip_containers` | `false` | never kill processes in containers (Linux) |
| `webhook_url` | | JSON POST (Slack-compatible `text`) on every kill / would-kill |
| `https_proxy` | | proxy for the webhook and license checks |
| `license_key_file` | | file with a key to activate automatically (images, autoscaling); or `HOTFIXD_LICENSE_KEY` |

Never killed, whatever the settings: kernel threads, PID 1 and other reserved
PIDs, hotfixd and its children, and a built-in list per OS (`protected_*.go`:
init/systemd, sshd, journald, container runtimes, package managers, Windows
core services and Defender, macOS launchd/WindowServer/…). A process name
killed 3 times within an hour is left alone for the rest of the hour (it is
being restarted by something; fighting it doesn't help).

### Files

| | Linux | Windows | macOS |
|---|---|---|---|
| Config | `/etc/hotfix-server/config.json` | `%ProgramData%\Hotfix Server\config.json` | `/Library/Application Support/Hotfix Server/config.json` |
| State (`status.json`, `license.json`) | `/var/lib/hotfix-server/` | `%ProgramData%\Hotfix Server\` | `/Library/Application Support/Hotfix Server/` |
| Log | journald + `/var/log/hotfix-server/hotfixd.log` | `…\logs\hotfixd.log` + Event Log (start/stop/errors) | `/Library/Logs/Hotfix Server/hotfixd.log` |
| Service | systemd `hotfixd` (hardened unit, `packaging/hotfixd.service`) | `hotfixd` (LocalSystem, delayed start, restarts on failure) | LaunchDaemon `town.buildcraft.hotfixd` |

`%ProgramData%\Hotfix Server` gets an ACL of SYSTEM/Administrators full,
Users read; `license.json` is admin-only (0600 on Unix).

## Licensing

- Each server holds a **seat** of a license (activation key `HFXS-…`), a
  lease renewed every 6 hours; a server that disappears frees its seat after
  8 days. Without a seat a server runs its **14-day trial**.
- Without a valid license or trial, enforce mode **only reports**; nothing is
  killed. A token stays valid for 7 days offline.
- Identity: an HMAC of the machine ID (`/etc/machine-id`, `MachineGuid`,
  `IOPlatformUUID`; the raw ID never leaves the machine) plus a random
  instance id per install. Cloned machines: `hotfixd license reset-identity`.
- Server side: `license-api/` (Node + PostgreSQL on Railway, `license.buildcraft.town`; see `license-api/README.md`).
- Builds without `-X main.licensePublicKey=…` are development builds: they
  don't enforce licensing and refuse `install` (set
  `HOTFIXD_ALLOW_DEV_INSTALL=1` to test one).

## Build and test

```bash
cd server
go vet ./... && go test ./...      # runs natively on each OS (incl. live process/kill tests)
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -ldflags "-X main.version=1.0.0" .
bash packaging/release.sh 0.0.0-test   # from the repo root: every archive + .deb/.rpm (needs nfpm)
```

CI: `.github/workflows/server.yml` — tests on Linux x64/arm64, Windows,
macOS arm64/x64; a real service end-to-end run on each (install, monitor
reports a CPU hog, enforce kills it, uninstall); the licensing API tests; a
licensing end-to-end run (real hotfixd against the real API run locally);
packaging with a .deb install/remove/purge test.

## Releasing

Hotfix Server ships on its **own** tags, `server-vX.Y.Z`, never marked
"latest" (the desktop apps' updaters and the website's desktop download
buttons follow the latest release; the desktop Build workflow skips
`server-v*` releases).

```bash
gh release create server-v1.0.0 --latest=false --title "Hotfix Server 1.0.0" --notes "…"
```

The Server workflow builds, tests and attaches `HotfixServer-v1.0.0-*`
assets. It refuses to release without the repo variable
`SERVER_LICENSE_PUBLIC_KEY`. The version comes from the tag (`-X
main.version`); there is nothing to bump by hand.
