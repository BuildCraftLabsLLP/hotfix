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

## Hotfix Server licensing (`src/server-api.js`)

A **separate Worker** (`wrangler.server.toml`, name `hotfix-server-api`) on
`hotfix.buildcraft.town/api/server/*`, so its setup can never block a `/dl`
deploy. It sells Hotfix Server ($1 per server per month on Razorpay
Subscriptions, quantity = servers), issues license keys, hands out seats and
14-day trials, and signs the tokens `hotfixd` checks (`server/license.go`).
Details are in the header comment of `src/server-api.js`.

One-time setup:

1. **Razorpay** (start in Test Mode): enable Subscriptions; create a monthly
   Plan at **$1.00** (USD needs international payments enabled); create API
   keys. Add a webhook to `https://hotfix.buildcraft.town/api/server/webhook`
   for the `subscription.*` events, with a secret of your choice.
2. **Cloudflare** (from `worker/`):

   ```bash
   wrangler d1 create hotfix-server                 # paste database_id into wrangler.server.toml
   wrangler kv namespace create HOTFIX_SERVER_KV    # paste the id into wrangler.server.toml
   # set RAZORPAY_KEY_ID and RAZORPAY_SERVER_PLAN_ID under [vars] in wrangler.server.toml
   wrangler d1 migrations apply hotfix-server --remote -c wrangler.server.toml
   node scripts/gen-server-license-key.mjs          # prints a public and a private key
   wrangler secret put SERVER_LICENSE_SIGNING_KEY -c wrangler.server.toml   # the PRIVATE key
   wrangler secret put RAZORPAY_KEY_SECRET -c wrangler.server.toml
   wrangler secret put RAZORPAY_WEBHOOK_SECRET -c wrangler.server.toml
   wrangler deploy -c wrangler.server.toml
   ```
3. **GitHub**: repo variable `SERVER_LICENSE_PUBLIC_KEY` = the PUBLIC key.
   CI compiles it into `hotfixd` and refuses to release without it.

Back up the private key: losing or rotating it stops every installed hotfixd
from accepting tokens until it's updated.

Tests: `node --test worker/test/server-api.test.mjs` (Node 24+, real SQLite via
`node:sqlite`). `worker/test/server-api-harness.mjs` serves the API locally
for end-to-end runs of hotfixd.

The download Worker also serves Hotfix Server downloads:
`/dl/server-linux` (.deb), `-linux-rpm`, `-linux-tar`, `-linux-arm64`
(+ `-rpm`, `-tar`), `/dl/server-windows` (+ `-arm64`), `/dl/server-macos`
(Apple Silicon), `/dl/server-macos-intel`. They take the newest `server-v*`
release (those are never "latest"), remembering the last good link in KV for
when GitHub's API is rate-limited.
