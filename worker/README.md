# hotfix-dl — counting download redirect

A Cloudflare Worker on `hotfix.buildcraft.town/dl/*` that counts download-button
clicks (a proxy for **installs**) and redirects to the latest GitHub release
asset. The app's silent auto-updater fetches release assets directly and never
hits `/dl`, so these counts stay separate from update traffic.

Endpoints:

- `GET /dl/mac` → 302 to the latest `*-macOS.dmg`
- `GET /dl/win` → 302 to the latest `Hotfix-Setup-*-Windows.exe`
- `GET /dl/linux` → 302 to the latest `*-Linux-x86_64.tar.gz` (`/dl/linux-arm64` for the arm64 bundle)
- `GET /dl/stats?key=<STATS_TOKEN>` → JSON of all counters

The same Worker also serves the **licensing API** for the Linux app on
`hotfix.buildcraft.town/license/*` — see [Licensing](#licensing-linux-subscription) below.

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

## Licensing (Linux subscription)

The Linux app is a **$1/month subscription locked to one computer**. There are
no license keys: the app sends an opaque fingerprint of its machine, and a
Stripe subscription is bound to that fingerprint. Code: `src/license.js`.

| Endpoint | Purpose |
|----------|---------|
| `GET /license/verify?fp=<fp>` | Signed token saying whether this machine has an active subscription (called by the app at startup and every 6h) |
| `GET /license/checkout?fp=<fp>` | Creates a Stripe Checkout Session ($1/month) for this machine and redirects to it |
| `GET /license/success?session_id=…` | Post-payment page; activates the machine immediately |
| `GET /license/portal?fp=<fp>` | Redirects to the Stripe billing portal (update card / cancel) |
| `POST /license/webhook` | Stripe webhook — keeps status in sync on renewal, failure, cancellation |

State is one KV record per machine (`lic:<fingerprint>` in the `HOTFIX_LICENSE`
namespace). Only `active`/`trialing` subscriptions unlock the app.

**Where the secrets live.** The Stripe secret key, the webhook signing secret
and the token-signing private key exist only as Cloudflare secrets. The app
contains no Stripe key at all — it just opens `/license/checkout` in the
browser — and only the *public* half of the signing key, which can verify
tokens but not create them.

### One-time setup

```bash
cd worker

# 1. KV namespace for license records — paste the id into wrangler.toml
#    (replacing REPLACE_WITH_HOTFIX_LICENSE_NAMESPACE_ID)
wrangler kv namespace create HOTFIX_LICENSE

# 2. Token-signing key pair. Prints a public and a private key.
node scripts/gen-license-key.mjs
wrangler secret put LICENSE_SIGNING_KEY      # paste the PRIVATE key

# 3. Stripe API key (Dashboard → Developers → API keys)
wrangler secret put STRIPE_SECRET_KEY        # sk_live_... (or sk_test_... while testing)

# 4. Deploy, then create the webhook in Stripe (Dashboard → Developers → Webhooks):
#      URL:    https://hotfix.buildcraft.town/license/webhook
#      Events: checkout.session.completed, customer.subscription.created,
#              customer.subscription.updated, customer.subscription.deleted
#    and store its signing secret:
wrangler deploy
wrangler secret put STRIPE_WEBHOOK_SECRET    # whsec_...
```

5. Put the **public** key from step 2 in the GitHub repo as the Actions
   *variable* `LICENSE_PUBLIC_KEY` (Settings → Secrets and variables → Actions →
   Variables). CI compiles it into the Linux binary and refuses to cut a release
   without it.
6. In Stripe, enable the **customer portal** (Settings → Billing → Customer
   portal) so "Manage Subscription…" in the app works.

The price is created inline at checkout ($1.00 USD, monthly). To sell a Price
you manage in the Stripe dashboard instead, set `STRIPE_PRICE_ID` under `[vars]`
in `wrangler.toml`.

Back up the private signing key. If it is lost or rotated, every installed
Linux build stops accepting tokens until it updates to a build with the new
public key.

### Testing

```bash
node --test worker/test/license.test.mjs   # from the repo root; no dependencies
```

Use Stripe **test mode** keys and card `4242 4242 4242 4242` for an end-to-end
run before switching the secrets to live keys.

### Limits worth knowing

- **Moving to a new computer** (or reinstalling the OS, which regenerates
  `/etc/machine-id`) means cancelling in the billing portal and subscribing
  again from the new machine; there is no transfer flow.
- The app is open source, so this deters casual sharing rather than a
  determined user: anyone can build it themselves without the check.
