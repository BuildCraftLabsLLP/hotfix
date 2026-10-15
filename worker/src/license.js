// Licensing API for the Linux app: hotfix.buildcraft.town/license/*
//
// The Linux build has a 7-day free trial, then a $1/month subscription, locked
// to one machine. There are no license keys to share: the trial and the
// subscription are bound to the machine fingerprint the app sends (an opaque
// SHA-256 derived from /etc/machine-id), and the app asks this API whether
// *its* fingerprint may run.
//
// The trial needs no card: it starts the first time a machine checks in and
// lasts TRIAL_DAYS. Subscribing during the trial doesn't cut it short — the
// Razorpay subscription starts (first charge) when the free week ends.
//
// Payments are Razorpay Subscriptions on a Plan made in the Razorpay Dashboard.
//
//   GET  /license/verify?fp=<fp>    -> { token }  signed status for this machine
//   GET  /license/checkout?fp=<fp>  -> page that opens Razorpay Checkout for this machine
//   POST /license/success?fp=<fp>   Checkout's callback: verifies the payment, activates
//   GET  /license/portal?fp=<fp>    -> "Manage subscription" page (status, cancel)
//   POST /license/cancel?fp=<fp>    cancels the machine's subscription (from that page)
//   POST /license/webhook           Razorpay webhook (signature-verified)
//
// State lives in Workers KV (binding HOTFIX_LICENSE):
//   `lic:<fp>`      { subscription_id, customer_id, status, current_period_end, cancel_scheduled, updated }
//   `trial:<fp>`    { start }  — when the machine's free trial began (never reset)
//   `checkout:<fp>` id of the subscription the last checkout created (expires in a day)
//
// Secrets (wrangler secret put …) — none of these ever reach the client:
//   RAZORPAY_KEY_SECRET      Razorpay API key secret
//   RAZORPAY_WEBHOOK_SECRET  secret set on the /license/webhook webhook in the Dashboard
//   LICENSE_SIGNING_KEY      Ed25519 private key (base64 PKCS#8) — see scripts/gen-license-key.mjs
// Vars (wrangler.toml [vars]):
//   RAZORPAY_KEY_ID          Razorpay API key id (rzp_live_… / rzp_test_…; public — Checkout needs it)
//   RAZORPAY_PLAN_ID         the monthly Plan to subscribe machines to (plan_…)
//   TRIAL_DAYS               optional: length of the free trial in days (default 7)
//
// The verify response is a token signed with LICENSE_SIGNING_KEY; the app ships
// only the matching public key. A token names the fingerprint it was issued
// for, so it is useless on any other machine, and it can't be forged by
// redirecting the app to a fake server.

const FP_RE = /^[0-9a-f]{64}$/;
const SUB_RE = /^sub_[A-Za-z0-9]+$/;
const TOKEN_TTL = 72 * 3600; // seconds a token stays valid (the app's offline grace)
// Subscription statuses that unlock the app. "authenticated": the customer has
// set up the payment but the first charge is still ahead (subscribed during
// the free trial).
const ACTIVE = new Set(["active", "authenticated"]);
const RAZORPAY_API = "https://api.razorpay.com/v1";
const DAY = 86400;
const DEFAULT_TRIAL_DAYS = 7;
// Razorpay allows at most 10 years of billing cycles.
const TOTAL_MONTHS = 120;
// Only defer the first charge to the trial's end when that's still a while off
// (start_at must be in the future when Razorpay processes it).
const MIN_DEFERRED_START = 3600;
const CHECKOUT_TTL = DAY;

export async function handleLicense(request, env, url) {
  const seg = url.pathname.replace(/^\/license\/?/, "").replace(/\/$/, "");
  const method = request.method;
  try {
    switch (`${method} ${seg}`) {
      case "GET verify":
        return await verify(env, url);
      case "GET checkout":
        return await checkout(env, url);
      case "POST success":
        return await success(request, env, url);
      case "GET portal":
        return await portal(env, url);
      case "POST cancel":
        return await cancel(request, env, url);
      case "POST webhook":
        return await webhook(request, env);
      case "GET cancelled":
        return page("Checkout cancelled", "No payment was taken. You can subscribe any time from the Hotfix tray menu.");
    }
    const known = ["verify", "checkout", "success", "portal", "cancel", "webhook", "cancelled"];
    return known.includes(seg) ? text("Method not allowed", 405) : text("Not found", 404);
  } catch (err) {
    // Never leak internals (Razorpay errors can echo request details).
    console.error("license:", err && err.stack ? err.stack : err);
    return text("License service error", 500);
  }
}

// --- verify ---

async function verify(env, url) {
  const fp = fingerprint(url);
  if (!fp) return text("Invalid fingerprint", 400);

  const rec = await env.HOTFIX_LICENSE.get(`lic:${fp}`, "json");
  const now = Math.floor(Date.now() / 1000);
  const trialEnd = await machineTrialEnd(env, fp, now);

  // plan: "subscription" (paid), "trial" (free week), or why it may not run:
  // "trial_expired" (never subscribed) / "lapsed" (subscription ended).
  let status = "inactive";
  let plan = rec ? "lapsed" : "trial_expired";
  let exp = now + TOKEN_TTL;
  if (rec && ACTIVE.has(rec.status)) {
    status = "active";
    plan = "subscription";
  } else if (now < trialEnd) {
    status = "active";
    plan = "trial";
    // A trial token never outlives the trial, so offline grace can't stretch it.
    exp = Math.min(exp, trialEnd);
  }
  const claims = {
    fp,
    status,
    plan,
    iat: now,
    exp,
    period_end: (rec && rec.current_period_end) || 0,
    trial_end: trialEnd,
  };
  return new Response(JSON.stringify({ token: await signToken(env, claims) }), {
    headers: { "Content-Type": "application/json", "Cache-Control": "no-store" },
  });
}

// machineTrialEnd returns when the machine's free trial ends (unix seconds),
// starting the trial the first time the machine is seen. The start is kept for
// good, so a machine gets one trial — reinstalling the app doesn't renew it.
async function machineTrialEnd(env, fp, now) {
  const key = `trial:${fp}`;
  let trial = await env.HOTFIX_LICENSE.get(key, "json");
  if (!trial || typeof trial.start !== "number") {
    trial = { start: now };
    await env.HOTFIX_LICENSE.put(key, JSON.stringify(trial));
  }
  return trial.start + trialDays(env) * DAY;
}

function trialDays(env) {
  const days = parseInt(env.TRIAL_DAYS ?? "", 10);
  return Number.isFinite(days) && days >= 0 ? days : DEFAULT_TRIAL_DAYS;
}

// Token = base64url(JSON claims) + "." + base64url(Ed25519 signature over the
// encoded claims string). Mirrored by parseLicenseToken in linux/license.go.
export async function signToken(env, claims) {
  const key = await crypto.subtle.importKey(
    "pkcs8",
    b64decode(env.LICENSE_SIGNING_KEY),
    { name: "Ed25519" },
    false,
    ["sign"],
  );
  const body = b64url(new TextEncoder().encode(JSON.stringify(claims)));
  const sig = await crypto.subtle.sign("Ed25519", key, new TextEncoder().encode(body));
  return `${body}.${b64url(new Uint8Array(sig))}`;
}

// --- checkout ---

// Creates a Razorpay subscription for this machine and serves a page that opens
// Razorpay Checkout on it. Checkout posts the result to /license/success.
async function checkout(env, url) {
  const fp = fingerprint(url);
  if (!fp) return text("Invalid fingerprint", 400);

  // Don't sell a second subscription for a machine that already has one.
  const rec = await env.HOTFIX_LICENSE.get(`lic:${fp}`, "json");
  if (rec && ACTIVE.has(rec.status)) {
    return page("Already subscribed", "This computer already has an active Hotfix subscription. Choose “Refresh License” in the Hotfix tray menu.");
  }

  const now = Math.floor(Date.now() / 1000);
  const params = {
    plan_id: env.RAZORPAY_PLAN_ID,
    total_count: TOTAL_MONTHS,
    quantity: 1,
    customer_notify: true,
    // How later webhook events (renewals, cancellations) find their machine.
    notes: { fingerprint: fp },
    expire_by: now + CHECKOUT_TTL,
  };
  // Subscribing during the free trial: the first charge comes when the trial
  // would have ended, not today.
  const trialEnd = await machineTrialEnd(env, fp, now);
  if (trialEnd - now >= MIN_DEFERRED_START) {
    params.start_at = trialEnd;
  }

  const sub = await razorpay(env, "POST", "/subscriptions", params);
  // Remembered so /success checks the payment against the subscription *we*
  // created for this machine, never one named by the browser.
  await env.HOTFIX_LICENSE.put(`checkout:${fp}`, sub.id, { expirationTtl: CHECKOUT_TTL });

  const first = params.start_at
    ? `Nothing is charged today: your free trial continues, and the first $1 payment is on ${formatDate(params.start_at)}.`
    : "$1 per month for this computer. Cancel any time.";
  return checkoutPage(env, url, fp, sub.id, first);
}

// Checkout's callback (a form POST). Verifies the payment signature against the
// subscription created for this machine, then activates from Razorpay's own
// record of the subscription — the webhook stays the source of truth for
// renewals and cancellations.
async function success(request, env, url) {
  const fp = fingerprint(url);
  if (!fp) return text("Invalid fingerprint", 400);

  const form = await request.formData().catch(() => new FormData());
  const paymentId = String(form.get("razorpay_payment_id") || "");
  const signature = String(form.get("razorpay_signature") || "");
  const subId = await env.HOTFIX_LICENSE.get(`checkout:${fp}`);

  const notDone = () =>
    page("Payment not completed", "The payment didn't go through, so nothing was activated. You can try again from the Hotfix tray menu.");
  if (!paymentId || !signature || !subId) return notDone();
  if (!(await hmacHexEqual(env.RAZORPAY_KEY_SECRET, `${paymentId}|${subId}`, signature))) {
    return text("Invalid payment signature", 400);
  }

  const sub = await razorpay(env, "GET", `/subscriptions/${subId}`);
  if (!sub.notes || sub.notes.fingerprint !== fp || !ACTIVE.has(sub.status)) return notDone();
  await saveLicense(env, fp, sub);

  if (sub.status === "authenticated" && sub.charge_at) {
    return page("You're subscribed 🎉", `Thanks! Your free trial continues — the first $1 payment is on ${formatDate(sub.charge_at)}. You can close this tab.`);
  }
  return page("You're subscribed 🎉", "Thanks! Hotfix on this computer unlocks automatically within a few seconds — you can close this tab.");
}

// --- manage / cancel ---

async function portal(env, url) {
  const fp = fingerprint(url);
  if (!fp) return text("Invalid fingerprint", 400);

  const rec = await env.HOTFIX_LICENSE.get(`lic:${fp}`, "json");
  if (!rec || !rec.subscription_id) {
    return page("No subscription found", "This computer has no Hotfix subscription to manage.");
  }
  const sub = await razorpay(env, "GET", `/subscriptions/${rec.subscription_id}`);
  await saveLicense(env, fp, sub);

  if (!ACTIVE.has(sub.status)) {
    return page("Subscription ended", "This computer's Hotfix subscription has ended. You can subscribe again from the Hotfix tray menu.");
  }
  const fresh = await env.HOTFIX_LICENSE.get(`lic:${fp}`, "json");
  if (fresh && fresh.cancel_scheduled) {
    return page("Cancellation scheduled", `Hotfix stays unlocked on this computer until ${formatDate(sub.current_end)}; you won't be charged again.`);
  }
  const next = sub.charge_at ? `Next payment: $1 on ${formatDate(sub.charge_at)}.` : "";
  return managePage(url, fp, next);
}

// Cancels from the manage page. Before the first charge (subscribed during the
// trial) it ends now — the remaining free days still apply; otherwise at the
// end of the month already paid for.
async function cancel(request, env, url) {
  const fp = fingerprint(url);
  if (!fp) return text("Invalid fingerprint", 400);
  // Only our own manage page may post here.
  const origin = request.headers.get("Origin");
  if (origin && origin !== url.origin) return text("Forbidden", 403);

  const rec = await env.HOTFIX_LICENSE.get(`lic:${fp}`, "json");
  if (!rec || !rec.subscription_id || !ACTIVE.has(rec.status)) {
    return page("No subscription found", "This computer has no active Hotfix subscription to cancel.");
  }
  const atCycleEnd = rec.status === "active";
  const sub = await razorpay(env, "POST", `/subscriptions/${rec.subscription_id}/cancel`, {
    cancel_at_cycle_end: atCycleEnd,
  });
  await saveLicense(env, fp, sub, { cancel_scheduled: atCycleEnd });

  if (atCycleEnd && sub.current_end) {
    return page("Subscription cancelled", `You won't be charged again. Hotfix stays unlocked on this computer until ${formatDate(sub.current_end)}.`);
  }
  return page("Subscription cancelled", "You won't be charged. Any free-trial days left still apply on this computer.");
}

// --- webhook ---

// Every subscription.* event re-reads the subscription from Razorpay instead of
// trusting the event body's copy: events can arrive out of order or be
// replayed (Razorpay signatures carry no timestamp), and either way the stored
// state ends up matching Razorpay's current one.
async function webhook(request, env) {
  const body = await request.text();
  const ok = await verifyRazorpaySignature(body, request.headers.get("X-Razorpay-Signature") || "", env.RAZORPAY_WEBHOOK_SECRET);
  if (!ok) return text("Bad signature", 400);

  const event = JSON.parse(body);
  const entity = event.payload && event.payload.subscription && event.payload.subscription.entity;
  if (typeof event.event === "string" && event.event.startsWith("subscription.") && entity && SUB_RE.test(entity.id || "")) {
    const sub = await razorpay(env, "GET", `/subscriptions/${entity.id}`);
    const fp = sub.notes && sub.notes.fingerprint;
    if (FP_RE.test(fp || "")) await saveLicense(env, fp, sub);
  }
  return text("ok", 200);
}

// X-Razorpay-Signature: hex HMAC-SHA256 of the raw body, keyed with the
// webhook's secret.
export async function verifyRazorpaySignature(body, signature, secret) {
  return hmacHexEqual(secret, body, signature);
}

async function hmacHexEqual(secret, message, signature) {
  if (!secret || !signature) return false;
  const key = await crypto.subtle.importKey(
    "raw",
    new TextEncoder().encode(secret),
    { name: "HMAC", hash: "SHA-256" },
    false,
    ["sign"],
  );
  const mac = new Uint8Array(await crypto.subtle.sign("HMAC", key, new TextEncoder().encode(message)));
  const expected = [...mac].map((b) => b.toString(16).padStart(2, "0")).join("");
  return timingSafeEqual(signature.trim().toLowerCase(), expected);
}

function timingSafeEqual(a, b) {
  if (a.length !== b.length) return false;
  let diff = 0;
  for (let i = 0; i < a.length; i++) diff |= a.charCodeAt(i) ^ b.charCodeAt(i);
  return diff === 0;
}

// --- storage ---

// Record a subscription's state against its machine. An event for an old,
// ended subscription must not knock out a newer active one on the same machine
// (e.g. the user cancelled, then re-subscribed). cancel_scheduled (set by
// /cancel) sticks to its subscription.
async function saveLicense(env, fp, sub, { cancel_scheduled } = {}) {
  const key = `lic:${fp}`;
  const prev = await env.HOTFIX_LICENSE.get(key, "json");
  if (prev && prev.subscription_id !== sub.id && ACTIVE.has(prev.status) && !ACTIVE.has(sub.status)) {
    return;
  }
  const scheduled = cancel_scheduled ?? Boolean(prev && prev.subscription_id === sub.id && prev.cancel_scheduled);
  await env.HOTFIX_LICENSE.put(
    key,
    JSON.stringify({
      subscription_id: sub.id,
      customer_id: sub.customer_id || null,
      status: sub.status,
      current_period_end: sub.current_end || sub.charge_at || 0,
      cancel_scheduled: scheduled,
      updated: Math.floor(Date.now() / 1000),
    }),
  );
}

// --- helpers ---

function fingerprint(url) {
  const fp = url.searchParams.get("fp") || "";
  return FP_RE.test(fp) ? fp : null;
}

// Minimal Razorpay REST client: Basic auth, JSON both ways.
async function razorpay(env, method, path, body) {
  const init = {
    method,
    headers: { Authorization: `Basic ${btoa(`${env.RAZORPAY_KEY_ID}:${env.RAZORPAY_KEY_SECRET}`)}` },
  };
  if (body) {
    init.headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(body);
  }
  const res = await fetch(RAZORPAY_API + path, init);
  const data = await res.json();
  if (!res.ok) {
    throw new Error(`razorpay ${method} ${path}: ${res.status} ${(data.error && data.error.description) || ""}`);
  }
  return data;
}

function formatDate(unix) {
  return new Date(unix * 1000).toUTCString().slice(5, 16); // "22 Oct 2026"
}

function b64decode(s) {
  return Uint8Array.from(atob(s), (c) => c.charCodeAt(0));
}

function b64url(bytes) {
  let bin = "";
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

function text(body, status) {
  return new Response(body, { status, headers: { "Cache-Control": "no-store" } });
}

const esc = (s) =>
  String(s).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");

const STYLE = `
  body { margin: 0; min-height: 100vh; display: grid; place-items: center;
         font: 16px/1.5 system-ui, sans-serif; background: #14110f; color: #f3ece6; }
  main { max-width: 28rem; padding: 2rem; text-align: center; }
  h1 { font-size: 1.5rem; margin: 0 0 .75rem; }
  p { margin: 0 0 1rem; color: #c9bdb3; }
  button { font: inherit; font-weight: 600; padding: .7rem 1.4rem; border: 0; border-radius: .6rem;
           background: #f26b1d; color: #fff; cursor: pointer; }
  button.quiet { background: transparent; color: #c9bdb3; border: 1px solid #4a403a; }`;

function html(title, inner, headers = {}) {
  const doc = `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>Hotfix — ${esc(title)}</title>
<style>${STYLE}</style></head>
<body><main>${inner}</main></body></html>`;
  return new Response(doc, {
    headers: { "Content-Type": "text/html; charset=utf-8", "Cache-Control": "no-store", ...headers },
  });
}

function page(title, message) {
  return html(title, `<h1>${esc(title)}</h1><p>${esc(message)}</p>`);
}

// Opens Razorpay Checkout on load (and from the button, if the browser blocked
// it). Checkout posts the payment result to callback_url.
function checkoutPage(env, url, fp, subId, note) {
  const options = {
    key: env.RAZORPAY_KEY_ID,
    subscription_id: subId,
    name: "Hotfix",
    description: "Hotfix for Linux — 1 computer",
    callback_url: `${url.origin}/license/success?fp=${fp}`,
    redirect: true,
    theme: { color: "#f26b1d" },
  };
  // JSON in a <script>: escape "<" so no value can close the tag.
  const json = JSON.stringify(options).replace(/</g, "\\u003c");
  return html(
    "Subscribe",
    `<h1>Subscribe to Hotfix</h1>
<p>${esc(note)}</p>
<button id="pay">Continue to payment</button>
<script src="https://checkout.razorpay.com/v1/checkout.js"></script>
<script>
  const options = ${json};
  options.modal = { ondismiss: () => { location.href = "/license/cancelled"; } };
  const open = () => new Razorpay(options).open();
  document.getElementById("pay").onclick = open;
  open();
</script>`,
  );
}

function managePage(url, fp, next) {
  return html(
    "Manage subscription",
    `<h1>Your Hotfix subscription</h1>
<p>Hotfix is unlocked on this computer. ${esc(next)}</p>
<form method="post" action="${esc(`${url.origin}/license/cancel?fp=${fp}`)}"
      onsubmit="return confirm('Cancel your Hotfix subscription?')">
  <button class="quiet" type="submit">Cancel subscription</button>
</form>`,
  );
}
