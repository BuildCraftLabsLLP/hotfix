// Tests for the licensing API. No dependencies — run with:
//   node --test worker/test/license.test.mjs
// KV and the Stripe API are faked in-process; nothing touches the network.
import { test, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";
import { createHmac, generateKeyPairSync, verify as edVerify } from "node:crypto";

import worker from "../src/worker.js";
import { verifyStripeSignature } from "../src/license.js";

const FP = "a".repeat(64);
const FP2 = "b".repeat(64);
const WEBHOOK_SECRET = "whsec_test";

const { publicKey, privateKey } = generateKeyPairSync("ed25519");

function fakeKV() {
  const m = new Map();
  return {
    m,
    async get(k, type) {
      const v = m.get(k);
      if (v === undefined) return null;
      return type === "json" ? JSON.parse(v) : v;
    },
    async put(k, v) {
      m.set(k, v);
    },
  };
}

let env;
let stripeCalls;
let stripeReplies;
const realFetch = globalThis.fetch;

beforeEach(() => {
  env = {
    HOTFIX_LICENSE: fakeKV(),
    STRIPE_SECRET_KEY: "sk_test_secret",
    STRIPE_WEBHOOK_SECRET: WEBHOOK_SECRET,
    LICENSE_SIGNING_KEY: privateKey.export({ type: "pkcs8", format: "der" }).toString("base64"),
  };
  stripeCalls = [];
  stripeReplies = {};
  globalThis.fetch = async (url, init = {}) => {
    const u = new URL(url);
    assert.equal(u.origin, "https://api.stripe.com", "only Stripe may be called");
    assert.equal(init.headers.Authorization, "Bearer sk_test_secret");
    stripeCalls.push({ method: init.method, path: u.pathname, search: u.search, body: new URLSearchParams(init.body || "") });
    const reply = stripeReplies[`${init.method} ${u.pathname}`];
    if (!reply) return new Response(JSON.stringify({ error: { message: "no such thing" } }), { status: 404 });
    return new Response(JSON.stringify(reply), { status: 200 });
  };
});

afterEach(() => {
  globalThis.fetch = realFetch;
});

const call = (path, init) => worker.fetch(new Request(`https://hotfix.buildcraft.town${path}`, init), env, {});

function decodeToken(token) {
  const [body, sig] = token.split(".");
  assert.ok(
    edVerify(null, Buffer.from(body), publicKey, Buffer.from(sig, "base64url")),
    "token signature must verify with the public key",
  );
  return JSON.parse(Buffer.from(body, "base64url").toString());
}

function signedWebhook(event, { secret = WEBHOOK_SECRET, t = Math.floor(Date.now() / 1000) } = {}) {
  const body = JSON.stringify(event);
  const v1 = createHmac("sha256", secret).update(`${t}.${body}`).digest("hex");
  return { method: "POST", body, headers: { "Stripe-Signature": `t=${t},v1=${v1}` } };
}

const sub = (over = {}) => ({
  id: "sub_1",
  customer: "cus_1",
  status: "active",
  current_period_end: 2000000000,
  metadata: { fingerprint: FP },
  ...over,
});

test("verify: unknown machine gets a signed inactive token", async () => {
  const res = await call(`/license/verify?fp=${FP}`);
  assert.equal(res.status, 200);
  const claims = decodeToken((await res.json()).token);
  assert.equal(claims.fp, FP);
  assert.equal(claims.status, "inactive");
  assert.ok(claims.exp > claims.iat);
});

test("verify: rejects malformed fingerprints", async () => {
  for (const fp of ["", "abc", "A".repeat(64), "a".repeat(63), "a".repeat(64) + "/x"]) {
    const res = await call(`/license/verify?fp=${encodeURIComponent(fp)}`);
    assert.equal(res.status, 400, `fp=${fp}`);
  }
});

test("verify: active subscription is bound to its own machine only", async () => {
  await env.HOTFIX_LICENSE.put(`lic:${FP}`, JSON.stringify({ subscription_id: "sub_1", status: "active", current_period_end: 2000000000 }));

  const mine = decodeToken((await (await call(`/license/verify?fp=${FP}`)).json()).token);
  assert.equal(mine.status, "active");
  assert.equal(mine.period_end, 2000000000);

  const other = decodeToken((await (await call(`/license/verify?fp=${FP2}`)).json()).token);
  assert.equal(other.status, "inactive");
  assert.equal(other.fp, FP2);
});

test("verify: canceled / past_due / unpaid are inactive", async () => {
  for (const status of ["canceled", "past_due", "unpaid", "incomplete", "incomplete_expired", "paused"]) {
    await env.HOTFIX_LICENSE.put(`lic:${FP}`, JSON.stringify({ subscription_id: "sub_1", status }));
    const claims = decodeToken((await (await call(`/license/verify?fp=${FP}`)).json()).token);
    assert.equal(claims.status, "inactive", status);
  }
});

test("checkout: creates a $1/month subscription session tied to the fingerprint", async () => {
  stripeReplies["POST /v1/checkout/sessions"] = { url: "https://checkout.stripe.com/c/pay/cs_test_123" };
  const res = await call(`/license/checkout?fp=${FP}`);
  assert.equal(res.status, 303);
  assert.equal(res.headers.get("Location"), "https://checkout.stripe.com/c/pay/cs_test_123");

  const body = stripeCalls[0].body;
  assert.equal(body.get("mode"), "subscription");
  assert.equal(body.get("client_reference_id"), FP);
  assert.equal(body.get("subscription_data[metadata][fingerprint]"), FP);
  assert.equal(body.get("line_items[0][quantity]"), "1");
  assert.equal(body.get("line_items[0][price_data][unit_amount]"), "100");
  assert.equal(body.get("line_items[0][price_data][currency]"), "usd");
  assert.equal(body.get("line_items[0][price_data][recurring][interval]"), "month");
  assert.match(body.get("success_url"), /\/license\/success\?session_id=\{CHECKOUT_SESSION_ID\}$/);
});

test("checkout: uses STRIPE_PRICE_ID when configured", async () => {
  env.STRIPE_PRICE_ID = "price_abc";
  stripeReplies["POST /v1/checkout/sessions"] = { url: "https://checkout.stripe.com/x" };
  await call(`/license/checkout?fp=${FP}`);
  const body = stripeCalls[0].body;
  assert.equal(body.get("line_items[0][price]"), "price_abc");
  assert.equal(body.get("line_items[0][price_data][unit_amount]"), null);
});

test("checkout: an already-subscribed machine is not charged again", async () => {
  await env.HOTFIX_LICENSE.put(`lic:${FP}`, JSON.stringify({ subscription_id: "sub_1", status: "active" }));
  const res = await call(`/license/checkout?fp=${FP}`);
  assert.equal(res.status, 200);
  assert.match(await res.text(), /Already subscribed/);
  assert.equal(stripeCalls.length, 0);
});

test("success: a completed session activates the machine", async () => {
  stripeReplies["GET /v1/checkout/sessions/cs_test_abc"] = { status: "complete", client_reference_id: FP, subscription: sub() };
  const res = await call("/license/success?session_id=cs_test_abc");
  assert.equal(res.status, 200);
  const rec = await env.HOTFIX_LICENSE.get(`lic:${FP}`, "json");
  assert.equal(rec.status, "active");
  assert.equal(rec.subscription_id, "sub_1");
  assert.equal(rec.customer_id, "cus_1");
});

test("success: an unpaid/open session activates nothing", async () => {
  stripeReplies["GET /v1/checkout/sessions/cs_test_abc"] = { status: "open", client_reference_id: FP, subscription: null };
  await call("/license/success?session_id=cs_test_abc");
  assert.equal(await env.HOTFIX_LICENSE.get(`lic:${FP}`), null);

  const bad = await call("/license/success?session_id=../../customers");
  assert.equal(bad.status, 400);
});

test("webhook: rejects missing, wrong, and stale signatures", async () => {
  const event = { type: "customer.subscription.updated", data: { object: sub() } };

  let res = await call("/license/webhook", { method: "POST", body: JSON.stringify(event) });
  assert.equal(res.status, 400);

  res = await call("/license/webhook", signedWebhook(event, { secret: "whsec_wrong" }));
  assert.equal(res.status, 400);

  res = await call("/license/webhook", signedWebhook(event, { t: Math.floor(Date.now() / 1000) - 3600 }));
  assert.equal(res.status, 400);

  assert.equal(await env.HOTFIX_LICENSE.get(`lic:${FP}`), null, "nothing may be written on a bad signature");
});

test("webhook: a tampered body fails verification", async () => {
  const init = signedWebhook({ type: "customer.subscription.updated", data: { object: sub() } });
  init.body = init.body.replace(FP, FP2);
  const res = await call("/license/webhook", init);
  assert.equal(res.status, 400);
});

test("webhook: subscription lifecycle activates then deactivates the machine", async () => {
  let res = await call("/license/webhook", signedWebhook({ type: "customer.subscription.created", data: { object: sub() } }));
  assert.equal(res.status, 200);
  assert.equal((await env.HOTFIX_LICENSE.get(`lic:${FP}`, "json")).status, "active");

  await call("/license/webhook", signedWebhook({ type: "customer.subscription.updated", data: { object: sub({ status: "past_due" }) } }));
  assert.equal((await env.HOTFIX_LICENSE.get(`lic:${FP}`, "json")).status, "past_due");

  await call("/license/webhook", signedWebhook({ type: "customer.subscription.deleted", data: { object: sub({ status: "canceled" }) } }));
  const claims = decodeToken((await (await call(`/license/verify?fp=${FP}`)).json()).token);
  assert.equal(claims.status, "inactive");
});

test("webhook: checkout.session.completed fetches the subscription and activates", async () => {
  stripeReplies["GET /v1/subscriptions/sub_1"] = sub();
  const res = await call(
    "/license/webhook",
    signedWebhook({ type: "checkout.session.completed", data: { object: { mode: "subscription", client_reference_id: FP, subscription: "sub_1" } } }),
  );
  assert.equal(res.status, 200);
  assert.equal((await env.HOTFIX_LICENSE.get(`lic:${FP}`, "json")).status, "active");
});

test("webhook: an old ended subscription can't deactivate a newer active one", async () => {
  await call("/license/webhook", signedWebhook({ type: "customer.subscription.created", data: { object: sub({ id: "sub_new" }) } }));
  await call("/license/webhook", signedWebhook({ type: "customer.subscription.deleted", data: { object: sub({ id: "sub_old", status: "canceled" }) } }));
  const rec = await env.HOTFIX_LICENSE.get(`lic:${FP}`, "json");
  assert.equal(rec.subscription_id, "sub_new");
  assert.equal(rec.status, "active");
});

test("webhook: reads the period end from the subscription item (newer API versions)", async () => {
  const s = sub({ current_period_end: undefined, items: { data: [{ current_period_end: 1900000000 }] } });
  await call("/license/webhook", signedWebhook({ type: "customer.subscription.updated", data: { object: s } }));
  assert.equal((await env.HOTFIX_LICENSE.get(`lic:${FP}`, "json")).current_period_end, 1900000000);
});

test("webhook: ignores subscriptions without a valid fingerprint", async () => {
  const res = await call("/license/webhook", signedWebhook({ type: "customer.subscription.updated", data: { object: sub({ metadata: {} }) } }));
  assert.equal(res.status, 200);
  assert.equal(env.HOTFIX_LICENSE.m.size, 0);
});

test("portal: redirects a subscribed machine to the Stripe billing portal", async () => {
  await env.HOTFIX_LICENSE.put(`lic:${FP}`, JSON.stringify({ subscription_id: "sub_1", customer_id: "cus_1", status: "active" }));
  stripeReplies["POST /v1/billing_portal/sessions"] = { url: "https://billing.stripe.com/p/session/x" };
  const res = await call(`/license/portal?fp=${FP}`);
  assert.equal(res.status, 303);
  assert.equal(stripeCalls[0].body.get("customer"), "cus_1");

  const none = await call(`/license/portal?fp=${FP2}`);
  assert.equal(none.status, 200);
  assert.match(await none.text(), /No subscription found/);
});

test("errors never leak Stripe details or secrets", async () => {
  // No canned reply -> the fake Stripe returns an error.
  const res = await call(`/license/checkout?fp=${FP}`);
  assert.equal(res.status, 500);
  const body = await res.text();
  assert.equal(body, "License service error");
});

test("routing: unknown paths and wrong methods", async () => {
  assert.equal((await call("/license/nope")).status, 404);
  assert.equal((await call(`/license/verify?fp=${FP}`, { method: "POST" })).status, 405);
  assert.equal((await call("/license/webhook")).status, 405);
});

test("verifyStripeSignature: accepts any matching v1 among several", async () => {
  const t = 1700000000;
  const body = "{}";
  const good = createHmac("sha256", WEBHOOK_SECRET).update(`${t}.${body}`).digest("hex");
  assert.equal(await verifyStripeSignature(body, `t=${t},v1=${"0".repeat(64)},v1=${good}`, WEBHOOK_SECRET, t), true);
  assert.equal(await verifyStripeSignature(body, `t=${t},v1=${good}`, "", t), false, "an unset secret must never verify");
  assert.equal(await verifyStripeSignature(body, `v1=${good}`, WEBHOOK_SECRET, t), false);
});
