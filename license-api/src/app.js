// Hotfix Server licensing API: license.buildcraft.town/api/server/*
//
// A Node service on Railway (src/server.js) backed by PostgreSQL (src/db.js).
// The handler is fetch-style (Request in, Response out).
//
// Model: $1 per server per month on Dodo Payments subscriptions (quantity = seats).
// A purchase creates a LICENSE with two keys, shown once:
//   - an ACTIVATION key (HFXS-…), deployed to servers: `hotfixd license activate <key>`
//   - a MANAGE key (HFXM-…), for billing: seats, add servers, cancel.
// Only SHA-256 hashes of keys are stored. Each server holds a SEAT: a lease
// renewed by every refresh (every few hours), so seats of servers that are gone
// free themselves. A server without a key gets a 14-day TRIAL, per machine.
//
// Machine API (JSON, called by hotfixd):
//   POST /api/server/trial       {fingerprint}                      -> {token}
//   POST /api/server/activate    {key, fingerprint, instance_id, …} -> {seat_id, seat_secret, token}
//   POST /api/server/refresh     {seat_id, seat_secret, …}          -> {token}
//   POST /api/server/deactivate  {seat_id, seat_secret}             -> {ok}
// Browser pages:
//   GET  /api/server/buy, POST /api/server/checkout, GET /api/server/success?ref=
//   GET|POST /api/server/manage, GET|POST /api/server/recover
// Dodo Payments:
//   POST /api/server/webhook
//
// Tokens: base64url(JSON claims) "." base64url(Ed25519 signature), as for the
// earlier desktop licensing; claims carry product "server". Verified by
// server/license.go with the public key compiled into hotfixd.
//
// env: DB and KV (src/db.js), plus the environment variables
//   SERVER_LICENSE_SIGNING_KEY,
//   DODO_PAYMENTS_API_KEY, DODO_PAYMENTS_WEBHOOK_KEY, DODO_SERVER_PRODUCT_ID,
//   DODO_PAYMENTS_ENVIRONMENT (test_mode | live_mode) (all four, or none:
//   billing off, see BILLING_ROUTES),
//   optional TRIAL_DAYS (14), TRIAL_IP_DAILY (25), GITHUB_TOKEN (latest-version lookup).

const PRODUCT = "server";
const DAY = 86400;
const TOKEN_TTL = 7 * DAY; // offline grace for a licensed server
const LEASE = 8 * DAY; // a seat outlives its token by a day
const DEFAULT_TRIAL_DAYS = 14;
const DEFAULT_TRIAL_IP_DAILY = 25;
const MAX_QUANTITY = 1000;
// Statuses that keep seats. "past_due" = a renewal failed and Dodo's grace
// period is running; "on_hold" (after it) does not keep seats.
const LIVE = new Set(["active", "past_due"]);
const FP_RE = /^[0-9a-f]{64}$/;
const ID_RE = /^[0-9a-f]{32}$/;
const REF_RE = /^[0-9a-f]{32}$/;
const SUB_RE = /^sub_[A-Za-z0-9]+$/;
const PAY_RE = /^pay_[A-Za-z0-9]+$/;
const DODO_API = { test_mode: "https://test.dodopayments.com", live_mode: "https://live.dodopayments.com" };
const WEBHOOK_TOLERANCE = 300; // seconds, as the Standard Webhooks libraries
const REPO = "BuildCraftLabsLLP/hotfix";

// Dodo Payments is configured all or nothing (src/server.js refuses a partial
// setup). Without it billing is off: trials and the machine API work as usual,
// the buy page says purchases open soon, and these routes answer 503.
export const BILLING_VARS = ["DODO_PAYMENTS_API_KEY", "DODO_PAYMENTS_WEBHOOK_KEY", "DODO_SERVER_PRODUCT_ID", "DODO_PAYMENTS_ENVIRONMENT"];
export const DODO_ENVIRONMENTS = Object.keys(DODO_API);
const billingEnabled = (env) => BILLING_VARS.every((k) => env[k]) && Object.hasOwn(DODO_API, env.DODO_PAYMENTS_ENVIRONMENT);
const BILLING_ROUTES = new Set(["checkout", "success", "manage", "recover", "webhook"]);

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    const seg = url.pathname.replace(/^\/api\/server\/?/, "").replace(/\/$/, "");
    const route = `${request.method} ${seg}`;
    try {
      if (BILLING_ROUTES.has(seg) && !billingEnabled(env)) {
        return seg === "webhook" ? text("Billing is not enabled", 503) : billingOffPage();
      }
      switch (route) {
        case "POST trial":
          return await trial(request, env);
        case "POST activate":
          return await activate(request, env);
        case "POST refresh":
          return await refresh(request, env);
        case "POST deactivate":
          return await deactivate(request, env);
        case "GET buy":
          return billingEnabled(env) ? buyPage(url) : billingOffPage(200);
        case "POST checkout":
          return await checkout(request, env, url);
        case "GET success":
          return await success(env, url);
        case "GET manage":
          return manageLoginPage(url);
        case "POST manage":
          return await manage(request, env, url);
        case "GET recover":
          return recoverPage(url);
        case "POST recover":
          return await recover(request, env, url);
        case "POST webhook":
          return await webhook(request, env);
        case "GET cancelled":
          return page("Checkout cancelled", "<p>No payment was taken.</p>");
      }
      const known = ["trial", "activate", "refresh", "deactivate", "buy", "checkout", "success", "manage", "recover", "webhook", "cancelled"];
      return text(known.includes(seg) ? "Method not allowed" : "Not found", known.includes(seg) ? 405 : 404);
    } catch (err) {
      console.error("server-api:", err && err.stack ? err.stack : err);
      return seg.match(/^(trial|activate|refresh|deactivate)$/)
        ? json({ error: "server_error", message: "License service error; try again later." }, 500)
        : text("License service error", 500);
    }
  },
};

// ---------------------------------------------------------------------------
// Machine API

async function trial(request, env) {
  const body = await readJSON(request);
  const fp = body && body.fingerprint;
  if (!FP_RE.test(fp || "")) return json({ error: "bad_request", message: "Invalid fingerprint." }, 400);

  const now = nowSec();
  const key = `trial:${fp}`;
  let rec = await env.KV.get(key, "json");
  if (!rec) {
    // New trials per network per day are capped, so short-lived machines
    // (CI runners, spot fleets) can't run forever on fresh trials.
    const capKey = `trialnet:${networkOf(request)}:${new Date(now * 1000).toISOString().slice(0, 10)}`;
    if ((await env.KV.incr(capKey, 2 * DAY)) > intVar(env.TRIAL_IP_DAILY, DEFAULT_TRIAL_IP_DAILY)) {
      return json({ error: "trial_limit", message: "Too many new trials from this network today. Buy a license at https://hotfix.buildcraft.town/server.html" }, 429);
    }
    rec = await env.KV.putIfAbsent(key, { start: now }); // never reset
  }
  const trialEnd = rec.start + intVar(env.TRIAL_DAYS, DEFAULT_TRIAL_DAYS) * DAY;
  const active = now < trialEnd;
  const claims = {
    product: PRODUCT,
    fp,
    status: active ? "active" : "inactive",
    plan: active ? "trial" : "trial_expired",
    iat: now,
    exp: active ? Math.min(now + TOKEN_TTL, trialEnd) : now + TOKEN_TTL,
    trial_end: trialEnd,
    latest_version: await latestVersion(env),
  };
  return json({ token: await signToken(env, claims) });
}

async function activate(request, env) {
  if (await rateLimited(env, request, "activate", 60)) return json({ error: "rate_limited", message: "Too many requests; slow down." }, 429);
  const b = await readJSON(request);
  const machine = machineFields(b);
  if (!machine) return json({ error: "bad_request", message: "Missing or invalid machine details." }, 400);
  const key = normalizeKey(b.key, "HFXS");
  if (!key) return json({ error: "invalid_key", message: "That isn't a valid activation key (HFXS-…). Check for typos." }, 400);

  const keyHash = await sha256hex(key);
  const secret = randomHex(32);
  const secretHash = await sha256hex(secret);
  const now = nowSec();

  // One transaction with the license row locked (FOR UPDATE), so concurrent
  // activations on a license are counted one after another and can't oversell.
  const result = await env.DB.transaction(async (tx) => {
    const lic = await tx.prepare("SELECT id FROM licenses WHERE activation_hash = ? FOR UPDATE").bind(keyHash).first();
    if (!lic) return { error: json({ error: "invalid_key", message: "Unknown activation key." }, 403) };
    const { seats } = await licenseSeats({ DB: tx }, lic.id, now);
    if (seats === 0) return { error: json({ error: "inactive", message: "This license has no active subscription." }, 403) };

    // Same server again (reinstall, re-activation): reuse its seat.
    const existing = await tx.prepare("SELECT id FROM seats WHERE license_id = ? AND instance_id = ?").bind(lic.id, machine.instance_id).first();
    if (existing) {
      await tx
        .prepare("UPDATE seats SET secret_hash=?, fingerprint=?, hostname=?, os=?, version=?, boot_id=?, last_seen=?, lease_until=? WHERE id=?")
        .bind(secretHash, machine.fingerprint, machine.hostname, machine.os, machine.version, machine.boot_id, now, now + LEASE, existing.id)
        .run();
      return { licenseId: lic.id, seatId: existing.id };
    }
    const used = await countSeats({ DB: tx }, lic.id, now);
    if (used >= allowedSeats(seats)) {
      return {
        error: json({
          error: "no_seats",
          message: `All ${seats} server${seats === 1 ? "" : "s"} on this license are in use (${used} active). Add servers, or release one, at https://license.buildcraft.town/api/server/manage`,
          seats,
          used,
        }, 409),
      };
    }
    const seatId = "seat_" + randomHex(12);
    await tx
      .prepare(
        `INSERT INTO seats (id, license_id, secret_hash, fingerprint, instance_id, hostname, os, version, boot_id, created, last_seen, lease_until)
         VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
      )
      .bind(seatId, lic.id, secretHash, machine.fingerprint, machine.instance_id, machine.hostname, machine.os, machine.version, machine.boot_id, now, now, now + LEASE)
      .run();
    // Housekeeping: drop seats whose lease ended long ago.
    await tx.prepare("DELETE FROM seats WHERE license_id = ? AND lease_until < ?").bind(lic.id, now - 30 * DAY).run();
    return { licenseId: lic.id, seatId };
  });
  if (result.error) return result.error;

  const token = await seatToken(env, { seatId: result.seatId, licenseId: result.licenseId, fp: machine.fingerprint, live: true, now });
  return json({ seat_id: result.seatId, seat_secret: secret, token });
}

async function refresh(request, env) {
  const b = await readJSON(request);
  const machine = machineFields(b);
  if (!machine || !/^seat_[0-9a-f]{24}$/.test(b.seat_id || "") || !/^[0-9a-f]{64}$/.test(b.seat_secret || "")) {
    return json({ error: "bad_request", message: "Missing or invalid seat details." }, 400);
  }
  const db = env.DB;
  const seat = await db.prepare("SELECT * FROM seats WHERE id = ?").bind(b.seat_id).first();
  if (!seat || !timingSafeEqual(seat.secret_hash, await sha256hex(b.seat_secret))) {
    // Released, expired and cleaned up, or taken over by a clone with the same identity.
    return json({ error: "seat_gone", message: "This server no longer holds a seat. Activate it again." }, 410);
  }
  const now = nowSec();
  const dupe = seat.instance_id !== machine.instance_id || (seat.hostname && seat.hostname !== machine.hostname && now - seat.last_seen < 3600);
  await db
    .prepare("UPDATE seats SET fingerprint=?, hostname=?, os=?, version=?, boot_id=?, last_seen=?, lease_until=? WHERE id=?")
    .bind(machine.fingerprint, machine.hostname, machine.os, machine.version, machine.boot_id, now, now + LEASE, seat.id)
    .run();
  const { seats } = await licenseSeats(env, seat.license_id, now);
  const token = await seatToken(env, { seatId: seat.id, licenseId: seat.license_id, fp: machine.fingerprint, live: seats > 0, now });
  return json({ token, duplicate_identity: dupe || undefined });
}

async function deactivate(request, env) {
  const b = await readJSON(request);
  if (!/^seat_[0-9a-f]{24}$/.test((b && b.seat_id) || "") || !/^[0-9a-f]{64}$/.test(b.seat_secret || "")) {
    return json({ error: "bad_request", message: "Missing seat details." }, 400);
  }
  const seat = await env.DB.prepare("SELECT id, secret_hash FROM seats WHERE id = ?").bind(b.seat_id).first();
  if (seat && timingSafeEqual(seat.secret_hash, await sha256hex(b.seat_secret))) {
    await env.DB.prepare("DELETE FROM seats WHERE id = ?").bind(seat.id).run();
  }
  return json({ ok: true });
}

function machineFields(b) {
  if (!b || typeof b !== "object") return null;
  const clean = (s, max) => (typeof s === "string" ? s.replace(/[^\x20-\x7e]/g, "").slice(0, max) : "");
  if (!FP_RE.test(b.fingerprint || "") || !ID_RE.test(b.instance_id || "")) return null;
  return {
    fingerprint: b.fingerprint,
    instance_id: b.instance_id,
    hostname: clean(b.hostname, 255),
    os: clean(b.os, 40),
    version: clean(b.version, 40),
    boot_id: clean(b.boot_id, 64),
  };
}

async function seatToken(env, { seatId, licenseId, fp, live, now }) {
  return signToken(env, {
    product: PRODUCT,
    fp,
    seat_id: seatId,
    license_id: licenseId,
    status: live ? "active" : "inactive",
    plan: live ? "subscription" : "lapsed",
    iat: now,
    exp: now + TOKEN_TTL,
    latest_version: await latestVersion(env),
  });
}

// A little headroom so swapping a server doesn't hard-fail while the old
// server's lease runs out: N seats allow N + max(1, N/10) activations.
function allowedSeats(seats) {
  return seats > 0 ? seats + Math.max(1, Math.floor(seats / 10)) : 0;
}

async function licenseSeats(env, licenseId, now) {
  const rows = await env.DB.prepare("SELECT * FROM subscriptions WHERE license_id = ? ORDER BY created").bind(licenseId).all();
  let seats = 0;
  for (const r of rows.results || []) {
    // A cancelled subscription keeps its seats until the paid period ends.
    if (LIVE.has(r.status) || (r.status === "cancelled" && (r.current_end || 0) > now)) seats += r.quantity;
  }
  return { seats, subscriptions: rows.results || [] };
}

async function countSeats(env, licenseId, now) {
  const r = await env.DB.prepare("SELECT COUNT(*) AS n FROM seats WHERE license_id = ? AND lease_until > ?").bind(licenseId, now).first();
  return r ? Number(r.n) : 0;
}

// ---------------------------------------------------------------------------
// Buying

async function checkout(request, env, url) {
  const form = await request.formData();
  const quantity = parseInt(String(form.get("quantity") || "1"), 10);
  if (!Number.isInteger(quantity) || quantity < 1 || quantity > MAX_QUANTITY) {
    return page("Choose a number of servers", `<p>Pick between 1 and ${MAX_QUANTITY} servers.</p><p><a href="${url.origin}/api/server/buy">Back</a></p>`);
  }
  // Optional: add the servers to an existing license.
  let pendingLicense = null;
  const manageKey = String(form.get("manage_key") || "").trim();
  if (manageKey) {
    const lic = await licenseByManageKey(env, manageKey);
    if (!lic) return page("Unknown manage key", `<p>That manage key wasn't recognized.</p><p><a href="${url.origin}/api/server/buy">Back</a></p>`);
    pendingLicense = lic.id;
  }

  // A Dodo subscription only exists once the checkout is paid, so the checkout
  // is recorded under a random ref, which comes back in the return URL and in
  // the session's metadata (see checkoutOf).
  const ref = randomHex(16);
  const session = await dodo(env, "POST", "/checkouts", {
    product_cart: [{ product_id: env.DODO_SERVER_PRODUCT_ID, quantity }],
    return_url: `${url.origin}/api/server/success?ref=${ref}`,
    cancel_url: `${url.origin}/api/server/cancelled`,
    metadata: { product: PRODUCT, ref },
    feature_flags: { redirect_immediately: true },
  });
  const now = nowSec();
  await env.DB
    .prepare("INSERT INTO checkouts (ref, session_id, pending_license_id, quantity, created) VALUES (?,?,?,?,?)")
    .bind(ref, session.session_id, pendingLicense, quantity, now)
    .run();
  // Housekeeping: checkouts that were never paid.
  await env.DB.prepare("DELETE FROM checkouts WHERE subscription_id IS NULL AND created < ?").bind(now - 7 * DAY).run();
  return redirect(session.checkout_url);
}

// Checkout's return URL: /success?ref=…, to which Dodo appends
// subscription_id and status. Those are unsigned, so the subscription is
// fetched from Dodo and must belong to this browser's checkout (ref). Keys are
// created here, once per subscription, and shown once: if the page never
// loads, the buyer recovers them with the payment id from Dodo's receipt.
async function success(env, url) {
  const ref = url.searchParams.get("ref") || "";
  const subId = url.searchParams.get("subscription_id") || "";
  if (!REF_RE.test(ref)) return text("Invalid checkout", 400);
  if (!SUB_RE.test(subId)) {
    return page("Payment not completed", "<p>The payment didn't go through, so nothing was charged or activated.</p>");
  }
  const sub = await found(dodo(env, "GET", `/subscriptions/${subId}`));
  if (!sub || sub.product_id !== env.DODO_SERVER_PRODUCT_ID) return text("Unknown subscription", 400);
  const row = await subscriptionRow(env, sub);
  const co = await env.DB.prepare("SELECT subscription_id FROM checkouts WHERE ref = ?").bind(ref).first();
  // Once a subscription has a row, it's matched to its checkout (or none).
  if (!co || (row && co.subscription_id !== subId)) return text("This payment doesn't belong to this checkout", 400);
  // Not matched to its checkout yet (Dodo hasn't listed the payment), or
  // still being confirmed (e.g. Indian cards' e-mandates take a few minutes).
  if (!row || sub.status === "pending") {
    return page(
      "Payment processing",
      `<h1>Payment processing</h1><p>Your payment is being confirmed. This page refreshes by itself; it usually takes a minute or two.</p>
<meta http-equiv="refresh" content="10">`,
    );
  }
  await syncSubscription(env, sub);
  if (!LIVE.has(sub.status)) {
    return page("Payment not completed", "<p>The payment didn't go through, so nothing was activated.</p>");
  }
  return await linkOrIssue(env, url, subId, row.pending_license_id, sub.quantity);
}

// The local row for a Dodo subscription of the Hotfix Server product, created
// the first time we see it (return URL, webhook or recovery) from the checkout
// that started it. null for other products, and (unless orphan, for recovery,
// where the buyer has proven the payment) while its checkout can't be found.
async function subscriptionRow(env, sub, { orphan = false } = {}) {
  const db = env.DB;
  const id = sub.subscription_id;
  const row = await db.prepare("SELECT * FROM subscriptions WHERE id = ?").bind(id).first();
  if (row || sub.product_id !== env.DODO_SERVER_PRODUCT_ID) return row;
  const co = await checkoutOf(env, sub);
  if (!co && !orphan) return null;
  const now = nowSec();
  await db
    .prepare(
      `INSERT INTO subscriptions (id, pending_license_id, status, quantity, customer_id, created, updated)
       VALUES (?,?,?,?,?,?,?) ON CONFLICT (id) DO NOTHING`,
    )
    .bind(id, co ? co.pending_license_id : null, sub.status, sub.quantity || 1, (sub.customer && sub.customer.customer_id) || null, now, now)
    .run();
  if (co) await db.prepare("UPDATE checkouts SET subscription_id = ? WHERE ref = ? AND subscription_id IS NULL").bind(id, co.ref).run();
  return db.prepare("SELECT * FROM subscriptions WHERE id = ?").bind(id).first();
}

// The checkout that started a subscription: by the ref in its metadata if
// Dodo carried it over from the session, else by the checkout session of one
// of its payments.
async function checkoutOf(env, sub) {
  const db = env.DB;
  const usable = (co) => co && (!co.subscription_id || co.subscription_id === sub.subscription_id);
  const ref = sub.metadata && sub.metadata.ref;
  if (typeof ref === "string" && REF_RE.test(ref)) {
    const co = await db.prepare("SELECT * FROM checkouts WHERE ref = ?").bind(ref).first();
    if (usable(co)) return co;
  }
  const payments = await dodo(env, "GET", `/payments?subscription_id=${encodeURIComponent(sub.subscription_id)}&page_size=100`);
  for (const p of payments.items || []) {
    if (!p.checkout_session_id) continue;
    const co = await db.prepare("SELECT * FROM checkouts WHERE session_id = ?").bind(p.checkout_session_id).first();
    if (usable(co)) return co;
  }
  return null;
}

// Attach a paid subscription to its license: an existing one ("add servers"),
// or a new license with fresh keys. Idempotent: only the first call for a
// subscription links it (and so only it shows the new keys).
async function linkOrIssue(env, url, subId, pendingLicense, quantity) {
  const db = env.DB;
  const servers = `${quantity} server${quantity === 1 ? "" : "s"}`;
  if (pendingLicense) {
    // The webhook may have linked it already; that's fine, no keys are shown.
    await linkPending(env, subId, pendingLicense);
    const r = await db.prepare("SELECT license_id FROM subscriptions WHERE id = ?").bind(subId).first();
    if (!r || r.license_id !== pendingLicense) return alreadyIssued(url);
    return page("Servers added", `<h1>Done: ${servers} added</h1><p>Your existing activation key now covers them. Activate each new server with <code>hotfixd license activate &lt;key&gt;</code>.</p>`);
  }

  const keys = { activation: makeKey("HFXS"), manage: makeKey("HFXM") };
  const licId = "lic_" + randomHex(12);
  await db
    .prepare("INSERT INTO licenses (id, activation_hash, manage_hash, created) VALUES (?,?,?,?)")
    .bind(licId, await sha256hex(normalizeKey(keys.activation, "HFXS")), await sha256hex(normalizeKey(keys.manage, "HFXM")), nowSec())
    .run();
  const res = await db.prepare("UPDATE subscriptions SET license_id = ? WHERE id = ? AND license_id IS NULL").bind(licId, subId).run();
  if (res.meta.changes !== 1) {
    await db.prepare("DELETE FROM licenses WHERE id = ?").bind(licId).run();
    return alreadyIssued(url);
  }
  return keysPage(url, keys, `You're subscribed for ${servers}`);
}

async function linkPending(env, subId, pendingLicense) {
  await env.DB.prepare("UPDATE subscriptions SET license_id = ? WHERE id = ? AND license_id IS NULL").bind(pendingLicense, subId).run();
}

function alreadyIssued(url) {
  return page(
    "Already done",
    `<h1>This payment was already processed</h1>
<p>Keys are shown only once. If you didn't save them, <a href="${url.origin}/api/server/recover">get new keys</a> with the payment id from your Dodo Payments receipt email.</p>`,
  );
}

// ---------------------------------------------------------------------------
// Managing

async function manage(request, env, url) {
  if (await rateLimited(env, request, "manage", 30)) return page("Slow down", "<p>Too many attempts. Try again in a minute.</p>");
  const form = await request.formData();
  const manageKey = String(form.get("manage_key") || "");
  const lic = await licenseByManageKey(env, manageKey);
  if (!lic) return manageLoginPage(url, "That manage key wasn't recognized.");

  const now = nowSec();
  const db = env.DB;
  let notice = "";
  const action = String(form.get("action") || "");
  const target = String(form.get("target") || "");

  if (action === "release" && /^seat_[0-9a-f]{24}$/.test(target)) {
    await db.prepare("DELETE FROM seats WHERE id = ? AND license_id = ?").bind(target, lic.id).run();
    notice = "Server released. Its seat is free now; that server stops being protected at its next license check.";
  } else if (["quantity", "cancel", "billing"].includes(action) && SUB_RE.test(target)) {
    const row = await db.prepare("SELECT * FROM subscriptions WHERE id = ? AND license_id = ?").bind(target, lic.id).first();
    if (!row) {
      notice = "Unknown subscription.";
    } else if (action === "billing") {
      // Dodo's customer portal: invoices, and the payment method (which
      // reactivates a subscription on hold).
      if (!row.customer_id) {
        notice = "Billing details aren't available for this subscription.";
      } else {
        const back = encodeURIComponent(`${url.origin}/api/server/manage`);
        const portal = await dodo(env, "POST", `/customers/${encodeURIComponent(row.customer_id)}/customer-portal/session?return_url=${back}`);
        return redirect(portal.link);
      }
    } else if (action === "cancel") {
      const sub = await dodo(env, "PATCH", `/subscriptions/${target}`, { cancel_at_next_billing_date: true });
      await syncSubscription(env, sub);
      notice = `Cancelled. Its servers stay covered until ${formatDate(unixOf(sub.next_billing_date) || row.current_end)}; no further charges.`;
    } else {
      notice = await changeQuantity(env, row, parseInt(String(form.get("quantity") || ""), 10));
    }
  }
  return managePage(env, url, manageKey, lic.id, now, notice);
}

// Seat increases apply at once, charging the prorated difference for the rest
// of the cycle; if Dodo refuses that charge (e.g. below its minimum), the
// change is applied without a charge now and billed from the next renewal.
// Decreases apply at the next billing date.
async function changeQuantity(env, row, quantity) {
  if (!Number.isInteger(quantity) || quantity < 1 || quantity > MAX_QUANTITY) return "Pick between 1 and 1000 servers.";
  if (quantity === row.quantity) return "No change.";
  const change = (proration_billing_mode, effective_at, extra = {}) =>
    dodo(env, "POST", `/subscriptions/${row.id}/change-plan`, { product_id: env.DODO_SERVER_PRODUCT_ID, quantity, proration_billing_mode, effective_at, ...extra });
  try {
    if (quantity > row.quantity) {
      try {
        // A failed charge must not put the whole subscription on hold.
        await change("prorated_immediately_keep_cycle", "immediately", { on_payment_failure: "prevent_change" });
      } catch {
        await change("do_not_bill", "immediately");
      }
      await env.DB.prepare("UPDATE subscriptions SET quantity = ?, updated = ? WHERE id = ?").bind(quantity, nowSec(), row.id).run();
      return `Now ${quantity} servers. The extra servers can be activated right away.`;
    }
    await change("do_not_bill", "next_billing_date");
    return `Changes to ${quantity} servers at the next billing date (${formatDate(row.current_end)}).`;
  } catch {
    return "This subscription can't be changed right now. To add servers, buy more below; to reduce, cancel it and buy a smaller one.";
  }
}

async function licenseByManageKey(env, key) {
  const k = normalizeKey(key, "HFXM");
  if (!k) return null;
  return env.DB.prepare("SELECT id FROM licenses WHERE manage_hash = ?").bind(await sha256hex(k)).first();
}

// ---------------------------------------------------------------------------
// Recovery: new keys, proven by a payment id from Dodo's receipt email.
// Running servers keep working (they refresh with their seat secret).

async function recover(request, env, url) {
  if (await rateLimited(env, request, "recover", 10)) return page("Slow down", "<p>Too many attempts. Try again in a minute.</p>");
  const form = await request.formData();
  const paymentId = String(form.get("payment_id") || "").trim();
  if (!PAY_RE.test(paymentId)) return recoverPage(url, "Enter the payment id exactly as it appears in the receipt (pay_…).");

  const payment = await found(dodo(env, "GET", `/payments/${paymentId}`));
  const subId = payment && payment.status === "succeeded" ? payment.subscription_id : null;
  const sub = SUB_RE.test(subId || "") ? await found(dodo(env, "GET", `/subscriptions/${subId}`)) : null;
  const row = sub && (await subscriptionRow(env, sub, { orphan: true }));
  if (!row) return recoverPage(url, "That isn't a paid Hotfix Server payment.");

  if (!row.license_id) {
    await syncSubscription(env, sub);
    if (!LIVE.has(sub.status)) return recoverPage(url, "That subscription isn't active.");
    return await linkOrIssue(env, url, subId, row.pending_license_id, sub.quantity);
  }
  const keys = { activation: makeKey("HFXS"), manage: makeKey("HFXM") };
  await env.DB
    .prepare("UPDATE licenses SET activation_hash = ?, manage_hash = ? WHERE id = ?")
    .bind(await sha256hex(normalizeKey(keys.activation, "HFXS")), await sha256hex(normalizeKey(keys.manage, "HFXM")), row.license_id)
    .run();
  return keysPage(url, keys, "New keys issued", "Your old keys no longer work. Servers that are already activated keep working without changes.");
}

// ---------------------------------------------------------------------------
// Webhook

async function webhook(request, env) {
  const body = await request.text();
  if (!(await webhookSignatureValid(env.DODO_PAYMENTS_WEBHOOK_KEY, request.headers, body))) {
    return text("Bad signature", 400);
  }
  const event = JSON.parse(body);
  const subId = event.data && event.data.subscription_id;
  if (typeof event.type === "string" && event.type.startsWith("subscription.") && SUB_RE.test(subId || "")) {
    // Re-fetch rather than trust the event: replays and out-of-order delivery
    // then can't store a stale state.
    const sub = await found(dodo(env, "GET", `/subscriptions/${subId}`));
    const row = sub && (await subscriptionRow(env, sub));
    if (row) {
      await syncSubscription(env, sub);
      // "Add servers" purchases show no keys, so they needn't wait for the return page.
      if (!row.license_id && row.pending_license_id && LIVE.has(sub.status)) await linkPending(env, row.id, row.pending_license_id);
    }
  }
  return text("ok", 200);
}

// Standard Webhooks (https://standardwebhooks.com), as Dodo signs them:
// base64 HMAC-SHA256 of "<webhook-id>.<webhook-timestamp>.<body>" with the
// base64-decoded secret (after "whsec_"), in "v1,<sig>" entries.
export async function webhookSignatureValid(secret, headers, body) {
  const id = headers.get("webhook-id");
  const ts = headers.get("webhook-timestamp") || "";
  const sigs = headers.get("webhook-signature");
  if (!secret || !id || !sigs || !/^\d+$/.test(ts) || Math.abs(nowSec() - Number(ts)) > WEBHOOK_TOLERANCE) return false;
  let key;
  try {
    key = await crypto.subtle.importKey("raw", b64decode(secret.replace(/^whsec_/, "")), { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  } catch {
    return false;
  }
  const mac = b64(new Uint8Array(await crypto.subtle.sign("HMAC", key, new TextEncoder().encode(`${id}.${ts}.${body}`))));
  return sigs.split(" ").some((s) => s.startsWith("v1,") && timingSafeEqual(s.slice(3), mac));
}

async function syncSubscription(env, sub) {
  const quantity = sub.quantity || 1;
  await env.DB
    .prepare(
      `UPDATE subscriptions SET status = ?, current_end = COALESCE(?::bigint, current_end),
       quantity = CASE WHEN ?::integer > quantity OR ?::integer = 0 THEN ?::integer ELSE quantity END,
       cancel_scheduled = ?, customer_id = COALESCE(?, customer_id), updated = ? WHERE id = ?`,
    )
    .bind(
      sub.status,
      unixOf(sub.next_billing_date),
      // Dodo's quantity drops only when a scheduled decrease takes effect (at
      // the billing date), so syncing it down is only safe once the
      // subscription has no scheduled change pending.
      quantity,
      sub.scheduled_change ? 1 : 0,
      quantity,
      sub.cancel_at_next_billing_date ? 1 : 0,
      (sub.customer && sub.customer.customer_id) || null,
      nowSec(),
      sub.subscription_id,
    )
    .run();
}

// ---------------------------------------------------------------------------
// Keys

const ALPHABET = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"; // Crockford base32

// makeKey returns PREFIX-XXXXX-XXXXX-XXXXX-XXXXX: 19 random characters (95
// bits) and a check character, so typos are caught before any lookup.
function makeKey(prefix) {
  const bytes = crypto.getRandomValues(new Uint8Array(19));
  const body = Array.from(bytes, (b) => ALPHABET[b & 31]).join("");
  const full = body + checkChar(prefix + body);
  return `${prefix}-${full.match(/.{5}/g).join("-")}`;
}

function checkChar(s) {
  let sum = 0;
  for (let i = 0; i < s.length; i++) sum = (sum * 31 + s.charCodeAt(i)) % 1000003;
  return ALPHABET[sum % 32];
}

// normalizeKey canonicalizes a typed key (case, dashes, spaces, Crockford
// look-alikes) and returns PREFIX+20 chars, or null if malformed.
export function normalizeKey(input, prefix) {
  if (typeof input !== "string") return null;
  let s = input.toUpperCase().replace(/[\s-]/g, "");
  if (!s.startsWith(prefix)) return null;
  s = s.slice(prefix.length).replace(/[IL]/g, "1").replace(/O/g, "0");
  if (!/^[0-9A-HJKMNP-TV-Z]{20}$/.test(s)) return null;
  if (checkChar(prefix + s.slice(0, 19)) !== s[19]) return null;
  return prefix + s;
}

export { makeKey };

// ---------------------------------------------------------------------------
// Pages

// Same look as the product site (docs/server.html): cream paper, Fraunces
// headings, Manrope text, burnt-orange accent.
const STYLE = `
  *, *::before, *::after { box-sizing: border-box; margin: 0; padding: 0; }
  :root { --bg: #F6F4F0; --paper: #FBFAF7; --card: #EDEAE5; --dark: #141416; --accent: #C9461E;
          --border: rgba(20,20,22,.10); --muted: rgba(20,20,22,.58);
          --ok: #2F7A4B; --warn: #9A5B0B; --bad: #B3261E; }
  body { background: var(--bg); color: var(--dark); font: 15px/1.55 'Manrope', system-ui, sans-serif;
         -webkit-font-smoothing: antialiased; min-height: 100vh; display: flex; flex-direction: column; }
  a { color: inherit; }
  code { font-family: ui-monospace, 'SF Mono', Menlo, Consolas, monospace; font-size: .9em; }
  header { display: flex; align-items: center; justify-content: space-between; height: 60px; padding: 0 32px;
           border-bottom: 1px solid var(--border); background: rgba(246,244,240,.92); }
  .logo { display: flex; align-items: center; gap: 10px; font-weight: 700; text-decoration: none; }
  header nav { display: flex; gap: 22px; font-size: 13px; font-weight: 600; }
  header nav a { text-decoration: none; color: var(--muted); }
  header nav a:hover { color: var(--dark); }
  main { flex: 1; width: 100%; max-width: 560px; margin: 0 auto; padding: 56px 20px 72px; }
  main.wide { max-width: 760px; }
  .eyebrow { font-size: 11px; font-weight: 700; letter-spacing: 1.4px; text-transform: uppercase; color: var(--accent); margin-bottom: 10px; }
  h1 { font-family: 'Fraunces', Georgia, serif; font-weight: 400; font-size: clamp(30px, 5vw, 42px);
       line-height: 1.1; letter-spacing: -.5px; margin-bottom: 14px; }
  h2 { font-size: 12px; font-weight: 700; letter-spacing: 1.2px; text-transform: uppercase; color: var(--muted); margin: 40px 0 12px; }
  p { color: var(--muted); margin-bottom: 12px; }
  p strong { color: var(--dark); }
  .lead { font-size: 16px; margin-bottom: 28px; }
  .small { font-size: 13px; color: var(--muted); }
  .fine { font-size: 13px; margin-top: 22px; }
  .fine a, .lead a, p a { text-decoration: underline; text-underline-offset: 2px; }
  .card { background: var(--paper); border: 1px solid var(--border); border-radius: 16px; padding: 26px;
          box-shadow: 0 1px 2px rgba(20,20,22,.04); }
  .card + .card { margin-top: 14px; }
  .card.flush { padding: 0; overflow-x: auto; }
  .notice { background: rgba(201,70,30,.08); border: 1px solid rgba(201,70,30,.22); color: var(--dark);
            border-radius: 12px; padding: 12px 16px; margin-bottom: 22px; font-size: 14px; }
  label.field { display: block; font-size: 13px; font-weight: 700; margin-bottom: 6px; }
  input { font: inherit; width: 100%; padding: 11px 13px; border: 1px solid rgba(20,20,22,.16); border-radius: 10px;
          background: #fff; color: var(--dark); }
  input:focus { outline: 3px solid rgba(201,70,30,.18); border-color: var(--accent); }
  .btn { display: inline-flex; align-items: center; justify-content: center; gap: 8px; font: inherit; font-size: 14px;
         font-weight: 700; padding: 12px 22px; border-radius: 10px; border: 0; cursor: pointer; text-decoration: none;
         background: var(--accent); color: #fff; white-space: nowrap; }
  .btn:hover { opacity: .92; }
  .btn.block { width: 100%; margin-top: 18px; }
  .btn.dark { background: var(--dark); }
  .btn.quiet { background: #fff; color: var(--dark); border: 1px solid rgba(20,20,22,.16); padding: 8px 14px; font-size: 13px; }
  .btn.quiet:hover { background: var(--card); opacity: 1; }
  .btn.danger { color: var(--bad); }
  form.inline { display: inline-flex; gap: 8px; align-items: center; }
  /* buy */
  .price { display: flex; align-items: center; gap: 14px; padding-bottom: 22px; margin-bottom: 22px; border-bottom: 1px solid var(--border); }
  .price .amount { font-family: 'Fraunces', Georgia, serif; font-weight: 300; font-size: 64px; line-height: 1; }
  .price .per { font-size: 13px; font-weight: 600; color: var(--muted); line-height: 1.35; }
  .stepper { display: flex; gap: 8px; }
  .stepper input { text-align: center; font-weight: 700; font-size: 18px; -moz-appearance: textfield; }
  .stepper input::-webkit-inner-spin-button { -webkit-appearance: none; }
  .stepper .btn.quiet { width: 48px; font-size: 20px; padding: 0; }
  .total { margin: 12px 0 0; font-size: 14px; }
  details { margin-top: 16px; font-size: 14px; }
  summary { cursor: pointer; color: var(--muted); font-weight: 600; }
  details[open] summary { margin-bottom: 10px; }
  .checks { list-style: none; margin-top: 22px; padding-top: 18px; border-top: 1px solid var(--border); }
  .checks li { position: relative; padding: 5px 0 5px 26px; font-size: 14px; color: var(--muted); }
  .checks li::before { content: "✓"; position: absolute; left: 2px; color: var(--accent); font-weight: 800; }
  /* keys */
  .done { width: 48px; height: 48px; border-radius: 50%; display: grid; place-items: center; margin-bottom: 18px;
          background: rgba(47,122,75,.12); color: var(--ok); font-size: 24px; font-weight: 800; }
  .key-head { display: flex; align-items: flex-start; justify-content: space-between; gap: 12px; margin-bottom: 12px; }
  .key-label { font-weight: 700; }
  .key { font-family: ui-monospace, 'SF Mono', Menlo, Consolas, monospace; font-size: 17px; letter-spacing: .5px;
         background: #fff; border: 1px dashed rgba(20,20,22,.22); border-radius: 10px; padding: 12px 14px;
         user-select: all; word-break: break-all; }
  .cmd { display: flex; align-items: center; justify-content: space-between; gap: 12px; background: var(--dark);
         color: #f3ece6; border-radius: 12px; padding: 12px 12px 12px 16px; }
  .cmd code { white-space: nowrap; overflow-x: auto; min-width: 0; flex: 1; }
  .cmd .btn.quiet { background: rgba(255,255,255,.08); color: #fff; border-color: rgba(255,255,255,.18); }
  /* manage */
  .usage-text { color: var(--dark); font-size: 17px; margin-bottom: 12px; }
  .bar { height: 8px; border-radius: 99px; background: var(--card); overflow: hidden; }
  .bar span { display: block; height: 100%; background: var(--accent); border-radius: 99px; }
  table { border-collapse: collapse; width: 100%; font-size: 14px; }
  th { text-align: left; font-size: 11px; font-weight: 700; letter-spacing: 1px; text-transform: uppercase; color: var(--muted);
       padding: 14px 18px 10px; border-bottom: 1px solid var(--border); }
  td { padding: 12px 18px; border-bottom: 1px solid var(--border); vertical-align: middle; }
  tr:last-child td { border-bottom: 0; }
  td.right { text-align: right; }
  .empty { padding: 26px; }
  .sub-head { display: flex; justify-content: space-between; align-items: flex-start; gap: 12px; }
  .sub-title { font-weight: 700; font-size: 16px; }
  .sub-meta { font-size: 13px; color: var(--muted); margin-top: 2px; }
  .sub-actions { display: flex; flex-wrap: wrap; gap: 8px; margin-top: 16px; }
  .sub-actions input { width: 76px; padding: 7px 10px; text-align: center; }
  .badge { font-size: 12px; font-weight: 700; padding: 4px 10px; border-radius: 99px; white-space: nowrap; }
  .badge.ok { background: rgba(47,122,75,.12); color: var(--ok); }
  .badge.warn { background: rgba(154,91,11,.12); color: var(--warn); }
  .badge.bad { background: rgba(179,38,30,.10); color: var(--bad); }
  .badge.muted { background: var(--card); color: var(--muted); }
  .alert { margin-top: 14px; font-size: 13px; color: var(--bad); }
  .add { display: flex; gap: 10px; align-items: flex-end; flex-wrap: wrap; }
  .add .stepper { flex: 1; min-width: 200px; }
  footer { padding: 26px 20px; text-align: center; font-size: 12px; color: var(--muted); border-top: 1px solid var(--border); }
  footer a { text-decoration: underline; text-underline-offset: 2px; }
  @media (max-width: 600px) { header { padding: 0 16px; } main { padding-top: 36px; } .card { padding: 20px; }
    .price .amount { font-size: 52px; } th:nth-child(2), td:nth-child(2) { display: none; } }`;

// The buy and manage pages' server steppers (with the monthly total), and the
// keys page's copy buttons.
const SCRIPT = `
  document.querySelectorAll(".stepper").forEach((s) => {
    const input = s.querySelector("input"), total = s.closest("form").querySelector("[data-total]");
    const clamp = (n) => Math.min(Number(input.max) || 1000, Math.max(1, n || 1));
    const show = () => { if (total) total.textContent = "$" + clamp(parseInt(input.value, 10)); };
    s.querySelectorAll("[data-step]").forEach((b) => b.addEventListener("click", () => {
      input.value = clamp((parseInt(input.value, 10) || 1) + Number(b.dataset.step)); show();
    }));
    input.addEventListener("input", show); show();
  });
  document.querySelectorAll("[data-copy]").forEach((b) => b.addEventListener("click", async () => {
    try { await navigator.clipboard.writeText(b.dataset.copy); const t = b.textContent; b.textContent = "Copied"; setTimeout(() => (b.textContent = t), 1500); } catch {}
  }));`;

const SITE = "https://hotfix.buildcraft.town/server.html";
const esc = (s) => String(s ?? "").replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");
const plural = (n, word) => `${n} ${word}${n === 1 ? "" : "s"}`;

function page(title, inner, { wide = false } = {}) {
  return new Response(
    `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1"><meta name="robots" content="noindex">
<title>Hotfix Server — ${esc(title)}</title>
<link rel="icon" href="data:image/svg+xml,<svg xmlns='http://www.w3.org/2000/svg' viewBox='0 0 100 100'><text y='.9em' font-size='90'>🔥</text></svg>">
<link rel="preconnect" href="https://fonts.googleapis.com"><link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=Fraunces:opsz,wght@9..144,300;9..144,400&family=Manrope:wght@400;600;700;800&display=swap" rel="stylesheet">
<style>${STYLE}</style></head>
<body>
<header><a class="logo" href="${SITE}"><span>🔥</span><span>Hotfix Server</span></a>
<nav><a href="/api/server/buy">Buy</a><a href="/api/server/manage">Manage license</a></nav></header>
<main${wide ? ' class="wide"' : ""}>${/<h1/.test(inner.slice(0, 300)) ? "" : `<h1>${esc(title)}</h1>`}${inner}</main>
<footer>Hotfix Server by BuildCraft Labs · <a href="${SITE}">Product page</a> · <a href="/api/server/recover">Lost your keys?</a></footer>
<script>${SCRIPT}</script>
</body></html>`,
    { headers: { "Content-Type": "text/html; charset=utf-8", "Cache-Control": "no-store", "X-Frame-Options": "DENY" } },
  );
}

const notice = (text) => (text ? `<p class="notice">${esc(text)}</p>` : "");

function billingOffPage(status = 503) {
  const res = page(
    "Buy Hotfix Server",
    `<p class="eyebrow">Hotfix Server</p>
<h1>Purchases open soon</h1>
<p class="lead"><strong>$1 per server per month.</strong> Until then every server gets a 14-day free trial when you install it, no card needed.</p>
<a class="btn" href="${SITE}#install">Install the free trial</a>`,
  );
  return new Response(res.body, { status, headers: res.headers });
}

function stepper(value = 1) {
  return `<div class="stepper"><button type="button" class="btn quiet" data-step="-1" aria-label="Fewer servers">−</button>
<input id="quantity" name="quantity" type="number" min="1" max="${MAX_QUANTITY}" value="${value}" required inputmode="numeric" aria-label="Servers">
<button type="button" class="btn quiet" data-step="1" aria-label="More servers">+</button></div>`;
}

function buyPage(url) {
  return page(
    "Buy Hotfix Server",
    `<p class="eyebrow">Hotfix Server</p>
<h1>Keep runaway processes off your servers</h1>
<p class="lead"><strong>$1 per server per month.</strong> Every server also gets a 14-day free trial when you install it, no card needed.</p>
<div class="card">
  <div class="price"><span class="amount">$1</span><span class="per">per server<br>per month</span></div>
  <form method="post" action="${url.origin}/api/server/checkout">
    <label class="field" for="quantity">How many servers?</label>
    ${stepper()}
    <p class="total"><strong data-total>$1</strong> per month, plus tax where it applies.</p>
    <details><summary>Adding servers to a license you already have?</summary>
      <label class="field" for="manage_key">Its manage key</label>
      <input id="manage_key" name="manage_key" placeholder="HFXM-…" autocomplete="off" spellcheck="false">
    </details>
    <button class="btn block" type="submit">Continue to payment</button>
  </form>
  <ul class="checks">
    <li>One activation key for all your servers</li>
    <li>Add or remove servers any time</li>
    <li>Cancel any time: servers stay covered until the end of the month you paid for</li>
  </ul>
</div>
<p class="fine">After paying you get an activation key for your servers and a manage key for billing. Payments are handled by Dodo Payments, our merchant of record, who also send your invoices.</p>`,
  );
}

function keysPage(url, keys, heading, extra = "") {
  const file = `Hotfix Server license\n\nActivation key (put on your servers):\n${keys.activation}\n\nManage key (billing, keep private):\n${keys.manage}\n\nManage: ${url.origin}/api/server/manage\n`;
  const command = `sudo hotfixd license activate ${keys.activation}`;
  const keyCard = (label, hint, key) => `<div class="card">
  <div class="key-head"><div><div class="key-label">${label}</div><div class="small">${hint}</div></div>
  <button type="button" class="btn quiet" data-copy="${esc(key)}">Copy</button></div>
  <div class="key">${esc(key)}</div>
</div>`;
  return page(
    heading,
    `<div class="done">✓</div>
<h1>${esc(heading)}</h1>
${notice(extra)}
<p class="notice"><strong>Save these keys now: they are shown only once.</strong> Download them, or copy them somewhere safe.</p>
${keyCard("Activation key", "Put this on your servers.", keys.activation)}
${keyCard("Manage key", "For billing: add servers, cancel. Keep it private.", keys.manage)}
<p style="margin-top:16px"><a class="btn dark" download="hotfix-server-license.txt" href="data:text/plain;charset=utf-8,${encodeURIComponent(file)}">Download as a text file</a></p>
<h2>Activate a server</h2>
<div class="cmd"><code>${esc(command)}</code><button type="button" class="btn quiet" data-copy="${esc(command)}">Copy</button></div>
<p class="fine">On Windows, run it in an administrator prompt, without <code>sudo</code>. Manage your license any time at <a href="${url.origin}/api/server/manage">${url.origin}/api/server/manage</a>.</p>`,
    { wide: true },
  );
}

function manageLoginPage(url, error = "") {
  return page(
    "Manage your license",
    `<p class="eyebrow">Manage</p>
<h1>Your Hotfix Server license</h1>
<p class="lead">Open your license with its manage key to see your servers, change how many you pay for, cancel, or get invoices.</p>
${notice(error)}
<div class="card"><form method="post" action="${url.origin}/api/server/manage">
  <label class="field" for="manage_key">Manage key</label>
  <input id="manage_key" name="manage_key" placeholder="HFXM-…" autocomplete="off" spellcheck="false" required>
  <button class="btn block" type="submit">Open</button>
</form></div>
<p class="fine">Lost your keys? <a href="${url.origin}/api/server/recover">Get new ones</a>.</p>`,
  );
}

function statusBadge(s) {
  if (s.cancel_scheduled && LIVE.has(s.status)) return ["warn", `Cancels ${formatDate(s.current_end)}`];
  const known = { active: ["ok", "Active"], past_due: ["warn", "Payment retrying"], on_hold: ["bad", "On hold"], pending: ["muted", "Pending"], paused: ["muted", "Paused"] };
  return known[s.status] || ["muted", s.status.charAt(0).toUpperCase() + s.status.slice(1)];
}

async function managePage(env, url, manageKey, licenseId, now, message) {
  const { seats, subscriptions } = await licenseSeats(env, licenseId, now);
  const servers = (await env.DB.prepare("SELECT * FROM seats WHERE license_id = ? AND lease_until > ? ORDER BY last_seen DESC").bind(licenseId, now).all()).results || [];
  const mk = esc(manageKey);
  const hidden = (action, target) => `<input type="hidden" name="manage_key" value="${mk}"><input type="hidden" name="action" value="${action}"><input type="hidden" name="target" value="${esc(target)}">`;

  const subCards = subscriptions
    .map((s) => {
      const live = LIVE.has(s.status) && !s.cancel_scheduled;
      const [tone, label] = statusBadge(s);
      const when = s.current_end ? ` · ${live ? "Renews" : "Covered until"} ${formatDate(s.current_end)}` : "";
      return `<div class="card">
<div class="sub-head"><div><div class="sub-title">${plural(s.quantity, "server")} · $${s.quantity}/month</div>
<div class="sub-meta"><code>${esc(s.id)}</code>${when}</div></div><span class="badge ${tone}">${esc(label)}</span></div>
${s.status === "on_hold" ? `<p class="alert">The last payment failed. Update your payment method under Billing to reactivate it.</p>` : ""}
<div class="sub-actions">
${live ? `<form class="inline" method="post">${hidden("quantity", s.id)}<input name="quantity" type="number" min="1" max="${MAX_QUANTITY}" value="${s.quantity}" aria-label="Servers"><button class="btn quiet">Change</button></form>` : ""}
${s.customer_id ? `<form class="inline" method="post">${hidden("billing", s.id)}<button class="btn quiet">Billing</button></form>` : ""}
${live ? `<form class="inline" method="post" onsubmit="return confirm('Cancel this subscription at the end of the paid period?')">${hidden("cancel", s.id)}<button class="btn quiet danger">Cancel</button></form>` : ""}
</div></div>`;
    })
    .join("");
  const serverRows = servers
    .map(
      (s) => `<tr><td><strong>${esc(s.hostname || "?")}</strong><div class="small">${esc(s.os)}</div></td><td>${esc(s.version)}</td><td>${formatDate(s.last_seen)}</td>
<td class="right"><form class="inline" method="post" onsubmit="return confirm('Release this server\\'s seat?')">${hidden("release", s.id)}<button class="btn quiet">Release</button></form></td></tr>`,
    )
    .join("");
  const pct = seats ? Math.min(100, Math.round((servers.length / seats) * 100)) : 0;

  return page(
    "Your license",
    `<p class="eyebrow">Your license</p>
<h1>Hotfix Server</h1>
${notice(message)}
<div class="card">
  <p class="usage-text"><strong>${servers.length}</strong> of <strong>${seats}</strong> server${seats === 1 ? "" : "s"} in use</p>
  <div class="bar"><span style="width:${pct}%"></span></div>
</div>
<h2>Servers</h2>
<div class="card flush">${
      servers.length
        ? `<table><thead><tr><th>Server</th><th>Version</th><th>Last seen</th><th></th></tr></thead><tbody>${serverRows}</tbody></table>`
        : `<div class="empty"><p>No servers activated yet.</p><p class="small">On each server, run <code>sudo hotfixd license activate &lt;activation key&gt;</code>.</p></div>`
    }</div>
<h2>Subscriptions</h2>
${subCards || `<div class="card"><p>No subscriptions.</p></div>`}
<h2>Add servers</h2>
<div class="card"><form method="post" action="${url.origin}/api/server/checkout">
  <input type="hidden" name="manage_key" value="${mk}">
  <div class="add">${stepper()}<button class="btn">Buy</button></div>
  <p class="total"><strong data-total>$1</strong> more per month. Your activation key covers the new servers.</p>
</form></div>
<p class="fine">Dodo Payments emails an invoice after every payment. <strong>Billing</strong> opens their customer portal: invoices and your payment method.</p>`,
    { wide: true },
  );
}

function recoverPage(url, error = "") {
  return page(
    "Lost your keys?",
    `<p class="eyebrow">Recover</p>
<h1>Get new license keys</h1>
<p class="lead">Enter the payment id (pay_…) from a Dodo Payments receipt email for this license. You'll get new keys; the old ones stop working, and servers that are already activated keep running.</p>
${notice(error)}
<div class="card"><form method="post" action="${url.origin}/api/server/recover">
  <label class="field" for="payment_id">Payment id</label>
  <input id="payment_id" name="payment_id" placeholder="pay_…" autocomplete="off" spellcheck="false" required>
  <button class="btn block" type="submit">Get new keys</button>
</form></div>`,
  );
}

// ---------------------------------------------------------------------------
// Helpers

// Latest Hotfix Server version (from server-v* releases), so servers can log
// that an update is out without calling GitHub themselves. Cached for an hour.
let latestCache = { value: "", until: 0 };

async function latestVersion(env) {
  if (Date.now() < latestCache.until) return latestCache.value;
  try {
    const headers = { "User-Agent": "hotfix-server-api", Accept: "application/vnd.github+json" };
    if (env.GITHUB_TOKEN) headers.Authorization = `Bearer ${env.GITHUB_TOKEN}`;
    const res = await fetch(`https://api.github.com/repos/${REPO}/releases?per_page=30`, { headers });
    let v = "";
    if (res.ok) {
      const rel = (await res.json()).find((r) => !r.draft && !r.prerelease && /^server-v\d/.test(r.tag_name));
      v = rel ? rel.tag_name.replace(/^server-v/, "") : "";
    }
    latestCache = { value: v, until: Date.now() + (res.ok ? 3600_000 : 300_000) };
    return v;
  } catch {
    latestCache = { value: latestCache.value, until: Date.now() + 300_000 };
    return latestCache.value;
  }
}

export async function signToken(env, claims) {
  const key = await crypto.subtle.importKey("pkcs8", b64decode(env.SERVER_LICENSE_SIGNING_KEY), { name: "Ed25519" }, false, ["sign"]);
  const body = b64url(new TextEncoder().encode(JSON.stringify(claims)));
  const sig = await crypto.subtle.sign("Ed25519", key, new TextEncoder().encode(body));
  return `${body}.${b64url(new Uint8Array(sig))}`;
}

async function dodo(env, method, path, body) {
  const init = { method, headers: { Authorization: `Bearer ${env.DODO_PAYMENTS_API_KEY}` } };
  if (body) {
    init.headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(body);
  }
  const res = await fetch(DODO_API[env.DODO_PAYMENTS_ENVIRONMENT] + path, init);
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    const err = new Error(`dodo ${method} ${path.split("?")[0]}: ${res.status} ${data.code || ""} ${data.message || ""}`.trim());
    err.status = res.status;
    throw err;
  }
  return data;
}

// found resolves a Dodo lookup to null when the object doesn't exist (or the
// id is malformed), and still throws on outages and auth errors.
const found = (p) => p.catch((err) => (err.status === 404 || err.status === 422 ? null : Promise.reject(err)));

async function rateLimited(env, request, kind, perMinute) {
  const key = `rl:${kind}:${networkOf(request)}:${Math.floor(Date.now() / 60000)}`;
  return (await env.KV.incr(key, 120)) > perMinute;
}

// networkOf groups addresses by network: IPv4 /24, IPv6 /56. The client
// address comes from server.js (X-Client-IP, from Railway's proxy).
function networkOf(request) {
  const ip = request.headers.get("X-Client-IP") || "unknown";
  if (ip.includes(":")) return ip.split(":").slice(0, 4).join(":").replace(/:[0-9a-f]{0,2}$/i, ":") + "/56";
  return ip.split(".").slice(0, 3).join(".") + ".0/24";
}

function timingSafeEqual(a, b) {
  if (typeof a !== "string" || typeof b !== "string" || a.length !== b.length) return false;
  let diff = 0;
  for (let i = 0; i < a.length; i++) diff |= a.charCodeAt(i) ^ b.charCodeAt(i);
  return diff === 0;
}

async function sha256hex(s) {
  return hex(new Uint8Array(await crypto.subtle.digest("SHA-256", new TextEncoder().encode(s))));
}

function randomHex(bytes) {
  return hex(crypto.getRandomValues(new Uint8Array(bytes)));
}

const hex = (b) => Array.from(b, (x) => x.toString(16).padStart(2, "0")).join("");
const nowSec = () => Math.floor(Date.now() / 1000);
const intVar = (v, d) => {
  const n = parseInt(v ?? "", 10);
  return Number.isFinite(n) && n >= 0 ? n : d;
};
const formatDate = (unix) => (unix ? new Date(unix * 1000).toUTCString().slice(5, 16) : "");
const unixOf = (iso) => (iso ? Math.floor(Date.parse(iso) / 1000) || null : null);

async function readJSON(request) {
  try {
    return await request.json();
  } catch {
    return null;
  }
}

function json(obj, status = 200) {
  return new Response(JSON.stringify(obj), { status, headers: { "Content-Type": "application/json", "Cache-Control": "no-store" } });
}

function text(body, status) {
  return new Response(body, { status, headers: { "Cache-Control": "no-store" } });
}

function redirect(location) {
  return new Response(null, { status: 303, headers: { Location: location, "Cache-Control": "no-store" } });
}

function b64decode(s) {
  return Uint8Array.from(atob(s), (c) => c.charCodeAt(0));
}

function b64(bytes) {
  let bin = "";
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin);
}

function b64url(bytes) {
  return b64(bytes).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}
