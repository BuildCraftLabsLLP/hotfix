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
   | `RAZORPAY_KEY_ID` | `rzp_test_…` (later `rzp_live_…`) |
   | `RAZORPAY_KEY_SECRET` | the key secret |
   | `RAZORPAY_SERVER_PLAN_ID` | `plan_…` from step 1 |
   | `RAZORPAY_WEBHOOK_SECRET` | any long random string (also used in step 5) |
   | `GITHUB_TOKEN` | optional: a read-only token, for the "new version available" lookup |
   | `TRIAL_DAYS`, `TRIAL_IP_DAILY` | optional: 14 and 25 by default |

   The service refuses to start if a required variable is missing (see the deploy
   logs); setting variables in the dashboard redeploys it. The four `RAZORPAY_*`
   variables are all or nothing: with **none** set, billing is off. Trials and
   `hotfixd` license calls work, the buy page says "Purchases open soon", and
   checkout/manage/recover/webhook answer 503. Setting all four turns it on.
2. Backups: see [Backups](#backups) (the `pg-backup` service and `backups` bucket in the same file).
3. `railway domain status <id>` (or the service's **Networking** tab) shows the
   DNS records for `license.buildcraft.town`.

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
