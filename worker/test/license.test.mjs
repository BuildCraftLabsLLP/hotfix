// Tests for the licensing API. No dependencies — run with:
//   node --test worker/test/license.test.mjs
// KV and the Razorpay API are faked in-process; nothing touches the network.
import { test, beforeEach, afterEach } from "node:test";
import assert from "node:assert/strict";
import { createHmac, generateKeyPairSync, verify as edVerify } from "node:crypto";

import worker from "../src/worker.js";
import { verifyRazorpaySignature } from "../src/license.js";

const FP = "a".repeat(64);
const FP2 = "b".repeat(64);
const KEY_ID = "rzp_test_key";
const KEY_SECRET = "rzp_test_secret";
const WEBHOOK_SECRET = "webhook_test_secret";
const PLAN = "plan_00000000000001";

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
let rzpCalls;
let rzpReplies;
const realFetch = globalThis.fetch;

beforeEach(() => {
  env = {
    HOTFIX_LICENSE: fakeKV(),
    RAZORPAY_KEY_ID: KEY_ID,
    RAZORPAY_KEY_SECRET: KEY_SECRET,
    RAZORPAY_WEBHOOK_SECRET: WEBHOOK_SECRET,
    RAZORPAY_PLAN_ID: PLAN,
    LICENSE_SIGNING_KEY: privateKey.export({ type: "pkcs8", format: "der" }).toString("base64"),
  };
  rzpCalls = [];
  rzpReplies = {};
  globalThis.fetch = async (url, init = {}) => {
    const u = new URL(url);
    assert.equal(u.origin, "https://api.razorpay.com", "only Razorpay may be called");
    assert.equal(init.headers.Authorization, `Basic ${Buffer.from(`${KEY_ID}:${KEY_SECRET}`).toString("base64")}`);
    rzpCalls.push({ method: init.method, path: u.pathname, body: init.body ? JSON.parse(init.body) : null });
    const reply = rzpReplies[`${init.method} ${u.pathname}`];
    if (!reply) {
      return new Response(JSON.stringify({ error: { code: "BAD_REQUEST_ERROR", description: "no such thing" } }), { status: 400 });
    }
    return new Response(JSON.stringify(typeof reply === "function" ? reply(init) : reply), { status: 200 });
  };
});

afterEach(() => {
  globalThis.fetch = realFetch;
});

const call = (path, init) => worker.fetch(new Request(`https://hotfix.buildcraft.town${path}`, init), env, {});
const now = () => Math.floor(Date.now() / 1000);
const hmac = (secret, msg) => createHmac("sha256", secret).update(msg).digest("hex");

function decodeToken(token) {
  const [body, sig] = token.split(".");
  assert.ok(
    edVerify(null, Buffer.from(body), publicKey, Buffer.from(sig, "base64url")),
    "token signature must verify with the public key",
  );
  return JSON.parse(Buffer.from(body, "base64url").toString());
}

const verifyClaims = async (fp) => decodeToken((await (await call(`/license/verify?fp=${fp}`)).json()).token);

// Puts a machine's free trial `daysAgo` days in the past.
async function trialStarted(fp, daysAgo) {
  await env.HOTFIX_LICENSE.put(`trial:${fp}`, JSON.stringify({ start: now() - daysAgo * 86400 }));
}

// A Razorpay subscription entity.
const sub = (over = {}) => ({
  id: "sub_1",
  entity: "subscription",
  plan_id: PLAN,
  customer_id: "cust_1",
  status: "active",
  current_end: 2000000000,
  charge_at: 2000000000,
  has_scheduled_changes: false,
  notes: { fingerprint: FP },
  ...over,
});

function signedWebhook(event, { secret = WEBHOOK_SECRET } = {}) {
  const body = JSON.stringify(event);
  return { method: "POST", body, headers: { "X-Razorpay-Signature": hmac(secret, body) } };
}

const subEvent = (name, entity = sub()) => ({
  entity: "event",
  event: name,
  payload: { subscription: { entity } },
  created_at: now(),
});

// Checkout's form POST to the callback URL.
function paymentCallback(fp, { paymentId = "pay_1", subId = "sub_1", signature } = {}) {
  const form = new URLSearchParams({
    razorpay_payment_id: paymentId,
    razorpay_subscription_id: subId,
    razorpay_signature: signature ?? hmac(KEY_SECRET, `${paymentId}|${subId}`),
  });
  return call(`/license/success?fp=${fp}`, {
    method: "POST",
    body: form.toString(),
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
  });
}

// --- verify / trial ---

test("verify: a new machine starts a 7-day free trial", async () => {
  const res = await call(`/license/verify?fp=${FP}`);
  assert.equal(res.status, 200);
  const claims = decodeToken((await res.json()).token);
  assert.equal(claims.fp, FP);
  assert.equal(claims.status, "active");
  assert.equal(claims.plan, "trial");
  assert.ok(Math.abs(claims.trial_end - (now() + 7 * 86400)) <= 2, "trial ends 7 days from first check-in");
  assert.ok(claims.exp > claims.iat && claims.exp <= claims.trial_end);
  assert.ok(await env.HOTFIX_LICENSE.get(`trial:${FP}`), "the trial start is recorded");
});

test("verify: the trial isn't restarted by later check-ins", async () => {
  await trialStarted(FP, 3);
  const claims = await verifyClaims(FP);
  assert.equal(claims.plan, "trial");
  assert.ok(Math.abs(claims.trial_end - (now() + 4 * 86400)) <= 2, "4 days left");
});

test("verify: a trial token never outlives the trial", async () => {
  await trialStarted(FP, 7 - 1 / 24); // one hour left
  const claims = await verifyClaims(FP);
  assert.equal(claims.status, "active");
  assert.ok(claims.exp <= claims.trial_end, "offline grace is capped at the trial's end");
  assert.ok(claims.exp - claims.iat <= 3600 + 2);
});

test("verify: an expired trial locks the machine", async () => {
  await trialStarted(FP, 8);
  const claims = await verifyClaims(FP);
  assert.equal(claims.status, "inactive");
  assert.equal(claims.plan, "trial_expired");
});

test("verify: TRIAL_DAYS sets the trial length", async () => {
  env.TRIAL_DAYS = "14";
  await trialStarted(FP, 10);
  const claims = await verifyClaims(FP);
  assert.equal(claims.plan, "trial");
  assert.ok(Math.abs(claims.trial_end - (now() + 4 * 86400)) <= 2);
});

test("verify: rejects malformed fingerprints", async () => {
  for (const fp of ["", "abc", "A".repeat(64), "a".repeat(63), "a".repeat(64) + "/x"]) {
    const res = await call(`/license/verify?fp=${encodeURIComponent(fp)}`);
    assert.equal(res.status, 400, `fp=${fp}`);
  }
});

test("verify: active subscription is bound to its own machine only", async () => {
  await env.HOTFIX_LICENSE.put(`lic:${FP}`, JSON.stringify({ subscription_id: "sub_1", status: "active", current_period_end: 2000000000 }));

  const mine = await verifyClaims(FP);
  assert.equal(mine.status, "active");
  assert.equal(mine.plan, "subscription");
  assert.equal(mine.period_end, 2000000000);

  // The subscription doesn't carry over: another machine only gets its own trial.
  const other = await verifyClaims(FP2);
  assert.equal(other.plan, "trial");
  assert.equal(other.fp, FP2);
});

test("verify: a subscription set up during the trial (authenticated) unlocks", async () => {
  await trialStarted(FP, 30);
  await env.HOTFIX_LICENSE.put(`lic:${FP}`, JSON.stringify({ subscription_id: "sub_1", status: "authenticated" }));
  const claims = await verifyClaims(FP);
  assert.equal(claims.status, "active");
  assert.equal(claims.plan, "subscription");
});

test("verify: created / pending / halted / cancelled are inactive once the trial is over", async () => {
  await trialStarted(FP, 30);
  for (const status of ["created", "pending", "halted", "cancelled", "completed", "expired", "paused"]) {
    await env.HOTFIX_LICENSE.put(`lic:${FP}`, JSON.stringify({ subscription_id: "sub_1", status, current_period_end: now() - 86400 }));
    const claims = await verifyClaims(FP);
    assert.equal(claims.status, "inactive", status);
    assert.equal(claims.plan, "lapsed", status);
  }
});

test("verify: cancelled during the trial, before any charge, ends as an expired trial", async () => {
  // The subscription's only date is its never-taken first charge, at the trial's end.
  await trialStarted(FP, 8);
  const trialEnd = now() - 86400;
  await env.HOTFIX_LICENSE.put(`lic:${FP}`, JSON.stringify({ subscription_id: "sub_1", status: "cancelled", current_period_end: trialEnd }));
  const claims = await verifyClaims(FP);
  assert.equal(claims.status, "inactive");
  assert.equal(claims.plan, "trial_expired");
});

// --- checkout ---

test("checkout: creates a subscription on the plan, tied to the fingerprint, and opens Razorpay Checkout", async () => {
  rzpReplies["POST /v1/subscriptions"] = { id: "sub_new1", status: "created" };
  const res = await call(`/license/checkout?fp=${FP}`);
  assert.equal(res.status, 200);
  const page = await res.text();

  const body = rzpCalls[0].body;
  assert.equal(body.plan_id, PLAN);
  assert.equal(body.quantity, 1);
  assert.equal(body.total_count, 120);
  assert.deepEqual(body.notes, { fingerprint: FP });
  // A machine still in its free week isn't charged until the week is over.
  assert.ok(Math.abs(body.start_at - (now() + 7 * 86400)) <= 2, "first charge at the end of the free trial");

  assert.equal(await env.HOTFIX_LICENSE.get(`checkout:${FP}`), "sub_new1");
  assert.match(page, /checkout\.razorpay\.com\/v1\/checkout\.js/);
  assert.match(page, /"subscription_id":"sub_new1"/);
  assert.match(page, new RegExp(`"key":"${KEY_ID}"`));
  assert.match(page, new RegExp(`/license/success\\?fp=${FP}`));
  assert.match(page, /Nothing is charged today/);
  assert.doesNotMatch(page, new RegExp(KEY_SECRET), "the key secret never reaches the browser");
});

test("checkout: after the trial, the subscription starts right away", async () => {
  rzpReplies["POST /v1/subscriptions"] = { id: "sub_new1", status: "created" };
  await trialStarted(FP, 20);
  const res = await call(`/license/checkout?fp=${FP}`);
  assert.equal(rzpCalls[0].body.start_at, undefined);
  assert.match(await res.text(), /\$1 per month/);

  rzpCalls.length = 0;
  await trialStarted(FP, 7 - 1 / 48); // half an hour left: too close to defer
  await call(`/license/checkout?fp=${FP}`);
  assert.equal(rzpCalls[0].body.start_at, undefined);
});

test("checkout: an already-subscribed machine is not charged again", async () => {
  await env.HOTFIX_LICENSE.put(`lic:${FP}`, JSON.stringify({ subscription_id: "sub_1", status: "active" }));
  const res = await call(`/license/checkout?fp=${FP}`);
  assert.equal(res.status, 200);
  assert.match(await res.text(), /Already subscribed/);
  assert.equal(rzpCalls.length, 0);
});

// --- success (Checkout callback) ---

test("success: a verified payment activates the machine", async () => {
  await env.HOTFIX_LICENSE.put(`checkout:${FP}`, "sub_1");
  rzpReplies["GET /v1/subscriptions/sub_1"] = sub();
  const res = await paymentCallback(FP);
  assert.equal(res.status, 200);
  assert.match(await res.text(), /You're subscribed/);
  const rec = await env.HOTFIX_LICENSE.get(`lic:${FP}`, "json");
  assert.equal(rec.status, "active");
  assert.equal(rec.subscription_id, "sub_1");
  assert.equal(rec.customer_id, "cust_1");
  assert.equal(rec.current_period_end, 2000000000);
});

test("success: subscribing during the trial says when the first payment is", async () => {
  const chargeAt = now() + 5 * 86400;
  await env.HOTFIX_LICENSE.put(`checkout:${FP}`, "sub_1");
  rzpReplies["GET /v1/subscriptions/sub_1"] = sub({ status: "authenticated", current_end: null, charge_at: chargeAt });
  const html = await (await paymentCallback(FP)).text();
  assert.match(html, /free trial continues/);
  assert.match(html, new RegExp(new Date(chargeAt * 1000).toUTCString().slice(5, 16)));
  assert.equal((await env.HOTFIX_LICENSE.get(`lic:${FP}`, "json")).status, "authenticated");
});

test("success: a bad signature or another machine's subscription activates nothing", async () => {
  await env.HOTFIX_LICENSE.put(`checkout:${FP}`, "sub_1");
  rzpReplies["GET /v1/subscriptions/sub_1"] = sub();

  // Forged signature.
  let res = await paymentCallback(FP, { signature: "0".repeat(64) });
  assert.equal(res.status, 400);
  // Signed for a different subscription than the one created for this machine.
  res = await paymentCallback(FP, { subId: "sub_other" });
  assert.equal(res.status, 400);
  assert.equal(await env.HOTFIX_LICENSE.get(`lic:${FP}`), null);

  // A genuine payment whose subscription belongs to another machine.
  rzpReplies["GET /v1/subscriptions/sub_1"] = sub({ notes: { fingerprint: FP2 } });
  res = await paymentCallback(FP);
  assert.match(await res.text(), /Payment not completed/);
  assert.equal(await env.HOTFIX_LICENSE.get(`lic:${FP}`), null);
});

test("success: no checkout or a failed payment activates nothing", async () => {
  let res = await paymentCallback(FP); // no checkout:<fp> recorded
  assert.match(await res.text(), /Payment not completed/);

  await env.HOTFIX_LICENSE.put(`checkout:${FP}`, "sub_1");
  res = await call(`/license/success?fp=${FP}`, {
    method: "POST",
    body: "error%5Bcode%5D=BAD_REQUEST_ERROR",
    headers: { "Content-Type": "application/x-www-form-urlencoded" },
  });
  assert.match(await res.text(), /Payment not completed/);
  assert.equal(rzpCalls.length, 0);
  assert.equal(await env.HOTFIX_LICENSE.get(`lic:${FP}`), null);
});

// --- webhook ---

test("webhook: rejects missing and wrong signatures", async () => {
  const event = subEvent("subscription.activated");
  let res = await call("/license/webhook", { method: "POST", body: JSON.stringify(event) });
  assert.equal(res.status, 400);
  res = await call("/license/webhook", signedWebhook(event, { secret: "wrong" }));
  assert.equal(res.status, 400);
  assert.equal(rzpCalls.length, 0);
  assert.equal(await env.HOTFIX_LICENSE.get(`lic:${FP}`), null, "nothing may be written on a bad signature");
});

test("webhook: a tampered body fails verification", async () => {
  const init = signedWebhook(subEvent("subscription.activated"));
  init.body = init.body.replace(FP, FP2);
  const res = await call("/license/webhook", init);
  assert.equal(res.status, 400);
});

test("webhook: subscription lifecycle activates then deactivates the machine", async () => {
  rzpReplies["GET /v1/subscriptions/sub_1"] = sub();
  let res = await call("/license/webhook", signedWebhook(subEvent("subscription.activated")));
  assert.equal(res.status, 200);
  assert.equal((await env.HOTFIX_LICENSE.get(`lic:${FP}`, "json")).status, "active");

  rzpReplies["GET /v1/subscriptions/sub_1"] = sub({ status: "pending" });
  await call("/license/webhook", signedWebhook(subEvent("subscription.pending")));
  assert.equal((await env.HOTFIX_LICENSE.get(`lic:${FP}`, "json")).status, "pending");

  rzpReplies["GET /v1/subscriptions/sub_1"] = sub({ status: "cancelled" });
  await call("/license/webhook", signedWebhook(subEvent("subscription.cancelled")));
  await trialStarted(FP, 30);
  assert.equal((await verifyClaims(FP)).status, "inactive");
});

test("webhook: uses Razorpay's current state, not the event's (replays and reordering are harmless)", async () => {
  // A replayed old "cancelled" event, while the subscription is active again.
  rzpReplies["GET /v1/subscriptions/sub_1"] = sub({ status: "active" });
  await call("/license/webhook", signedWebhook(subEvent("subscription.cancelled", sub({ status: "cancelled" }))));
  assert.equal((await env.HOTFIX_LICENSE.get(`lic:${FP}`, "json")).status, "active");
});

test("webhook: an old ended subscription can't deactivate a newer active one", async () => {
  rzpReplies["GET /v1/subscriptions/sub_new"] = sub({ id: "sub_new" });
  rzpReplies["GET /v1/subscriptions/sub_old"] = sub({ id: "sub_old", status: "cancelled" });
  await call("/license/webhook", signedWebhook(subEvent("subscription.activated", sub({ id: "sub_new" }))));
  await call("/license/webhook", signedWebhook(subEvent("subscription.cancelled", sub({ id: "sub_old" }))));
  const rec = await env.HOTFIX_LICENSE.get(`lic:${FP}`, "json");
  assert.equal(rec.subscription_id, "sub_new");
  assert.equal(rec.status, "active");
});

test("webhook: ignores other events and subscriptions without a valid fingerprint", async () => {
  rzpReplies["GET /v1/subscriptions/sub_1"] = sub({ notes: {} });
  let res = await call("/license/webhook", signedWebhook(subEvent("subscription.activated")));
  assert.equal(res.status, 200);
  res = await call("/license/webhook", signedWebhook({ event: "payment.captured", payload: { payment: { entity: { id: "pay_1" } } } }));
  assert.equal(res.status, 200);
  assert.equal(env.HOTFIX_LICENSE.m.size, 0);
});

// --- manage / cancel ---

test("portal: shows the subscription with a cancel button", async () => {
  await env.HOTFIX_LICENSE.put(`lic:${FP}`, JSON.stringify({ subscription_id: "sub_1", status: "active" }));
  rzpReplies["GET /v1/subscriptions/sub_1"] = sub();
  const res = await call(`/license/portal?fp=${FP}`);
  assert.equal(res.status, 200);
  const html = await res.text();
  assert.match(html, /Cancel subscription/);
  assert.match(html, new RegExp(`/license/cancel\\?fp=${FP}`));
  assert.match(html, /Next payment/);

  const none = await call(`/license/portal?fp=${FP2}`);
  assert.match(await none.text(), /No subscription found/);
});

test("cancel: a paid subscription ends with the month already paid for", async () => {
  await env.HOTFIX_LICENSE.put(`lic:${FP}`, JSON.stringify({ subscription_id: "sub_1", status: "active" }));
  rzpReplies["POST /v1/subscriptions/sub_1/cancel"] = sub();
  const res = await call(`/license/cancel?fp=${FP}`, { method: "POST", headers: { Origin: "https://hotfix.buildcraft.town" } });
  assert.equal(res.status, 200);
  assert.deepEqual(rzpCalls[0].body, { cancel_at_cycle_end: true });
  assert.match(await res.text(), /stays unlocked on this computer until/);
  assert.equal((await verifyClaims(FP)).plan, "subscription", "still unlocked for the paid month");

  // The manage page (and later webhooks) keep the cancellation on record.
  rzpReplies["GET /v1/subscriptions/sub_1"] = sub();
  await call("/license/webhook", signedWebhook(subEvent("subscription.updated")));
  const html = await (await call(`/license/portal?fp=${FP}`)).text();
  assert.match(html, /Cancellation scheduled/);
  assert.doesNotMatch(html, /Cancel subscription/);
});

test("cancel: before the first charge it cancels now; trial days still apply", async () => {
  await env.HOTFIX_LICENSE.put(`lic:${FP}`, JSON.stringify({ subscription_id: "sub_1", status: "authenticated" }));
  rzpReplies["POST /v1/subscriptions/sub_1/cancel"] = sub({ status: "cancelled", current_end: null });
  const res = await call(`/license/cancel?fp=${FP}`, { method: "POST" });
  assert.deepEqual(rzpCalls[0].body, { cancel_at_cycle_end: false });
  assert.match(await res.text(), /free-trial days left still apply/);
  await trialStarted(FP, 2);
  assert.equal((await verifyClaims(FP)).plan, "trial");
});

test("cancel: refuses cross-site posts and machines without a subscription", async () => {
  await env.HOTFIX_LICENSE.put(`lic:${FP}`, JSON.stringify({ subscription_id: "sub_1", status: "active" }));
  const res = await call(`/license/cancel?fp=${FP}`, { method: "POST", headers: { Origin: "https://evil.example" } });
  assert.equal(res.status, 403);
  const none = await call(`/license/cancel?fp=${FP2}`, { method: "POST" });
  assert.match(await none.text(), /No subscription found/);
  assert.equal(rzpCalls.length, 0);
});

// --- misc ---

test("errors never leak Razorpay details or secrets", async () => {
  // No canned reply -> the fake Razorpay returns an error.
  const res = await call(`/license/checkout?fp=${FP}`);
  assert.equal(res.status, 500);
  assert.equal(await res.text(), "License service error");
});

test("routing: unknown paths and wrong methods", async () => {
  assert.equal((await call("/license/nope")).status, 404);
  assert.equal((await call(`/license/verify?fp=${FP}`, { method: "POST" })).status, 405);
  assert.equal((await call("/license/webhook")).status, 405);
  assert.equal((await call(`/license/success?fp=${FP}`)).status, 405);
  assert.equal((await call(`/license/cancel?fp=${FP}`)).status, 405);
});

test("verifyRazorpaySignature: hex HMAC of the raw body", async () => {
  const body = '{"event":"subscription.activated"}';
  const good = hmac(WEBHOOK_SECRET, body);
  assert.equal(await verifyRazorpaySignature(body, good, WEBHOOK_SECRET), true);
  assert.equal(await verifyRazorpaySignature(body, good.toUpperCase(), WEBHOOK_SECRET), true);
  assert.equal(await verifyRazorpaySignature(body + " ", good, WEBHOOK_SECRET), false);
  assert.equal(await verifyRazorpaySignature(body, good, ""), false, "an unset secret must never verify");
  assert.equal(await verifyRazorpaySignature(body, "", WEBHOOK_SECRET), false);
});
