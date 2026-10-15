# hotfix-dl — counting download redirect

A Cloudflare Worker on `hotfix.buildcraft.town/dl/*` that counts download-button
clicks (a proxy for **installs**) and redirects to the latest GitHub release
asset. The app's silent auto-updater fetches release assets directly and never
hits `/dl`, so these counts stay separate from update traffic.

Endpoints:

- `GET /dl/mac` → 302 to the latest `*-macOS.dmg`
- `GET /dl/win` → 302 to the latest `Hotfix-Setup-*-Windows.exe`
- `GET /dl/linux` → 302 to the latest `*-Linux-x86_64.deb` (Ubuntu/Debian installer)
- `GET /dl/linux-rpm` → `*-Linux-x86_64.rpm` (Fedora/openSUSE); `/dl/linux-tar` → `*-Linux-x86_64.tar.gz` (any distro); `/dl/linux-arm64` → `*-Linux-arm64.tar.gz`
- `GET /dl/stats?key=<STATS_TOKEN>` → JSON of all counters

## Deploy

```bash
cd worker
npm i -g wrangler        # or: npx wrangler ...
wrangler login

# 1. Create the KV namespace and paste its id into wrangler.toml
wrangler kv namespace create HOTFIX_DL

# 2. Set the stats token (and optionally a GitHub token for higher API limits)
wrangler secret put STATS_TOKEN
# wrangler secret put GITHUB_TOKEN

# 3. Ship it
wrangler deploy
```

Prerequisite: `buildcraft.town` must be an active zone in this Cloudflare
account with the `hotfix` record proxied (orange cloud) so the route can
intercept `/dl/*` before GitHub Pages serves the rest of the site.

## Reading counts

```bash
curl "https://hotfix.buildcraft.town/dl/stats?key=YOUR_TOKEN"
# { "count:mac": 128, "count:win": 74, "count:mac:2026-07-02": 5, ... }
```

`count:mac` / `count:win` are lifetime totals; the dated keys are per-day
buckets for trend lines.

## Hotfix Server downloads

This Worker also serves Hotfix Server downloads:
`/dl/server-linux` (.deb), `-linux-rpm`, `-linux-tar`, `-linux-arm64`
(+ `-rpm`, `-tar`), `/dl/server-windows` (+ `-arm64`), `/dl/server-macos`
(Apple Silicon), `/dl/server-macos-intel`. They take the newest `server-v*`
release (those are never "latest"), remembering the last good link in KV for
when GitHub's API is rate-limited.

Hotfix Server licensing runs on Railway, not here: see `license-api/README.md`.
