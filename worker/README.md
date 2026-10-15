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

The Linux app has a **7-day free trial, then a $1/month subscription locked to
one computer**. There are no license keys: the app sends an opaque fingerprint
of its machine, and the trial and the Razorpay subscription are bound to that
fingerprint. Code: `src/license.js`.

| Endpoint | Purpose |
|----------|---------|
| `GET /license/verify?fp=<fp>` | Signed token saying whether this machine may run — subscribed, or still in its free trial (called by the app at startup and every 6h) |
| `GET /license/checkout?fp=<fp>` | Creates a Razorpay subscription for this machine and opens Razorpay Checkout on it |
| `POST /license/success?fp=<fp>` | Checkout's callback: verifies the payment signature and activates the machine immediately |
| `GET /license/portal?fp=<fp>` | "Manage Subscription…" page: status, next payment, Cancel button |
| `POST /license/cancel?fp=<fp>` | Cancels (at the end of the paid month; right away if nothing was charged yet) |
| `POST /license/webhook` | Razorpay webhook — keeps status in sync on renewal, failure, cancellation |

**The free trial** needs no card. It starts the first time a machine checks in
(`trial:<fingerprint>` in KV, never reset — reinstalling doesn't renew it) and
lasts `TRIAL_DAYS` (default 7). A trial token expires with the trial, so going
offline can't stretch it. Subscribing during the trial doesn't cut it short:
the subscription's `start_at` is set to the trial's end, so the customer only
authorises the payment method now (Razorpay may take a small refundable
authorisation amount) and the first $1 is charged when the trial would have
ended.

State is one KV record per machine (`lic:<fingerprint>` in the `HOTFIX_LICENSE`
namespace). `active` and `authenticated` (payment set up, first charge still
ahead) subscriptions unlock the app; `pending` (a failed charge being retried),
`halted`, `cancelled` and the rest don't. Webhooks re-read the subscription
from Razorpay's API rather than trusting the event body, so replayed or
out-of-order events can't set a stale status.

**Where the secrets live.** The Razorpay key secret, the webhook secret and the
token-signing private key exist only as Cloudflare secrets. The app contains no
Razorpay key at all — it just opens `/license/checkout` in the browser — and
only the *public* half of the signing key, which can verify tokens but not
create them. The Razorpay key *id* is public by design (Checkout runs in the
browser with it).

### One-time setup

In the **Razorpay Dashboard** (start in Test Mode):

1. Enable **Subscriptions**.
2. **Subscriptions → Plans → Create Plan**: billing frequency *monthly*, every
   *1* month, amount **$1.00** (USD needs international payments enabled on the
   account; otherwise use an INR price and update the "$1" wording in the app,
   the site and `src/license.js`). Note the `plan_…` id.
3. **Account & Settings → API keys**: generate a key (`rzp_test_…` id + secret).

Then:

```bash
cd worker

# 4. KV namespace for license records — paste the id into wrangler.toml
#    (replacing REPLACE_WITH_HOTFIX_LICENSE_NAMESPACE_ID)
wrangler kv namespace create HOTFIX_LICENSE

# 5. In wrangler.toml [vars], set RAZORPAY_KEY_ID (rzp_…) and RAZORPAY_PLAN_ID (plan_…).

# 6. Token-signing key pair. Prints a public and a private key.
node scripts/gen-license-key.mjs
wrangler secret put LICENSE_SIGNING_KEY      # paste the PRIVATE key

# 7. Razorpay key secret
wrangler secret put RAZORPAY_KEY_SECRET

# 8. Deploy, then add the webhook in Razorpay (Account & Settings → Webhooks):
#      URL:    https://hotfix.buildcraft.town/license/webhook
#      Secret: any long random string
#      Events: subscription.authenticated, subscription.activated,
#              subscription.charged, subscription.pending, subscription.halted,
#              subscription.cancelled, subscription.completed,
#              subscription.updated, subscription.paused, subscription.resumed
#    and store the same secret here:
wrangler deploy
wrangler secret put RAZORPAY_WEBHOOK_SECRET
```

9. Put the **public** key from step 6 in the GitHub repo as the Actions
   *variable* `LICENSE_PUBLIC_KEY` (Settings → Secrets and variables → Actions →
   Variables). CI compiles it into the Linux binary and refuses to cut a release
   without it.

To go live, create the same Plan in Live Mode, switch `RAZORPAY_KEY_ID` /
`RAZORPAY_PLAN_ID` to the live values, `wrangler secret put RAZORPAY_KEY_SECRET`
with the live secret, add the webhook again in Live Mode, and redeploy.

Back up the private signing key. If it is lost or rotated, every installed
Linux build stops accepting tokens until it updates to a build with the new
public key.

### Testing

```bash
node --test worker/test/license.test.mjs   # from the repo root; no dependencies
```

For an end-to-end run use Test Mode keys and one of Razorpay's test cards
(e.g. Visa `4384 7968 2770 3274`, any future expiry, any CVV) before switching
to live keys.

### Limits worth knowing

- **Moving to a new computer** (or reinstalling the OS, which regenerates
  `/etc/machine-id`) means cancelling from "Manage Subscription…" and
  subscribing again from the new machine; there is no transfer flow. The new
  machine gets its own free trial.
- Razorpay has no customer self-service portal, so "Manage Subscription…" only
  shows the status and cancels. To change the card, cancel and subscribe again
  after the paid month ends.
- The app is open source, so this deters casual sharing rather than a
  determined user: anyone can build it themselves without the check.
