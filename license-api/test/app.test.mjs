// Tests for the Hotfix Server licensing API (src/app.js).
//   npm test                     (in license-api/; Node 24+)
// The database is PostgreSQL: PGlite (Postgres in WebAssembly) by default, or a
// real server when TEST_DATABASE_URL is set (CI), emptied between tests. The
// Dodo Payments and GitHub APIs are faked in-process. Nothing touches the network.
import { test, beforeEach, afterEach, after } from "node:test";
import assert from "node:assert/strict";
import { createHmac, generateKeyPairSync, verify as edVerify } from "node:crypto";

import api, { makeKey, normalizeKey, webhookSignatureValid } from "../src/app.js";
import { pglite, postgres } from "../src/db.js";

const API_KEY = "dodo_test_key";
const WEBHOOK_KEY = "whsec_" + Buffer.from("hotfix-server-webhook-signing-key").toString("base64");
const PRODUCT_ID = "pdt_SERVER0000001";
const { publicKey, privateKey } = generateKeyPairSync("ed25519");

const fp = (c) => c.repeat(64);
const inst = (c) => c.repeat(32);

// One real Postgres for the whole run (CI), or a fresh PGlite per test.
const shared = process.env.TEST_DATABASE_URL ? await postgres(process.env.TEST_DATABASE_URL) : null;
after(async () => shared && (await shared.close()));
let store;
async function freshStore() {
  if (!shared) return pglite();
  await shared.db.prepare("TRUNCATE refund_requests, seats, checkouts, subscriptions, licenses, kv").run();
  return shared;
}
// Direct SQL for assertions.
const sql = async (text, ...args) => (await store.db.prepare(text).bind(...args).all()).results;

let env, dodo, dodoCalls, emails, clock;
const realFetch = globalThis.fetch;
const realNow = Date.now;
const iso = (unix) => new Date(unix * 1000).toISOString();

beforeEach(async () => {
  store = await freshStore();
  env = {
    DB: store.db,
    KV: store.kv,
    DODO_PAYMENTS_API_KEY: API_KEY,
    DODO_PAYMENTS_WEBHOOK_KEY: WEBHOOK_KEY,
    DODO_SERVER_PRODUCT_ID: PRODUCT_ID,
    DODO_PAYMENTS_ENVIRONMENT: "test_mode",
    RESEND_API_KEY: "re_test",
    REFUND_EMAIL_TO: "billing@buildcraft.town",
    REFUND_EMAIL_FROM: "Hotfix Server <refunds@buildcraft.town>",
    SERVER_LICENSE_SIGNING_KEY: privateKey.export({ type: "pkcs8", format: "der" }).toString("base64"),
  };
  clock = 1_800_000_000;
  Date.now = () => clock * 1000;
  // copyMetadata: whether Dodo carries the session's metadata over to the subscription
  // (undocumented; the app also matches by the payment's checkout session).
  dodo = { sessions: [], subs: new Map(), payments: new Map(), changeFails: [], copyMetadata: true };
  dodoCalls = [];
  emails = { sent: [], fail: false };
  globalThis.fetch = async (url, init = {}) => {
    const u = new URL(url);
    if (u.origin === "https://api.github.com") {
      return Response.json([{ tag_name: "v1.0.13", draft: false, prerelease: false }, { tag_name: "server-v1.2.0", draft: false, prerelease: false }]);
    }
    if (u.origin === "https://api.resend.com") {
      assert.equal(init.headers.Authorization, "Bearer re_test");
      if (emails.fail) return Response.json({ message: "domain not verified" }, { status: 403 });
      emails.sent.push({ ...JSON.parse(init.body), idempotencyKey: init.headers["Idempotency-Key"] });
      return Response.json({ id: `email_${emails.sent.length}` });
    }
    assert.equal(u.origin, "https://test.dodopayments.com");
    assert.equal(init.headers.Authorization, `Bearer ${API_KEY}`);
    const body = init.body ? JSON.parse(init.body) : null;
    dodoCalls.push({ method: init.method, path: u.pathname, query: Object.fromEntries(u.searchParams), body });
    const notFound = () => Response.json({ code: "NOT_FOUND", message: "not found" }, { status: 404 });
    let m;
    if (init.method === "POST" && u.pathname === "/checkouts") {
      const session_id = `cks_${dodo.sessions.length + 1}`;
      dodo.sessions.push({ session_id, ...body });
      return Response.json({ session_id, checkout_url: `https://test.checkout.dodopayments.com/session/${session_id}` });
    }
    if ((m = u.pathname.match(/^\/subscriptions\/(sub_\w+)$/))) {
      const s = dodo.subs.get(m[1]);
      if (!s) return notFound();
      if (init.method === "PATCH") Object.assign(s, body);
      return Response.json(s);
    }
    if ((m = u.pathname.match(/^\/subscriptions\/(sub_\w+)\/change-plan$/))) {
      const s = dodo.subs.get(m[1]);
      const fail = dodo.changeFails.shift();
      if (fail) return Response.json({ code: "INVALID_REQUEST", message: fail }, { status: 422 });
      if (body.effective_at === "immediately") s.quantity = body.quantity;
      else s.scheduled_change = { quantity: body.quantity, effective_at: s.next_billing_date };
      return Response.json({ payment_id: null });
    }
    if (u.pathname === "/payments") {
      const status = u.searchParams.get("status");
      return Response.json({ items: [...dodo.payments.values()].filter((p) => p.subscription_id === u.searchParams.get("subscription_id") && (!status || p.status === status)) });
    }
    if ((m = u.pathname.match(/^\/payments\/(pay_\w+)$/))) {
      return dodo.payments.has(m[1]) ? Response.json(dodo.payments.get(m[1])) : notFound();
    }
    if ((m = u.pathname.match(/^\/customers\/(cus_\w+)\/customer-portal\/session$/))) {
      return Response.json({ link: `https://test.customer.dodopayments.com/${m[1]}?return=${encodeURIComponent(u.searchParams.get("return_url"))}` });
    }
    return Response.json({ code: "UNSUPPORTED", message: "unsupported" }, { status: 400 });
  };
});

afterEach(async () => {
  if (!shared) await store.close();
  globalThis.fetch = realFetch;
  Date.now = realNow;
});

const call = (path, init = {}) =>
  api.fetch(new Request(`https://license.buildcraft.town/api/server/${path}`, { ...init, headers: { "X-Client-IP": "203.0.113.7", ...(init.headers || {}) } }), env);
const postJSON = (path, body, ip) =>
  call(path, { method: "POST", body: JSON.stringify(body), headers: { "Content-Type": "application/json", ...(ip ? { "X-Client-IP": ip } : {}) } });
const postForm = (path, fields) => call(path, { method: "POST", body: new URLSearchParams(fields).toString(), headers: { "Content-Type": "application/x-www-form-urlencoded" } });

function claims(token) {
  const [body, sig] = token.split(".");
  assert.ok(edVerify(null, Buffer.from(body), publicKey, Buffer.from(sig, "base64url")), "token signature");
  return JSON.parse(Buffer.from(body, "base64url").toString());
}

// Starts a checkout; returns the session as Dodo received it, plus our ref.
async function startCheckout(quantity, manageKey) {
  const res = await postForm("checkout", { quantity: String(quantity), ...(manageKey ? { manage_key: manageKey } : {}) });
  assert.equal(res.status, 303, "redirects to Dodo's checkout");
  const session = dodo.sessions.at(-1);
  assert.equal(res.headers.get("Location"), `https://test.checkout.dodopayments.com/session/${session.session_id}`);
  return { session, ref: new URL(session.return_url).searchParams.get("ref") };
}

// The customer pays on Dodo's page: Dodo creates the subscription and its first payment.
function pay(session, { status = "active" } = {}) {
  const n = dodo.subs.size + 1;
  const subId = `sub_${String(n).padStart(14, "0")}`;
  const item = session.product_cart[0];
  dodo.subs.set(subId, {
    subscription_id: subId,
    product_id: item.product_id,
    quantity: item.quantity,
    status,
    customer: { customer_id: `cus_${n}`, email: `buyer${n}@example.com` },
    metadata: dodo.copyMetadata ? session.metadata : {},
    next_billing_date: iso(clock + 30 * 86400),
    cancel_at_next_billing_date: false,
    scheduled_change: null,
  });
  const paymentId = `pay_${String(n).padStart(14, "0")}`;
  dodo.payments.set(paymentId, {
    payment_id: paymentId,
    subscription_id: subId,
    checkout_session_id: session.session_id,
    status: "succeeded",
    total_amount: 118 * item.quantity,
    currency: "USD",
    created_at: iso(clock),
    refund_status: null,
    customer: { customer_id: `cus_${n}`, email: `buyer${n}@example.com`, name: `Buyer ${n}` },
  });
  return { subId, paymentId };
}

const returnTo = (ref, subId) => call(`success?ref=${ref}&subscription_id=${subId}&status=active`);

// Buys `quantity` seats end-to-end and returns the keys shown on the success page.
async function buy(quantity, manageKey) {
  const { session, ref } = await startCheckout(quantity, manageKey);
  const { subId, paymentId } = pay(session);
  const page = await (await returnTo(ref, subId)).text();
  return { activation: page.match(/HFXS(-[0-9A-Z]{5}){4}/)?.[0], manage: page.match(/HFXM(-[0-9A-Z]{5}){4}/)?.[0], subId, paymentId, ref, page };
}

const machine = (c, extra = {}) => ({ fingerprint: fp(c), instance_id: inst(c), hostname: `host-${c}`, os: "linux", version: "1.0.0", ...extra });

// Standard Webhooks, as Dodo signs them.
let webhookSeq = 0;
function signed(event, { key = WEBHOOK_KEY, ts = clock } = {}) {
  const body = JSON.stringify({ business_id: "bus_1", timestamp: iso(clock), ...event });
  const id = `msg_${++webhookSeq}`;
  const sig = createHmac("sha256", Buffer.from(key.replace(/^whsec_/, ""), "base64")).update(`${id}.${ts}.${body}`).digest("base64");
  return { method: "POST", body, headers: { "webhook-id": id, "webhook-timestamp": String(ts), "webhook-signature": `v1,${sig}` } };
}
const subEvent = (type, subId) => signed({ type, data: { subscription_id: subId, payload_type: "Subscription" } });

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

test("buy: checkout creates a session for N seats; the return page issues keys once", async () => {
  const keys = await buy(3);
  const session = dodo.sessions[0];
  assert.deepEqual(session.product_cart, [{ product_id: PRODUCT_ID, quantity: 3 }]);
  assert.equal(session.metadata.product, "server");
  assert.equal(session.metadata.ref, keys.ref);
  assert.equal(session.return_url, `https://license.buildcraft.town/api/server/success?ref=${keys.ref}`);
  assert.ok(keys.activation && keys.manage, "both keys shown");
  assert.match(keys.page, /shown only once/);

  const again = await (await returnTo(keys.ref, keys.subId)).text();
  assert.match(again, /already processed/);
  assert.doesNotMatch(again, /HFXS-/, "keys are never shown twice");

  const stored = await sql("SELECT * FROM licenses");
  assert.equal(stored.length, 1);
  assert.doesNotMatch(JSON.stringify(stored), new RegExp(keys.activation.replace(/-/g, "").slice(4)), "only hashes are stored");
  const [sub] = await sql("SELECT * FROM subscriptions");
  assert.equal(sub.quantity, 3);
  assert.equal(sub.customer_id, "cus_1");
  assert.equal(sub.current_end, clock + 30 * 86400);
});

test("buy: matched by the payment's checkout session when Dodo doesn't copy the metadata", async () => {
  dodo.copyMetadata = false;
  const keys = await buy(1);
  assert.ok(keys.activation, "keys shown");
});

test("buy: another checkout's return URL can't claim a subscription", async () => {
  const victim = await startCheckout(1);
  const { subId } = pay(victim.session);
  const attacker = await startCheckout(1);
  const res = await returnTo(attacker.ref, subId);
  assert.equal(res.status, 400);
  assert.doesNotMatch(await res.text(), /HFXS-/);
  assert.match(await (await returnTo(victim.ref, subId)).text(), /HFXS-/, "the real buyer still gets the keys");
  assert.equal((await returnTo("0".repeat(32), subId)).status, 400, "unknown checkout");
  assert.equal((await call(`success?ref=${victim.ref}&subscription_id=sub_NOPE0000000000`)).status, 400, "unknown subscription");
});

test("buy: other Dodo products are never adopted", async () => {
  const { session, ref } = await startCheckout(1);
  session.product_cart[0].product_id = "pdt_SOMETHINGELSE";
  const { subId } = pay(session);
  const res = await returnTo(ref, subId);
  assert.equal(res.status, 400);
  assert.doesNotMatch(await res.text(), /HFXS-/);
  await call("webhook", subEvent("subscription.active", subId));
  assert.equal((await sql("SELECT * FROM subscriptions")).length, 0);
});

test("buy: pending, failed and abandoned payments issue nothing", async () => {
  const { session, ref } = await startCheckout(1);
  const { subId } = pay(session, { status: "pending" });
  assert.match(await (await returnTo(ref, subId)).text(), /Payment processing/, "e-mandates confirm later");
  dodo.subs.get(subId).status = "failed";
  assert.match(await (await returnTo(ref, subId)).text(), /Payment not completed/);
  assert.match(await (await call(`success?ref=${ref}&status=failed`)).text(), /Payment not completed/, "no subscription at all");
  dodo.subs.get(subId).status = "active";
  assert.match(await (await returnTo(ref, subId)).text(), /HFXS-/, "issued once it goes through");
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
  const refreshPlan = async () => claims((await (await postJSON("refresh", { seat_id: seat.seat_id, seat_secret: seat.seat_secret, ...machine("a") })).json()).token);

  dodo.subs.get(subId).status = "past_due"; // grace period: still covered
  await call("webhook", subEvent("subscription.past_due", subId));
  assert.equal((await refreshPlan()).status, "active");

  dodo.subs.get(subId).status = "on_hold";
  await call("webhook", subEvent("subscription.on_hold", subId));
  let c = await refreshPlan();
  assert.equal(c.status, "inactive");
  assert.equal(c.plan, "lapsed");

  dodo.subs.get(subId).status = "active";
  dodo.subs.get(subId).next_billing_date = iso(clock + 60 * 86400);
  await call("webhook", subEvent("subscription.renewed", subId));
  c = await refreshPlan();
  assert.equal(c.status, "active");
  assert.equal((await sql("SELECT current_end FROM subscriptions WHERE id = ?", subId))[0].current_end, clock + 60 * 86400);
});

test("refresh: flags a seat used by two different machines", async () => {
  const { activation } = await buy(1);
  const seat = await (await postJSON("activate", { key: activation, ...machine("a") })).json();
  const res = await postJSON("refresh", { seat_id: seat.seat_id, seat_secret: seat.seat_secret, ...machine("a", { hostname: "clone" }) });
  assert.equal((await res.json()).duplicate_identity, true);
});

// --- manage -----------------------------------------------------------------

test("manage: lists servers, releases a seat, changes quantity, cancels, opens billing", async () => {
  const { activation, manage, subId } = await buy(2);
  const seat = await (await postJSON("activate", { key: activation, ...machine("a") })).json();
  const changePlan = () => dodoCalls.filter((c) => c.path.endsWith("/change-plan")).at(-1).body;

  assert.match(await (await postForm("manage", { manage_key: makeKey("HFXM") })).text(), /wasn&#39;t recognized|wasn't recognized/);
  let html = await (await postForm("manage", { manage_key: manage })).text();
  assert.match(html, /1<\/strong> of <strong>2<\/strong>/);
  assert.match(html, /host-a/);
  for (const action of ["quantity", "cancel", "billing"]) {
    assert.ok(html.includes(`name="action" value="${action}"><input type="hidden" name="target" value="${subId}">`), `${action} button targets the subscription`);
  }

  html = await (await postForm("manage", { manage_key: manage, action: "release", target: seat.seat_id })).text();
  assert.match(html, /Server released/);
  assert.match(html, /0<\/strong> of <strong>2/);

  // Increase: immediate, prorated for the rest of the cycle.
  html = await (await postForm("manage", { manage_key: manage, action: "quantity", target: subId, quantity: "5" })).text();
  assert.match(html, /Now 5 servers/);
  assert.deepEqual(changePlan(), { product_id: PRODUCT_ID, quantity: 5, proration_billing_mode: "prorated_immediately_keep_cycle", effective_at: "immediately", on_payment_failure: "prevent_change" });
  // Charge refused (e.g. below Dodo's minimum): applied now, billed from the next renewal.
  dodo.changeFails.push("TOTAL_PAYMENT_AMOUNT_BELOW_MINIMUM_AMOUNT");
  html = await (await postForm("manage", { manage_key: manage, action: "quantity", target: subId, quantity: "6" })).text();
  assert.match(html, /Now 6 servers/);
  assert.deepEqual(changePlan(), { product_id: PRODUCT_ID, quantity: 6, proration_billing_mode: "do_not_bill", effective_at: "immediately" });
  assert.match(html, /0<\/strong> of <strong>6/);
  // Decrease: at the next billing date; seats stay until then, even when Dodo syncs.
  html = await (await postForm("manage", { manage_key: manage, action: "quantity", target: subId, quantity: "2" })).text();
  assert.match(html, /at the next billing date/);
  assert.equal(changePlan().effective_at, "next_billing_date");
  await call("webhook", subEvent("subscription.updated", subId));
  assert.equal((await sql("SELECT quantity FROM subscriptions WHERE id = ?", subId))[0].quantity, 6);
  // Dodo refuses outright.
  dodo.changeFails.push("no", "no");
  html = await (await postForm("manage", { manage_key: manage, action: "quantity", target: subId, quantity: "9" })).text();
  assert.match(html, /can&#39;t be changed|can't be changed/);

  const billing = await postForm("manage", { manage_key: manage, action: "billing", target: subId });
  assert.equal(billing.status, 303);
  assert.match(billing.headers.get("Location"), /^https:\/\/test\.customer\.dodopayments\.com\/cus_1\?return=https%3A%2F%2Flicense\.buildcraft\.town%2Fapi%2Fserver%2Fmanage$/);

  html = await (await postForm("manage", { manage_key: manage, action: "cancel", target: subId })).text();
  assert.match(html, /Cancelled/);
  assert.deepEqual(dodoCalls.filter((c) => c.method === "PATCH").at(-1).body, { cancel_at_next_billing_date: true });
  assert.match(html, /class="badge warn">Cancels \d+ \w+ \d{4}</, "shown as cancelling, with the end date");
  assert.doesNotMatch(html, /value="cancel"/, "no second cancel button");
});

test("manage: buying more servers adds them to the same license", async () => {
  const first = await buy(1);
  const more = await buy(2, first.manage);
  assert.match(more.page, /2 servers added/);
  assert.equal(more.activation, undefined, "no new keys");
  for (const ch of "abcd") assert.equal((await postJSON("activate", { key: first.activation, ...machine(ch) })).status, 200, ch); // 3 seats + 1 spare
  assert.equal((await postJSON("activate", { key: first.activation, ...machine("e") })).status, 409);
});

test("manage: added servers count once paid, even if the return page never loads", async () => {
  const first = await buy(1);
  const { session, ref } = await startCheckout(2, first.manage);
  const { subId } = pay(session);
  await call("webhook", subEvent("subscription.active", subId));
  for (const ch of "abcd") assert.equal((await postJSON("activate", { key: first.activation, ...machine(ch) })).status, 200, ch);
  assert.match(await (await returnTo(ref, subId)).text(), /2 servers added/, "the late return page still confirms");
});

// --- recovery ---------------------------------------------------------------

test("recover: new keys with the receipt's payment id; activated servers keep working", async () => {
  const { activation, paymentId } = await buy(1);
  const seat = await (await postJSON("activate", { key: activation, ...machine("a") })).json();

  assert.match(await (await postForm("recover", { payment_id: "pay_FAKE0000000000" })).text(), /isn&#39;t a paid|isn't a paid/);
  assert.match(await (await postForm("recover", { payment_id: "nope" })).text(), /exactly as it appears/);
  const html = await (await postForm("recover", { payment_id: paymentId })).text();
  const fresh = html.match(/HFXS(-[0-9A-Z]{5}){4}/)[0];
  assert.notEqual(fresh, activation);

  assert.equal((await postJSON("activate", { key: activation, ...machine("b") })).status, 403, "old key revoked");
  assert.equal((await postJSON("activate", { key: fresh, ...machine("b") })).status, 200);
  assert.equal((await postJSON("refresh", { seat_id: seat.seat_id, seat_secret: seat.seat_secret, ...machine("a") })).status, 200, "existing seat unaffected");
});

test("recover: a paid checkout whose return page never loaded gets its keys", async () => {
  const { session } = await startCheckout(2);
  const { paymentId } = pay(session);
  const html = await (await postForm("recover", { payment_id: paymentId })).text();
  assert.match(html, /subscribed for 2 servers/);
  assert.match(html, /HFXS(-[0-9A-Z]{5}){4}/);
  dodo.payments.get(paymentId).status = "failed";
  assert.match(await (await postForm("recover", { payment_id: paymentId })).text(), /isn&#39;t a paid|isn't a paid/, "only succeeded payments count");
});

// --- refunds ----------------------------------------------------------------

const manageHtml = async (manage, fields = {}) => (await postForm("manage", { manage_key: manage, ...fields })).text();

test("refund request: offered within 7 days of a payment and emailed to us once; nothing else changes", async () => {
  const { manage, subId, paymentId, activation } = await buy(2);
  let html = await manageHtml(manage);
  assert.match(html, /Request a refund/);
  assert.ok(html.includes(`name="action" value="refund"><input type="hidden" name="target" value="${subId}">`));

  html = await manageHtml(manage, { action: "refund", target: subId, reason: "Bought the wrong number of servers" });
  assert.match(html, /Refund requested for the payment of/);
  assert.equal(emails.sent.length, 1);
  const e = emails.sent[0];
  assert.deepEqual(e.to, ["billing@buildcraft.town"]);
  assert.equal(e.from, "Hotfix Server <refunds@buildcraft.town>");
  assert.equal(e.reply_to, "buyer1@example.com", "replying goes to the customer");
  assert.equal(e.subject, `[TEST] Refund request: Hotfix Server, ${paymentId} (2.36 USD)`);
  assert.match(e.text, /Bought the wrong number of servers/);
  assert.match(e.text, new RegExp(`Subscription: ${subId}, 2 servers`));
  assert.match(e.idempotencyKey, /^rr_[0-9a-f]{24}$/);

  // Shown as requested; asking again sends nothing.
  html = await manageHtml(manage);
  assert.match(html, /Refund requested on/);
  assert.doesNotMatch(html, /Send refund request/);
  assert.match(await manageHtml(manage, { action: "refund", target: subId }), /already been requested/);
  assert.equal(emails.sent.length, 1);
  // A request alone changes nothing: no Dodo writes, the servers stay licensed.
  assert.equal(dodoCalls.filter((c) => c.method !== "GET" && c.path.startsWith("/subscriptions")).length, 0);
  assert.equal((await postJSON("activate", { key: activation, ...machine("a") })).status, 200);
});

test("refund request: not offered after 7 days, for refunded payments, or without the Resend variables", async () => {
  const { manage, subId, paymentId } = await buy(1);
  clock += 8 * 86400;
  assert.doesNotMatch(await manageHtml(manage), /Request a refund/);
  assert.match(await manageHtml(manage, { action: "refund", target: subId }), /no payment from the last 7 days/);
  clock -= 8 * 86400;

  dodo.payments.get(paymentId).refund_status = "full";
  assert.doesNotMatch(await manageHtml(manage), /Request a refund/);
  dodo.payments.get(paymentId).refund_status = null;

  delete env.RESEND_API_KEY;
  assert.doesNotMatch(await manageHtml(manage), /Request a refund/);
  assert.match(await manageHtml(manage, { action: "refund", target: subId }), /aren&#39;t available|aren't available/);
  assert.equal(emails.sent.length, 0);
});

test("refund request: if the email can't be sent, nothing is recorded and the buyer can retry", async () => {
  const { manage, subId } = await buy(1);
  emails.fail = true;
  assert.match(await manageHtml(manage, { action: "refund", target: subId }), /couldn&#39;t be sent|couldn't be sent/);
  assert.equal(Number((await sql("SELECT COUNT(*) n FROM refund_requests"))[0].n), 0);
  emails.fail = false;
  assert.match(await manageHtml(manage, { action: "refund", target: subId }), /Refund requested for/);
  assert.equal(Number((await sql("SELECT COUNT(*) n FROM refund_requests"))[0].n), 1);
});

test("refund: refunding the payment in Dodo ends the subscription at once; its servers stop being licensed", async () => {
  const { activation, subId, paymentId } = await buy(1);
  const seat = await (await postJSON("activate", { key: activation, ...machine("a") })).json();
  dodo.payments.get(paymentId).refund_status = "full";
  const refundEvent = () => signed({ type: "refund.succeeded", data: { refund_id: "ref_1", payment_id: paymentId, payload_type: "Refund" } });
  assert.equal((await call("webhook", refundEvent())).status, 200);

  const patches = () => dodoCalls.filter((c) => c.method === "PATCH" && c.path === `/subscriptions/${subId}`);
  assert.equal(patches().length, 1);
  assert.equal(patches()[0].body.status, "cancelled");
  assert.equal(dodo.subs.get(subId).status, "cancelled");
  assert.equal((await sql("SELECT status FROM subscriptions WHERE id = ?", subId))[0].status, "cancelled");
  const c = claims((await (await postJSON("refresh", { seat_id: seat.seat_id, seat_secret: seat.seat_secret, ...machine("a") })).json()).token);
  assert.equal(c.status, "inactive", "no seats, though the paid period hasn't ended");
  assert.equal((await postJSON("activate", { key: activation, ...machine("b") })).status, 403);

  await call("webhook", refundEvent()); // replayed
  assert.equal(patches().length, 1, "already cancelled: not cancelled again");
});

test("refund: events for payments that aren't refunded or aren't ours are ignored", async () => {
  const { subId, paymentId } = await buy(1);
  assert.equal((await call("webhook", signed({ type: "refund.succeeded", data: { payment_id: paymentId } }))).status, 200);
  assert.equal((await call("webhook", signed({ type: "refund.succeeded", data: { payment_id: "pay_UNKNOWN0000000" } }))).status, 200);
  assert.equal(dodoCalls.filter((c) => c.method === "PATCH").length, 0);
  assert.equal(dodo.subs.get(subId).status, "active");
});

// --- webhook ----------------------------------------------------------------

test("webhook: Standard Webhooks signature checked; state re-fetched from Dodo", async () => {
  const { subId } = await buy(1);
  const ev = { type: "subscription.cancelled", data: { subscription_id: subId, status: "cancelled" } };
  assert.equal((await call("webhook", signed(ev, { key: "whsec_" + Buffer.from("wrong").toString("base64") }))).status, 400);
  assert.equal((await call("webhook", signed(ev, { ts: clock - 600 }))).status, 400, "stale timestamp (replay)");
  assert.equal((await call("webhook", { method: "POST", body: JSON.stringify(ev) })).status, 400, "unsigned");
  const multi = signed(ev);
  multi.headers["webhook-signature"] = `v1,AAAA ${multi.headers["webhook-signature"]}`;
  assert.equal((await call("webhook", multi)).status, 200, "any matching v1 signature is accepted");
  // The event says cancelled, Dodo says active: Dodo wins.
  assert.equal((await sql("SELECT status FROM subscriptions WHERE id = ?", subId))[0].status, "active");
  // Unknown subscriptions, and payment events, are ignored.
  assert.equal((await call("webhook", subEvent("subscription.active", "sub_UNKNOWN0000000"))).status, 200);
  assert.equal((await call("webhook", signed({ type: "payment.succeeded", data: { payment_id: "pay_1", subscription_id: subId } }))).status, 200);
});

test("webhook: matches the Standard Webhooks specification's test vector", async () => {
  // From github.com/standard-webhooks/standard-webhooks, independent of signed() above.
  clock = 1614265330;
  const headers = new Headers({ "webhook-id": "msg_p5jXN8AQM9LWM0D4loKWxJek", "webhook-timestamp": "1614265330", "webhook-signature": "v1,g0hM9SsE+OTPJTGt/tmIKtSyZlE3uFJELVlNIOLJ1OE=" });
  assert.equal(await webhookSignatureValid("whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw", headers, '{"test": 2432232314}'), true);
  assert.equal(await webhookSignatureValid("whsec_MfKQ9r8GKYqrTwjUPD8ILPZIo2LaLaSw", headers, '{"test": 2432232315}'), false);
});

test("billing off: no Dodo variables -> trials and activation work, purchases don't", async () => {
  for (const k of ["DODO_PAYMENTS_API_KEY", "DODO_PAYMENTS_WEBHOOK_KEY", "DODO_SERVER_PRODUCT_ID", "DODO_PAYMENTS_ENVIRONMENT"]) delete env[k];
  const buyRes = await call("buy");
  assert.equal(buyRes.status, 200);
  const html = await buyRes.text();
  assert.match(html, /\$1 per server per month/);
  assert.match(html, /Purchases open soon/);
  assert.doesNotMatch(html, /checkout/);
  for (const [path, init] of [["checkout", { method: "POST" }], [`success?ref=${"0".repeat(32)}`, {}], ["manage", {}], ["manage", { method: "POST" }], ["recover", {}], ["webhook", { method: "POST" }]]) {
    assert.equal((await call(path, init)).status, 503, path);
  }
  assert.equal(dodoCalls.length, 0);
  const t = await postJSON("trial", { fingerprint: fp("a") });
  assert.equal(t.status, 200);
  assert.equal(claims((await t.json()).token).plan, "trial");
  const a = await postJSON("activate", { key: "HFXS-AAAAA-AAAAA-AAAAA-AAAAA", ...machine("b") });
  assert.notEqual(a.status, 500);
});

test("routing", async () => {
  assert.equal((await call("nope")).status, 404);
  assert.equal((await call("activate")).status, 405);
  assert.match(await (await call("buy")).text(), /\$1 per server per month/);
});
