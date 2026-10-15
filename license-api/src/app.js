// Hotfix Server licensing API: license.buildcraft.town/api/server/*
//
// A Node service on Railway (src/server.js) backed by PostgreSQL (src/db.js).
// The handler is fetch-style (Request in, Response out).
//
// Model: $1 per server per month on Razorpay Subscriptions (quantity = seats).
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
//   GET  /api/server/buy, POST /api/server/checkout, POST /api/server/success?sub=
//   GET|POST /api/server/manage, GET|POST /api/server/recover
// Razorpay:
//   POST /api/server/webhook
//
// Tokens: base64url(JSON claims) "." base64url(Ed25519 signature), as for the
// earlier desktop licensing; claims carry product "server". Verified by
// server/license.go with the public key compiled into hotfixd.
//
// env: DB and KV (src/db.js), plus the environment variables
//   RAZORPAY_KEY_ID, RAZORPAY_KEY_SECRET, RAZORPAY_WEBHOOK_SECRET,
//   RAZORPAY_SERVER_PLAN_ID, SERVER_LICENSE_SIGNING_KEY,
//   optional TRIAL_DAYS (14), TRIAL_IP_DAILY (25), GITHUB_TOKEN (latest-version lookup).

const PRODUCT = "server";
const DAY = 86400;
const TOKEN_TTL = 7 * DAY; // offline grace for a licensed server
const LEASE = 8 * DAY; // a seat outlives its token by a day
const DEFAULT_TRIAL_DAYS = 14;
const DEFAULT_TRIAL_IP_DAILY = 25;
const TOTAL_MONTHS = 120; // Razorpay allows at most 10 years of cycles
const MAX_QUANTITY = 1000;
// Statuses that keep seats. "pending" = a failed charge Razorpay is retrying.
const LIVE = new Set(["active", "authenticated", "pending"]);
const FP_RE = /^[0-9a-f]{64}$/;
const ID_RE = /^[0-9a-f]{32}$/;
const SUB_RE = /^sub_[A-Za-z0-9]+$/;
const PAY_RE = /^pay_[A-Za-z0-9]+$/;
const RAZORPAY_API = "https://api.razorpay.com/v1";
const REPO = "BuildCraftLabsLLP/hotfix";

export default {
  async fetch(request, env) {
    const url = new URL(request.url);
    const seg = url.pathname.replace(/^\/api\/server\/?/, "").replace(/\/$/, "");
    const route = `${request.method} ${seg}`;
    try {
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
          return buyPage(url);
        case "POST checkout":
          return await checkout(request, env, url);
        case "POST success":
          return await success(request, env, url);
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
  const rows = await env.DB.prepare("SELECT status, quantity, current_end FROM subscriptions WHERE license_id = ?").bind(licenseId).all();
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

  const now = nowSec();
  const sub = await razorpay(env, "POST", "/subscriptions", {
    plan_id: env.RAZORPAY_SERVER_PLAN_ID,
    total_count: TOTAL_MONTHS,
    quantity,
    customer_notify: true,
    notes: { product: PRODUCT },
    expire_by: now + DAY,
  });
  await env.DB
    .prepare("INSERT INTO subscriptions (id, pending_license_id, status, quantity, created, updated) VALUES (?,?,?,?,?,?)")
    .bind(sub.id, pendingLicense, sub.status || "created", quantity, now, now)
    .run();

  const options = {
    key: env.RAZORPAY_KEY_ID,
    subscription_id: sub.id,
    name: "Hotfix Server",
    description: `${quantity} server${quantity === 1 ? "" : "s"}, $${quantity}/month`,
    callback_url: `${url.origin}/api/server/success?sub=${sub.id}`,
    redirect: true,
    theme: { color: "#f26b1d" },
  };
  const opts = JSON.stringify(options).replace(/</g, "\\u003c");
  return page(
    "Subscribe",
    `<h1>Hotfix Server</h1>
<p>${quantity} server${quantity === 1 ? "" : "s"}: <strong>$${quantity} per month</strong>. Cancel any time.</p>
<button id="pay">Continue to payment</button>
<script src="https://checkout.razorpay.com/v1/checkout.js"></script>
<script>
  const options = ${opts};
  options.modal = { ondismiss: () => { location.href = "/api/server/cancelled"; } };
  const open = () => new Razorpay(options).open();
  document.getElementById("pay").onclick = open;
  open();
</script>`,
  );
}

// Checkout's callback. Keys are created here, once per subscription, and shown
// once: if the page never loads, the buyer recovers them with the
// subscription and payment ids from Razorpay's receipt (see recover).
async function success(request, env, url) {
  const subId = url.searchParams.get("sub") || "";
  if (!SUB_RE.test(subId)) return text("Invalid subscription", 400);
  const form = await request.formData().catch(() => new FormData());
  const paymentId = String(form.get("razorpay_payment_id") || "");
  const signature = String(form.get("razorpay_signature") || "");
  const row = await env.DB.prepare("SELECT * FROM subscriptions WHERE id = ?").bind(subId).first();
  if (!row || !paymentId || !signature) {
    return page("Payment not completed", "<p>The payment didn't go through, so nothing was charged or activated.</p>");
  }
  if (!(await hmacHexEqual(env.RAZORPAY_KEY_SECRET, `${paymentId}|${subId}`, signature))) {
    return text("Invalid payment signature", 400);
  }
  const sub = await razorpay(env, "GET", `/subscriptions/${subId}`);
  await syncSubscription(env, sub);
  if (!LIVE.has(sub.status)) {
    return page("Payment not completed", "<p>The payment didn't go through, so nothing was activated.</p>");
  }
  return await linkOrIssue(env, url, subId, row.pending_license_id, sub.quantity);
}

// Attach a paid subscription to its license: an existing one ("add servers"),
// or a new license with fresh keys. Idempotent: only the first call for a
// subscription links it (and so only it shows the new keys).
async function linkOrIssue(env, url, subId, pendingLicense, quantity) {
  const db = env.DB;
  const servers = `${quantity} server${quantity === 1 ? "" : "s"}`;
  if (pendingLicense) {
    const res = await db.prepare("UPDATE subscriptions SET license_id = ? WHERE id = ? AND license_id IS NULL").bind(pendingLicense, subId).run();
    if (res.meta.changes !== 1) return alreadyIssued(url);
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

function alreadyIssued(url) {
  return page(
    "Already done",
    `<h1>This payment was already processed</h1>
<p>Keys are shown only once. If you didn't save them, <a href="${url.origin}/api/server/recover">get new keys</a> with the subscription and payment ids from Razorpay's receipt email.</p>`,
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
  } else if ((action === "quantity" || action === "cancel") && SUB_RE.test(target)) {
    const row = await db.prepare("SELECT * FROM subscriptions WHERE id = ? AND license_id = ?").bind(target, lic.id).first();
    if (!row) {
      notice = "Unknown subscription.";
    } else if (action === "cancel") {
      const sub = await razorpay(env, "POST", `/subscriptions/${target}/cancel`, { cancel_at_cycle_end: true });
      await syncSubscription(env, sub, { cancel_scheduled: 1 });
      notice = `Cancelled. Its servers stay covered until ${formatDate(sub.current_end || row.current_end)}; no further charges.`;
    } else {
      notice = await changeQuantity(env, row, parseInt(String(form.get("quantity") || ""), 10));
    }
  }
  return managePage(env, url, manageKey, lic.id, now, notice);
}

// Seat increases apply at once (prorated by Razorpay); if Razorpay refuses an
// immediate change (e.g. the prorated amount is below its minimum) it's
// scheduled for the next cycle, but the seats are granted now. Decreases apply
// at the next cycle. UPI / e-mandate subscriptions can't be changed at all.
async function changeQuantity(env, row, quantity) {
  if (!Number.isInteger(quantity) || quantity < 1 || quantity > MAX_QUANTITY) return "Pick between 1 and 1000 servers.";
  if (quantity === row.quantity) return "No change.";
  const increase = quantity > row.quantity;
  const tryPatch = (when) => razorpay(env, "PATCH", `/subscriptions/${row.id}`, { quantity, schedule_change_at: when });
  try {
    if (increase) {
      try {
        await tryPatch("now");
      } catch {
        await tryPatch("cycle_end");
      }
      await env.DB.prepare("UPDATE subscriptions SET quantity = ?, updated = ? WHERE id = ?").bind(quantity, nowSec(), row.id).run();
      return `Now ${quantity} servers. The extra servers can be activated right away.`;
    }
    await tryPatch("cycle_end");
    return `Changes to ${quantity} servers at the next billing date (${formatDate(row.current_end)}).`;
  } catch {
    return "This subscription can't be changed (subscriptions paid by UPI or e-mandate are fixed). To add servers, buy more below; to reduce, cancel it and buy a smaller one.";
  }
}

async function licenseByManageKey(env, key) {
  const k = normalizeKey(key, "HFXM");
  if (!k) return null;
  return env.DB.prepare("SELECT id FROM licenses WHERE manage_hash = ?").bind(await sha256hex(k)).first();
}

// ---------------------------------------------------------------------------
// Recovery: new keys, proven by the subscription + payment ids in Razorpay's
// receipt. Running servers keep working (they refresh with their seat secret).

async function recover(request, env, url) {
  if (await rateLimited(env, request, "recover", 10)) return page("Slow down", "<p>Too many attempts. Try again in a minute.</p>");
  const form = await request.formData();
  const subId = String(form.get("subscription_id") || "").trim();
  const paymentId = String(form.get("payment_id") || "").trim();
  if (!SUB_RE.test(subId) || !PAY_RE.test(paymentId)) return recoverPage(url, "Enter both ids exactly as they appear in the receipt (sub_… and pay_…).");

  const row = await env.DB.prepare("SELECT * FROM subscriptions WHERE id = ?").bind(subId).first();
  if (!row) return recoverPage(url, "No Hotfix Server subscription with that id.");
  const invoices = await razorpay(env, "GET", `/invoices?subscription_id=${subId}`);
  const paid = (invoices.items || []).some((inv) => inv.payment_id === paymentId && inv.status === "paid");
  if (!paid) return recoverPage(url, "That payment doesn't belong to that subscription.");

  if (!row.license_id) {
    const sub = await razorpay(env, "GET", `/subscriptions/${subId}`);
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
  if (!(await hmacHexEqual(env.RAZORPAY_WEBHOOK_SECRET, body, request.headers.get("X-Razorpay-Signature") || ""))) {
    return text("Bad signature", 400);
  }
  const event = JSON.parse(body);
  const entity = event.payload && event.payload.subscription && event.payload.subscription.entity;
  if (typeof event.event === "string" && event.event.startsWith("subscription.") && entity && SUB_RE.test(entity.id || "")) {
    // Re-fetch rather than trust the event: replays and out-of-order delivery
    // then can't store a stale state.
    const known = await env.DB.prepare("SELECT id FROM subscriptions WHERE id = ?").bind(entity.id).first();
    if (known) await syncSubscription(env, await razorpay(env, "GET", `/subscriptions/${entity.id}`));
  }
  return text("ok", 200);
}

async function syncSubscription(env, sub, extra = {}) {
  await env.DB
    .prepare(
      `UPDATE subscriptions SET status = ?, current_end = COALESCE(?::bigint, current_end),
       quantity = CASE WHEN ?::integer > quantity OR ?::integer = 0 THEN ?::integer ELSE quantity END,
       cancel_scheduled = GREATEST(cancel_scheduled, ?), updated = ? WHERE id = ?`,
    )
    .bind(
      sub.status,
      sub.current_end || sub.charge_at || null,
      // Razorpay's quantity drops only when a scheduled decrease takes effect
      // (at the cycle end), so syncing it down is only safe once the
      // subscription has no scheduled change pending.
      sub.quantity || 1,
      sub.has_scheduled_changes ? 1 : 0,
      sub.quantity || 1,
      extra.cancel_scheduled || 0,
      nowSec(),
      sub.id,
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

const STYLE = `
  body { margin: 0; min-height: 100vh; display: grid; place-items: center;
         font: 16px/1.5 system-ui, sans-serif; background: #14110f; color: #f3ece6; }
  main { max-width: 40rem; padding: 2rem 1rem; }
  h1 { font-size: 1.5rem; margin: 0 0 .75rem; }
  p, li { color: #c9bdb3; }
  a { color: #f6a06a; }
  code, .key { font-family: ui-monospace, monospace; }
  .key { display: block; font-size: 1.15rem; padding: .6rem .8rem; margin: .3rem 0 1rem;
         background: #221c18; border: 1px solid #4a403a; border-radius: .5rem; user-select: all; }
  input { font: inherit; padding: .5rem .6rem; border-radius: .4rem; border: 1px solid #4a403a;
          background: #221c18; color: #f3ece6; }
  button { font: inherit; font-weight: 600; padding: .6rem 1.2rem; border: 0; border-radius: .5rem;
           background: #f26b1d; color: #fff; cursor: pointer; }
  button.quiet { background: transparent; color: #c9bdb3; border: 1px solid #4a403a; padding: .3rem .7rem; }
  table { border-collapse: collapse; width: 100%; margin: .5rem 0 1.5rem; }
  td, th { text-align: left; padding: .35rem .5rem; border-bottom: 1px solid #2c2521; font-size: .92rem; }
  .notice { background: #2a2016; border-left: 3px solid #f26b1d; padding: .6rem .8rem; }
  form.inline { display: inline; }`;

const esc = (s) => String(s ?? "").replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;").replace(/"/g, "&quot;");

function page(title, inner) {
  return new Response(
    `<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1"><meta name="robots" content="noindex">
<title>Hotfix Server — ${esc(title)}</title><style>${STYLE}</style></head>
<body><main>${inner.startsWith("<h1") ? "" : `<h1>${esc(title)}</h1>`}${inner}</main></body></html>`,
    { headers: { "Content-Type": "text/html; charset=utf-8", "Cache-Control": "no-store", "X-Frame-Options": "DENY" } },
  );
}

function buyPage(url) {
  return page(
    "Buy Hotfix Server",
    `<h1>Buy Hotfix Server</h1>
<p><strong>$1 per server per month.</strong> Every server also gets a 14-day free trial when you install it, no card needed.</p>
<form method="post" action="${url.origin}/api/server/checkout">
  <p><label>Servers: <input name="quantity" type="number" min="1" max="${MAX_QUANTITY}" value="1" required style="width:6rem"></label></p>
  <p><label>Adding servers to an existing license? Its manage key (optional):<br>
    <input name="manage_key" placeholder="HFXM-…" style="width:22rem" autocomplete="off"></label></p>
  <button type="submit">Continue</button>
</form>
<p>After paying you get an activation key for your servers and a manage key for billing.</p>`,
  );
}

function keysPage(url, keys, heading, extra = "") {
  const file = `Hotfix Server license\n\nActivation key (put on your servers):\n${keys.activation}\n\nManage key (billing, keep private):\n${keys.manage}\n\nManage: ${url.origin}/api/server/manage\n`;
  return page(
    heading,
    `<h1>${esc(heading)}</h1>
${extra ? `<p class="notice">${esc(extra)}</p>` : ""}
<p><strong>Save these keys now: they are shown only once.</strong></p>
<p>Activation key: put this on your servers.</p>
<span class="key">${esc(keys.activation)}</span>
<p>Manage key: for billing (add servers, cancel). Keep it private.</p>
<span class="key">${esc(keys.manage)}</span>
<p><a download="hotfix-server-license.txt" href="data:text/plain;charset=utf-8,${encodeURIComponent(file)}">Download as a text file</a></p>
<h2>Activate a server</h2>
<p><code>sudo hotfixd license activate ${esc(keys.activation)}</code> (Windows: in an administrator prompt, without sudo)</p>
<p>Manage your license any time at <a href="${url.origin}/api/server/manage">${url.origin}/api/server/manage</a>.</p>`,
  );
}

function manageLoginPage(url, error = "") {
  return page(
    "Manage your license",
    `<h1>Manage your Hotfix Server license</h1>
${error ? `<p class="notice">${esc(error)}</p>` : ""}
<form method="post" action="${url.origin}/api/server/manage">
  <p><label>Manage key:<br><input name="manage_key" placeholder="HFXM-…" style="width:22rem" autocomplete="off" required></label></p>
  <button type="submit">Open</button>
</form>
<p>Lost your keys? <a href="${url.origin}/api/server/recover">Get new ones</a>.</p>`,
  );
}

async function managePage(env, url, manageKey, licenseId, now, notice) {
  const { seats, subscriptions } = await licenseSeats(env, licenseId, now);
  const servers = (await env.DB.prepare("SELECT * FROM seats WHERE license_id = ? AND lease_until > ? ORDER BY last_seen DESC").bind(licenseId, now).all()).results || [];
  const mk = esc(manageKey);
  const hidden = (action, target) => `<input type="hidden" name="manage_key" value="${mk}"><input type="hidden" name="action" value="${action}"><input type="hidden" name="target" value="${esc(target)}">`;

  const subRows = subscriptions
    .map((s) => {
      const live = LIVE.has(s.status) && !s.cancel_scheduled;
      return `<tr><td>${esc(s.id)}</td><td>${s.quantity}</td><td>${esc(s.cancel_scheduled ? "cancelling" : s.status)}</td><td>${s.current_end ? formatDate(s.current_end) : ""}</td>
<td>${live ? `<form class="inline" method="post">${hidden("quantity", s.id)}<input name="quantity" type="number" min="1" max="${MAX_QUANTITY}" value="${s.quantity}" style="width:4.5rem"> <button class="quiet">Change</button></form>
<form class="inline" method="post" onsubmit="return confirm('Cancel this subscription at the end of the paid period?')">${hidden("cancel", s.id)}<button class="quiet">Cancel</button></form>` : ""}</td></tr>`;
    })
    .join("");
  const serverRows = servers
    .map(
      (s) => `<tr><td>${esc(s.hostname || "?")}</td><td>${esc(s.os)}</td><td>${esc(s.version)}</td><td>${formatDate(s.last_seen)}</td>
<td><form class="inline" method="post" onsubmit="return confirm('Release this server\\'s seat?')">${hidden("release", s.id)}<button class="quiet">Release</button></form></td></tr>`,
    )
    .join("");

  return page(
    "Your license",
    `<h1>Your Hotfix Server license</h1>
${notice ? `<p class="notice">${esc(notice)}</p>` : ""}
<p><strong>${servers.length}</strong> of <strong>${seats}</strong> server${seats === 1 ? "" : "s"} in use.</p>
<h2>Servers</h2>
${servers.length ? `<table><tr><th>Host</th><th>OS</th><th>Version</th><th>Last seen</th><th></th></tr>${serverRows}</table>` : "<p>No servers activated yet.</p>"}
<h2>Subscriptions</h2>
<table><tr><th>Subscription</th><th>Servers</th><th>Status</th><th>Paid until</th><th></th></tr>${subRows}</table>
<form method="post" action="${url.origin}/api/server/checkout">
  <input type="hidden" name="manage_key" value="${mk}">
  <label>Add servers: <input name="quantity" type="number" min="1" max="${MAX_QUANTITY}" value="1" style="width:4.5rem"></label>
  <button>Buy</button>
</form>
<p>Invoices are emailed by Razorpay after every payment.</p>`,
  );
}

function recoverPage(url, error = "") {
  return page(
    "Lost your keys?",
    `<h1>Get new license keys</h1>
${error ? `<p class="notice">${esc(error)}</p>` : ""}
<p>Enter the subscription id (sub_…) and a payment id (pay_…) from a Razorpay receipt email for this license. You'll get new keys; the old ones stop working, and servers that are already activated keep running.</p>
<form method="post" action="${url.origin}/api/server/recover">
  <p><label>Subscription id:<br><input name="subscription_id" placeholder="sub_…" style="width:22rem" required></label></p>
  <p><label>Payment id:<br><input name="payment_id" placeholder="pay_…" style="width:22rem" required></label></p>
  <button type="submit">Get new keys</button>
</form>`,
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

async function razorpay(env, method, path, body) {
  const init = { method, headers: { Authorization: `Basic ${btoa(`${env.RAZORPAY_KEY_ID}:${env.RAZORPAY_KEY_SECRET}`)}` } };
  if (body) {
    init.headers["Content-Type"] = "application/json";
    init.body = JSON.stringify(body);
  }
  const res = await fetch(RAZORPAY_API + path, init);
  const data = await res.json();
  if (!res.ok) throw new Error(`razorpay ${method} ${path.split("?")[0]}: ${res.status} ${(data.error && data.error.description) || ""}`);
  return data;
}

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

export async function hmacHexEqual(secret, message, signature) {
  if (!secret || !signature) return false;
  const key = await crypto.subtle.importKey("raw", new TextEncoder().encode(secret), { name: "HMAC", hash: "SHA-256" }, false, ["sign"]);
  const mac = new Uint8Array(await crypto.subtle.sign("HMAC", key, new TextEncoder().encode(message)));
  return timingSafeEqual(signature.trim().toLowerCase(), hex(mac));
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

function b64decode(s) {
  return Uint8Array.from(atob(s), (c) => c.charCodeAt(0));
}

function b64url(bytes) {
  let bin = "";
  for (const b of bytes) bin += String.fromCharCode(b);
  return btoa(bin).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}
