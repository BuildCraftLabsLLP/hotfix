# Hotfix Server licensing API (`license-api/`)

The backend that sells and licenses Hotfix Server: **$1 per server per month**
on [Dodo Payments](https://dodopayments.com) subscriptions (merchant of record:
they handle sales tax / VAT and invoices), license keys, server seats and 14-day trials, and
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

**1. Dodo Payments** (start in Test Mode; test and live have separate products, keys and webhooks):
- **Products → Create Product → Subscription**: "Hotfix Server", **$1.00 USD**, billed
  every **1 month**, with a long subscription period (e.g. 20 years: a period equal to
  the billing interval would expire after one charge). No trial (`hotfixd` has its
  own). Customers buy one unit per server. Copy its `pdt_…` id.
- **Settings → Subscriptions**: a grace period (e.g. 7 days) for failed renewals.
  Servers stay licensed while it runs (`past_due`), and stop once the
  subscription goes `on_hold`.
- **Developer → API Keys**: a key with **Write** access to Subscriptions, Payments
  (checkout sessions) and Customers (billing portal).

**2. Signing key** — on your computer, from this folder:
```bash
node scripts/gen-license-key.mjs
```
It prints a **public** key and a **private** key. Keep the private key safe
(back it up): losing or rotating it stops every installed hotfixd from
accepting tokens until it's updated.

**3. Railway project** (`hotfix-license`; already created). Its layout lives in
`.railway/railway.ts` (Railway's infrastructure-as-code: the `license-api`
service with its domain and `/healthz` health check, and the PostgreSQL
database with its volume). Variables are deliberately **not** in that file.
From this folder, after `railway login` and `railway link`:
```bash
npm install
railway config plan     # what would change in Railway
railway config apply
railway up --service license-api    # build the Dockerfile and deploy this folder
```
1. In the service's **Variables**:

   | Variable | Value |
   |---|---|
   | `DATABASE_URL` | `${{Postgres.DATABASE_URL}}` (reference to the database) |
   | `SERVER_LICENSE_SIGNING_KEY` | the **private** key from step 2 |
   | `DODO_PAYMENTS_ENVIRONMENT` | `test_mode` (later `live_mode`) |
   | `DODO_PAYMENTS_API_KEY` | the API key from step 1 |
   | `DODO_SERVER_PRODUCT_ID` | `pdt_…` from step 1 |
   | `DODO_PAYMENTS_WEBHOOK_KEY` | the webhook's signing secret (`whsec_…`) from step 5 |
   | `GITHUB_TOKEN` | optional: a read-only token, for the "new version available" lookup |
   | `TRIAL_DAYS`, `TRIAL_IP_DAILY` | optional: 14 and 25 by default |

   The service refuses to start if a required variable is missing (see the deploy
   logs); setting variables in the dashboard redeploys it. The four `DODO_*`
   variables are all or nothing: with **none** set, billing is off. Trials and
   `hotfixd` license calls work, the buy page says "Purchases open soon", and
   checkout/success/manage/recover/webhook answer 503. Setting all four (in one
   change, or the service refuses to start) turns it on.
2. Backups: see [Backups](#backups) (the `pg-backup` service and `backups` bucket in the same file).
3. `railway domain status <id>` (or the service's **Networking** tab) shows the
   DNS records for `license.buildcraft.town`.

**4. DNS** (Cloudflare, where `buildcraft.town` lives): add the records Railway
showed (a CNAME for `license`, and a TXT record if asked) as **DNS only (grey
cloud), not proxied**. The API reads the client's address from Railway's
`X-Real-IP`; behind Cloudflare's proxy every customer would look like a
Cloudflare address and trip the per-network trial cap.

**5. Dodo Payments webhook** (Developer → Webhooks → Add endpoint):
`https://license.buildcraft.town/api/server/webhook`, events: all
`subscription.*`. Copy its signing secret into `DODO_PAYMENTS_WEBHOOK_KEY`.
Every event is checked (Standard Webhooks signature, at most 5 minutes old)
and the subscription is then re-read from Dodo, so a replayed or out-of-order
event can't store a stale state.

How a purchase flows: `checkout` creates a Dodo checkout session and records
it (`checkouts` table) under a random ref, which goes into the return URL and
the session's metadata. A Dodo subscription only exists once it's paid, and
the return URL's `subscription_id` is unsigned, so `success` re-reads the
subscription and only shows keys if it's this product's and matches that ref
(through the metadata, or the checkout session of its first payment). Lost the
page? `recover` issues new keys for the payment id on Dodo's receipt email.

**6. GitHub:** repo variable `SERVER_LICENSE_PUBLIC_KEY` = the **public** key
from step 2. The Server workflow compiles it into `hotfixd` and refuses to
release without it.

Check it: `https://license.buildcraft.town/healthz` says `ok`, and
`https://license.buildcraft.town/api/server/buy` shows the buy page. Make a
test purchase with a Dodo test card (Visa `4242 4242 4242 4242`, expiry `06/32`,
CVV `123`), then do step 1 and 5 again in Live Mode and switch the four `DODO_*`
variables to the live values.

Deploys: `railway up --service license-api` from this folder. (To deploy on
every push to `main` instead, connect the service to the GitHub repo in its
Settings, with Root Directory `/license-api` and watch path `/license-api/**`.)

## Backups

Railway's own volume backups need a Pro workspace, so the project runs its own:
the **`pg-backup`** cron service (`backup/`) runs every night at 02:30 UTC. It
`pg_dump`s the database, checks the archive is readable, encrypts it with
[age](https://age-encryption.org) to `BACKUP_AGE_RECIPIENT` (a **public** key),
uploads it to the **`backups`** bucket (region `sjc`, away from the database)
as `license-db-<UTC time>.dump.age`, and deletes dumps older than 30 days
(`KEEP_DAYS`). A failed night shows as a failed run of `pg-backup`.

The **private** age key (`AGE-SECRET-KEY-1…`) is never on Railway: keep it with
the signing key. Without it the dumps can't be read (by anyone, us included).

`pg-backup` variables: `DATABASE_URL=${{Postgres.DATABASE_URL}}`,
`BACKUP_AGE_RECIPIENT=age1…`, and `BUCKET`, `ENDPOINT`, `ACCESS_KEY_ID`,
`SECRET_ACCESS_KEY`, `REGION` as references to the bucket
(`${{backups.BUCKET}}` …). Deploy it with
`railway up backup --path-as-root --service pg-backup`; run a backup now with
`railway cron run --service pg-backup` (or the service's "Run now").

**Restore** (needs `age` and PostgreSQL 18's `pg_restore`, or the `backup/`
Docker image, which has both):
1. Download the dump: the bucket's Credentials tab has S3 keys for any S3 client, e.g.
   `aws --endpoint-url https://t3.storageapi.dev s3 cp s3://<BUCKET>/license-db-….dump.age .`
2. Get a connection URL for the database (Postgres → Variables → `DATABASE_PUBLIC_URL`).
3. Stop `license-api` (or scale it to zero) so nothing writes during the restore.
4. `backup/restore.sh license-db-….dump.age <age key file> "$DATABASE_PUBLIC_URL"`.
   It replaces the tables in one transaction: if anything fails, nothing changes.
5. Start `license-api` again.

CI restores a real backup (into an empty database and over existing data)
on every change to `license-api/`.

## Develop and test

```bash
cd license-api
npm install
npm test                                              # PGlite: real Postgres in WebAssembly, nothing to install
TEST_DATABASE_URL=postgres://… npm test               # against a real PostgreSQL (CI does both)
node test/harness.mjs 8787 /tmp/lic                   # serve the API locally with a seeded license
```

CI (`.github/workflows/server.yml`, job `api`) also starts the real
`src/server.js` against PostgreSQL 18 (as on Railway) twice (the second start proves migrations
are idempotent) and builds the Docker image; job `license-e2e` runs an
installed hotfixd against this API.
