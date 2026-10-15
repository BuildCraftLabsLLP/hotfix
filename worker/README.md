# hotfix-dl — counting download redirect

A Cloudflare Worker on `hotfix.buildcraft.town/dl/*` that counts download-button
clicks (a proxy for **installs**) and redirects to the latest GitHub release
asset. The app's silent auto-updater fetches release assets directly and never
hits `/dl`, so these counts stay separate from update traffic.

Endpoints:

- `GET /dl/mac` → 302 to the latest `*-macOS.dmg`
- `GET /dl/win` → 302 to the latest `Hotfix-Setup-*-Windows.exe`
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
