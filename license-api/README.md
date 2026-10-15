# Hotfix Server licensing API (`license-api/`)

The backend that sells and licenses Hotfix Server: **$1 per server per month**
on Razorpay Subscriptions, license keys, server seats and 14-day trials, and
the Ed25519-signed tokens `hotfixd` checks (`server/license.go`).

- **Runs on [Railway](https://railway.com)** at `https://license.buildcraft.town`
  (`hotfixd` and the website use `…/api/server/*`).
- **Node 24**, one dependency (`pg`). `src/server.js` is the HTTP entry point,
  `src/app.js` the request handler (its header comment lists every route),
  `src/db.js` the database layer.
- **PostgreSQL** (Railway's). `migrations/*.sql` are applied at startup and
  recorded in `schema_migrations`, so a deploy migrates by itself.
- Licensed under the Elastic License 2.0, like `server/`.

## Set it up on Railway (once)

**1. Razorpay** (start in Test Mode):
- Enable Subscriptions.
- Create a monthly Plan at **$1.00** (USD needs international payments enabled on the account).
- Create API keys (key id + secret).

**2. Signing key** — on your computer, from this folder:
```bash
node scripts/gen-license-key.mjs
```
It prints a **public** key and a **private** key. Keep the private key safe
(back it up): losing or rotating it stops every installed hotfixd from
accepting tokens until it's updated.

**3. Railway project:**
1. **New Project → Deploy from GitHub repo** → `BuildCraftLabsLLP/hotfix`.
2. In the service's **Settings**: **Root Directory** `/license-api`, and **Config file
   path** `/license-api/railway.json` (it builds the Dockerfile and health-checks `/healthz`).
3. **New → Database → PostgreSQL** in the same project. Enable its backups.
4. In the service's **Variables**:

   | Variable | Value |
   |---|---|
   | `DATABASE_URL` | `${{Postgres.DATABASE_URL}}` (reference to the database) |
   | `SERVER_LICENSE_SIGNING_KEY` | the **private** key from step 2 |
   | `RAZORPAY_KEY_ID` | `rzp_test_…` (later `rzp_live_…`) |
   | `RAZORPAY_KEY_SECRET` | the key secret |
   | `RAZORPAY_SERVER_PLAN_ID` | `plan_…` from step 1 |
   | `RAZORPAY_WEBHOOK_SECRET` | any long random string (also used in step 5) |
   | `GITHUB_TOKEN` | optional: a read-only token, for the "new version available" lookup |
   | `TRIAL_DAYS`, `TRIAL_IP_DAILY` | optional: 14 and 25 by default |

   The service refuses to start if a required variable is missing (see the deploy logs).
5. **Settings → Networking → Custom Domain** → `license.buildcraft.town`. Railway
   shows the DNS records to add.

**4. DNS** (Cloudflare, where `buildcraft.town` lives): add the records Railway
showed (a CNAME for `license`, and a TXT record if asked) as **DNS only (grey
cloud), not proxied**. The API reads the client's address from Railway's
`X-Real-IP`; behind Cloudflare's proxy every customer would look like a
Cloudflare address and trip the per-network trial cap.

**5. Razorpay webhook:** `https://license.buildcraft.town/api/server/webhook`,
secret = `RAZORPAY_WEBHOOK_SECRET`, events: all `subscription.*`.

**6. GitHub:** repo variable `SERVER_LICENSE_PUBLIC_KEY` = the **public** key
from step 2. The Server workflow compiles it into `hotfixd` and refuses to
release without it.

Check it: `https://license.buildcraft.town/healthz` says `ok`, and
`https://license.buildcraft.town/api/server/buy` shows the buy page. Make a
test purchase with a Razorpay test card (e.g. Visa `4384 7968 2770 3274`, any
future expiry, any CVV), then switch the Razorpay variables to live keys.

Deploys: every push to `main` that changes `license-api/` redeploys
(`watchPatterns` in `railway.json`).

## Develop and test

```bash
cd license-api
npm install
npm test                                              # PGlite: real Postgres in WebAssembly, nothing to install
TEST_DATABASE_URL=postgres://… npm test               # against a real PostgreSQL (CI does both)
node test/harness.mjs 8787 /tmp/lic                   # serve the API locally with a seeded license
```

CI (`.github/workflows/server.yml`, job `api`) also starts the real
`src/server.js` against PostgreSQL 16 twice (the second start proves migrations
are idempotent) and builds the Docker image; job `license-e2e` runs an
installed hotfixd against this API.
