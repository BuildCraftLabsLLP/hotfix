// Tests for the Hotfix Server licensing API (worker/src/server-api.js).
//   node --test worker/test/server-api.test.mjs      (Node 24+: uses node:sqlite)
// D1 is real SQLite (node:sqlite) with the real migration; KV and the Razorpay
// and GitHub APIs are faked in-process. Nothing touches the network.
import { test, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";
import { createHmac, generateKeyPairSync, verify as edVerify } from "node:crypto";
import { DatabaseSync } from "node:sqlite";

import api, { makeKey, normalizeKey } from "../src/server-api.js";

const SCHEMA = readFileSync(new URL("../migrations/0001_server_licensing.sql", import.meta.url), "utf8");
const KEY_ID = "rzp_test_key";
const KEY_SECRET = "rzp_test_secret";
const WEBHOOK_SECRET = "whsec_server";
const { publicKey, privateKey } = generateKeyPairSync("ed25519");

const fp = (c) => c.repeat(64);
const inst = (c) => c.repeat(32);

// D1's API over node:sqlite.
function fakeD1() {
  const db = new DatabaseSync(":memory:");
  db.exec(SCHEMA);
  const statement = (sql, args = []) => ({
    bind: (...a) => statement(sql, a),
    first: async () => db.prepare(sql).get(...args) ?? null,
    all: async () => ({ results: db.prepare(sql).all(...args) }),
    run: async () => ({ meta: { changes: Number(db.prepare(sql).run(...args).changes) } }),
  });
  return { prepare: (sql) => statement(sql), db };
}

function fakeKV() {
  const m = new Map();
  return {
    m,
    async get(k, type) {
      const v = m.get(k);
      return v === undefined ? null : type === "json" ? JSON.parse(v) : v;
    },
    async put(k, v) {
      m.set(k, v);
    },
  };
}

let env, rzp, rzpCalls, clock;
const realFetch = globalThis.fetch;
const realNow = Date.now;

beforeEach(() => {
  env = {
    HOTFIX_SERVER_DB: fakeD1(),
    HOTFIX_SERVER_KV: fakeKV(),
    RAZORPAY_KEY_ID: KEY_ID,
    RAZORPAY_KEY_SECRET: KEY_SECRET,
    RAZORPAY_WEBHOOK_SECRET: WEBHOOK_SECRET,
    RAZORPAY_SERVER_PLAN_ID: "plan_SERVER0000001",
    SERVER_LICENSE_SIGNING_KEY: privateKey.export({ type: "pkcs8", format: "der" }).toString("base64"),
  };
  clock = 1_800_000_000;
  Date.now = () => clock * 1000;
  rzp = { subs: new Map(), patchFails: [], invoices: new Map() };
  rzpCalls = [];
  globalThis.fetch = async (url, init = {}) => {
    const u = new URL(url);
    if (u.origin === "https://api.github.com") {
      return Response.json([{ tag_name: "v1.0.13", draft: false, prerelease: false }, { tag_name: "server-v1.2.0", draft: false, prerelease: false }]);
    }
    assert.equal(u.origin, "https://api.razorpay.com");
    assert.equal(init.headers.Authorization, `Basic ${Buffer.from(`${KEY_ID}:${KEY_SECRET}`).toString("base64")}`);
    const body = init.body ? JSON.parse(init.body) : null;
    rzpCalls.push({ method: init.method, path: u.pathname + u.search, body });
    let m;
    if (init.method === "POST" && u.pathname === "/v1/subscriptions") {
      const id = "sub_" + (rzp.subs.size + 1).toString().padStart(14, "0");
      rzp.subs.set(id, { id, status: "created", quantity: body.quantity, notes: body.notes });
      return Response.json(rzp.subs.get(id));
    }
    if ((m = u.pathname.match(/^\/v1\/subscriptions\/(sub_\w+)$/))) {
      const s = rzp.subs.get(m[1]);
      if (init.method === "PATCH") {
        const fail = rzp.patchFails.shift();
        if (fail) return Response.json({ error: { description: fail } }, { status: 400 });
        if (body.schedule_change_at === "now") s.quantity = body.quantity;
        else s.has_scheduled_changes = true;
      }
      return Response.json(s);
    }
    if ((m = u.pathname.match(/^\/v1\/subscriptions\/(sub_\w+)\/cancel$/))) {
      const s = rzp.subs.get(m[1]);
      s.has_scheduled_changes = true;
      return Response.json(s);
    }
    if (u.pathname === "/v1/invoices") {
      return Response.json({ items: rzp.invoices.get(u.searchParams.get("subscription_id")) || [] });
    }
    return Response.json({ error: { description: "unsupported" } }, { status: 400 });
  };
});

afterEach(() => {
  globalThis.fetch = realFetch;
  Date.now = realNow;
});

const call = (path, init = {}) =>
  api.fetch(new Request(`https://hotfix.buildcraft.town/api/server/${path}`, { ...init, headers: { "CF-Connecting-IP": "203.0.113.7", ...(init.headers || {}) } }), env);
const postJSON = (path, body, ip) =>
  call(path, { method: "POST", body: JSON.stringify(body), headers: { "Content-Type": "application/json", ...(ip ? { "CF-Connecting-IP": ip } : {}) } });
const postForm = (path, fields) => call(path, { method: "POST", body: new URLSearchParams(fields).toString(), headers: { "Content-Type": "application/x-www-form-urlencoded" } });

function claims(token) {
  const [body, sig] = token.split(".");
  assert.ok(edVerify(null, Buffer.from(body), publicKey, Buffer.from(sig, "base64url")), "token signature");
  return JSON.parse(Buffer.from(body, "base64url").toString());
}

// Buys `quantity` seats end-to-end and returns the keys shown on the success page.
async function buy(quantity, manageKey) {
  const res = await postForm("checkout", { quantity: String(quantity), ...(manageKey ? { manage_key: manageKey } : {}) });
  const html = await res.text();
  const subId = html.match(/success\?sub=(sub_\w+)/)[1];
  rzp.subs.get(subId).status = "active";
  rzp.subs.get(subId).current_end = clock + 30 * 86400;
  const page = await (await paySuccess(subId)).text();
  const keys = { activation: page.match(/HFXS(-[0-9A-Z]{5}){4}/)?.[0], manage: page.match(/HFXM(-[0-9A-Z]{5}){4}/)?.[0], subId, page };
  return keys;
}

function paySuccess(subId, { paymentId = "pay_" + subId.slice(4), signature } = {}) {
  return call(`success?sub=${subId}`, {
    method: "POST",
    body: new URLSearchParams({ razorpay_payment_id: paymentId, razorpay_subscription_id: subId, razorpay_signature: signature ?? createHmac("sha256", KEY_SECRET).update(`${paymentId}|${subId}`).digest("hex") }).toString(),
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
  });
}

const machine = (c, extra = {}) => ({ fingerprint: fp(c), instance_id: inst(c), hostname: `host-${c}`, os: "linux", version: "1.0.0", ...extra });

// --- keys -------------------------------------------------------------------

test("keys: format, check character and forgiving normalization", () => {
  const k = makeKey("HFXS");
  assert.match(k, /^HFXS(-[0-9A-HJKMNP-TV-Z]{5}){4}$/);
  assert.ok(normalizeKey(k, "HFXS"));
  assert.equal(normalizeKey(k.toLowerCase().replace(/-/g, " "), "HFXS"), normalizeKey(k, "HFXS"));
  const typo = k.slice(0, -1) + (k.at(-1) === "0" ? "1" : "0");
  assert.equal(normalizeKey(typo, "HFXS"), null, "a typo fails the check character");
  assert.equal(normalizeKey(k, "HFXM"), null, "wrong key type");
  assert.equal(normalizeKey("HFXS-short", "HFXS"), null);
});

// --- trial ------------------------------------------------------------------

test("trial: 14 days per machine, never restarted", async () => {
  let c = claims((await (await postJSON("trial", { fingerprint: fp("a") })).json()).token);
  assert.equal(c.product, "server");
  assert.equal(c.plan, "trial");
  assert.equal(c.trial_end, clock + 14 * 86400);
  assert.equal(c.latest_version, "1.2.0");
  assert.ok(c.exp <= c.trial_end);

  clock += 10 * 86400;
  c = claims((await (await postJSON("trial", { fingerprint: fp("a") })).json()).token);
  assert.equal(c.trial_end, 1_800_000_000 + 14 * 86400, "same trial, not a new one");

  clock += 5 * 86400;
  c = claims((await (await postJSON("trial", { fingerprint: fp("a") })).json()).token);
  assert.equal(c.status, "inactive");
  assert.equal(c.plan, "trial_expired");
});

test("trial: new trials per network per day are capped", async () => {
  env.TRIAL_IP_DAILY = "3";
  for (const ch of "abc") assert.equal((await postJSON("trial", { fingerprint: fp(ch) }, "198.51.100.1")).status, 200);
  const res = await postJSON("trial", { fingerprint: fp("d") }, "198.51.100.99"); // same /24
  assert.equal(res.status, 429);
  assert.equal((await res.json()).error, "trial_limit");
  assert.equal((await postJSON("trial", { fingerprint: fp("a") }, "198.51.100.1")).status, 200, "existing trials keep working");
  assert.equal((await postJSON("trial", { fingerprint: fp("e") }, "192.0.2.1")).status, 200, "other networks unaffected");
});

test("trial: rejects bad fingerprints", async () => {
  assert.equal((await postJSON("trial", { fingerprint: "nope" })).status, 400);
  assert.equal((await call("trial")).status, 405);
});

// --- buying -----------------------------------------------------------------

test("buy: checkout creates a subscription for N seats; success issues keys once", async () => {
  const keys = await buy(3);
  const create = rzpCalls.find((c) => c.method === "POST" && c.path === "/v1/subscriptions");
  assert.equal(create.body.quantity, 3);
  assert.equal(create.body.plan_id, "plan_SERVER0000001");
  assert.deepEqual(create.body.notes, { product: "server" });
  assert.ok(keys.activation && keys.manage, "both keys shown");
  assert.match(keys.page, /shown only once/);

  const again = await (await paySuccess(keys.subId)).text();
  assert.match(again, /already processed/);
  assert.doesNotMatch(again, /HFXS-/, "keys are never shown twice");

  const stored = env.HOTFIX_SERVER_DB.db.prepare("SELECT * FROM licenses").all();
  assert.equal(stored.length, 1);
  assert.doesNotMatch(JSON.stringify(stored), new RegExp(keys.activation.replace(/-/g, "").slice(4)), "only hashes are stored");
});

test("buy: forged or unpaid callbacks issue nothing", async () => {
  const res = await postForm("checkout", { quantity: "1" });
  const subId = (await res.text()).match(/success\?sub=(sub_\w+)/)[1];
  assert.equal((await paySuccess(subId, { signature: "0".repeat(64) })).status, 400);
  // Genuine signature but the subscription isn't paid.
  assert.match(await (await paySuccess(subId)).text(), /Payment not completed/);
  assert.equal(env.HOTFIX_SERVER_DB.db.prepare("SELECT COUNT(*) n FROM licenses").get().n, 0);
  assert.match(await (await postForm("checkout", { quantity: "0" })).text(), /between 1 and/);
});

// --- seats ------------------------------------------------------------------

test("activate: N seats (plus one spare), same server reuses its seat", async () => {
  const { activation } = await buy(3);
  for (const ch of "abcd") {
    const res = await postJSON("activate", { key: activation, ...machine(ch) });
    assert.equal(res.status, 200, `server ${ch}`);
    const body = await res.json();
    assert.match(body.seat_id, /^seat_[0-9a-f]{24}$/);
    const c = claims(body.token);
    assert.equal(c.plan, "subscription");
    assert.equal(c.exp - c.iat, 7 * 86400);
  }
  const full = await postJSON("activate", { key: activation, ...machine("e") });
  assert.equal(full.status, 409);
  assert.equal((await full.json()).error, "no_seats");
  assert.equal((await postJSON("activate", { key: activation, ...machine("a") })).status, 200, "re-activating a server doesn't need a new seat");
});

test("activate: concurrent activations never oversell", async () => {
  const { activation } = await buy(3);
  const results = await Promise.all("0123456789".split("").map((d) => postJSON("activate", { key: activation, ...machine(d) })));
  assert.equal(results.filter((r) => r.status === 200).length, 4); // 3 seats + 1 spare
  assert.equal(results.filter((r) => r.status === 409).length, 6);
});

test("activate: bad and unknown keys", async () => {
  await buy(1);
  assert.equal((await postJSON("activate", { key: "HFXS-0000", ...machine("a") })).status, 400);
  assert.equal((await postJSON("activate", { key: makeKey("HFXS"), ...machine("a") })).status, 403);
  assert.equal((await postJSON("activate", { key: makeKey("HFXS"), fingerprint: "x" })).status, 400);
});

test("refresh: renews the lease; wrong secret or released seat is gone", async () => {
  const { activation } = await buy(1);
  const seat = await (await postJSON("activate", { key: activation, ...machine("a") })).json();
  clock += 3 * 86400;
  const res = await postJSON("refresh", { seat_id: seat.seat_id, seat_secret: seat.seat_secret, ...machine("a") });
  assert.equal(res.status, 200);
  assert.equal(claims((await res.json()).token).exp, clock + 7 * 86400);

  assert.equal((await postJSON("refresh", { seat_id: seat.seat_id, seat_secret: "f".repeat(64), ...machine("a") })).status, 410);
  await postJSON("deactivate", { seat_id: seat.seat_id, seat_secret: seat.seat_secret });
  assert.equal((await postJSON("refresh", { seat_id: seat.seat_id, seat_secret: seat.seat_secret, ...machine("a") })).status, 410);
});

test("seats: a server that stops refreshing frees its seat when its lease ends", async () => {
  const { activation } = await buy(1); // 1 seat + 1 spare
  await postJSON("activate", { key: activation, ...machine("a") });
  await postJSON("activate", { key: activation, ...machine("b") });
  assert.equal((await postJSON("activate", { key: activation, ...machine("c") })).status, 409);
  clock += 9 * 86400; // leases (8 days) ran out
  assert.equal((await postJSON("activate", { key: activation, ...machine("c") })).status, 200);
});

test("refresh: a lapsed subscription turns the token inactive; renewal brings it back", async () => {
  const { activation, subId } = await buy(1);
  const seat = await (await postJSON("activate", { key: activation, ...machine("a") })).json();
  rzp.subs.get(subId).status = "halted";
  await call("webhook", signed({ event: "subscription.halted", payload: { subscription: { entity: { id: subId } } } }));
  let c = claims((await (await postJSON("refresh", { seat_id: seat.seat_id, seat_secret: seat.seat_secret, ...machine("a") })).json()).token);
  assert.equal(c.status, "inactive");
  assert.equal(c.plan, "lapsed");

  rzp.subs.get(subId).status = "active";
  await call("webhook", signed({ event: "subscription.activated", payload: { subscription: { entity: { id: subId } } } }));
  c = claims((await (await postJSON("refresh", { seat_id: seat.seat_id, seat_secret: seat.seat_secret, ...machine("a") })).json()).token);
  assert.equal(c.status, "active");
});

test("refresh: flags a seat used by two different machines", async () => {
  const { activation } = await buy(1);
  const seat = await (await postJSON("activate", { key: activation, ...machine("a") })).json();
  const res = await postJSON("refresh", { seat_id: seat.seat_id, seat_secret: seat.seat_secret, ...machine("a", { hostname: "clone" }) });
  assert.equal((await res.json()).duplicate_identity, true);
});

// --- manage -----------------------------------------------------------------

test("manage: lists servers, releases a seat, changes quantity, cancels", async () => {
  const { activation, manage, subId } = await buy(2);
  const seat = await (await postJSON("activate", { key: activation, ...machine("a") })).json();

  assert.match(await (await postForm("manage", { manage_key: makeKey("HFXM") })).text(), /wasn&#39;t recognized|wasn't recognized/);
  let html = await (await postForm("manage", { manage_key: manage })).text();
  assert.match(html, /1<\/strong> of <strong>2<\/strong>/);
  assert.match(html, /host-a/);

  html = await (await postForm("manage", { manage_key: manage, action: "release", target: seat.seat_id })).text();
  assert.match(html, /Server released/);
  assert.match(html, /0<\/strong> of <strong>2/);

  // Increase: immediate.
  html = await (await postForm("manage", { manage_key: manage, action: "quantity", target: subId, quantity: "5" })).text();
  assert.match(html, /Now 5 servers/);
  assert.deepEqual(rzpCalls.at(-1).body, { quantity: 5, schedule_change_at: "now" });
  // Immediate change refused (e.g. below Razorpay's minimum): scheduled, seats granted anyway.
  rzp.patchFails.push("amount too low");
  html = await (await postForm("manage", { manage_key: manage, action: "quantity", target: subId, quantity: "6" })).text();
  assert.match(html, /Now 6 servers/);
  assert.equal(rzpCalls.at(-1).body.schedule_change_at, "cycle_end");
  // UPI / e-mandate: can't change.
  rzp.patchFails.push("cannot be updated when payment mode is UPI");
  html = await (await postForm("manage", { manage_key: manage, action: "quantity", target: subId, quantity: "2" })).text();
  assert.match(html, /can&#39;t be changed|can't be changed/);

  html = await (await postForm("manage", { manage_key: manage, action: "cancel", target: subId })).text();
  assert.match(html, /Cancelled/);
  assert.deepEqual(rzpCalls.at(-1).body, { cancel_at_cycle_end: true });
});

test("manage: buying more servers adds them to the same license", async () => {
  const first = await buy(1);
  const more = await buy(2, first.manage);
  assert.match(more.page, /2 servers added/);
  assert.equal(more.activation, undefined, "no new keys");
  for (const ch of "abcd") assert.equal((await postJSON("activate", { key: first.activation, ...machine(ch) })).status, 200, ch); // 3 seats + 1 spare
  assert.equal((await postJSON("activate", { key: first.activation, ...machine("e") })).status, 409);
});

// --- recovery ---------------------------------------------------------------

test("recover: new keys with the receipt's ids; activated servers keep working", async () => {
  const { activation, subId } = await buy(1);
  const seat = await (await postJSON("activate", { key: activation, ...machine("a") })).json();
  rzp.invoices.set(subId, [{ id: "inv_1", payment_id: "pay_REAL", status: "paid" }]);

  assert.match(await (await postForm("recover", { subscription_id: subId, payment_id: "pay_FAKE" })).text(), /doesn&#39;t belong|doesn't belong/);
  const html = await (await postForm("recover", { subscription_id: subId, payment_id: "pay_REAL" })).text();
  const fresh = html.match(/HFXS(-[0-9A-Z]{5}){4}/)[0];
  assert.notEqual(fresh, activation);

  assert.equal((await postJSON("activate", { key: activation, ...machine("b") })).status, 403, "old key revoked");
  assert.equal((await postJSON("activate", { key: fresh, ...machine("b") })).status, 200);
  assert.equal((await postJSON("refresh", { seat_id: seat.seat_id, seat_secret: seat.seat_secret, ...machine("a") })).status, 200, "existing seat unaffected");
});

// --- webhook ----------------------------------------------------------------

function signed(event, secret = WEBHOOK_SECRET) {
  const body = JSON.stringify(event);
  return { method: "POST", body, headers: { "X-Razorpay-Signature": createHmac("sha256", secret).update(body).digest("hex") } };
}

test("webhook: signature checked; state re-fetched from Razorpay", async () => {
  const { subId } = await buy(1);
  const ev = { event: "subscription.cancelled", payload: { subscription: { entity: { id: subId, status: "cancelled" } } } };
  assert.equal((await call("webhook", signed(ev, "wrong"))).status, 400);
  // The event says cancelled, Razorpay says active: Razorpay wins.
  assert.equal((await call("webhook", signed(ev))).status, 200);
  assert.equal(env.HOTFIX_SERVER_DB.db.prepare("SELECT status FROM subscriptions WHERE id = ?").get(subId).status, "active");
  // Unknown subscriptions are ignored.
  const other = { event: "subscription.activated", payload: { subscription: { entity: { id: "sub_UNKNOWN0000000" } } } };
  assert.equal((await call("webhook", signed(other))).status, 200);
});

test("routing", async () => {
  assert.equal((await call("nope")).status, 404);
  assert.equal((await call("activate")).status, 405);
  assert.match(await (await call("buy")).text(), /\$1 per server per month/);
});
