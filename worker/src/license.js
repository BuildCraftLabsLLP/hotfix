// Licensing API for the Linux app: hotfix.buildcraft.town/license/*
//
// The Linux build is a $1/month subscription locked to one machine. There are
// no license keys to share: a subscription is bound to the machine fingerprint
// the app sends (an opaque SHA-256 derived from /etc/machine-id), and the app
// asks this API whether *its* fingerprint has an active subscription.
//
//   GET  /license/verify?fp=<fp>    -> { token }  signed status for this machine
//   GET  /license/checkout?fp=<fp>  -> 303 to a Stripe Checkout page
//   GET  /license/success?session_id=…  post-payment landing page (activates)
//   GET  /license/portal?fp=<fp>    -> 303 to the Stripe billing portal
//   POST /license/webhook           Stripe webhook (signature-verified)
//
// State lives in Workers KV (binding HOTFIX_LICENSE) as `lic:<fp>`:
//   { subscription_id, customer_id, status, current_period_end, updated }
//
// Secrets (wrangler secret put …) — none of these ever reach the client:
//   STRIPE_SECRET_KEY      Stripe API key (sk_live_… / sk_test_…)
//   STRIPE_WEBHOOK_SECRET  signing secret of the /license/webhook endpoint (whsec_…)
//   LICENSE_SIGNING_KEY    Ed25519 private key (base64 PKCS#8) — see scripts/gen-license-key.mjs
// Optional var:
//   STRIPE_PRICE_ID        a recurring Price to sell; default is an inline $1/month price
//
// The verify response is a token signed with LICENSE_SIGNING_KEY; the app ships
// only the matching public key. A token names the fingerprint it was issued
// for, so it is useless on any other machine, and it can't be forged by
// redirecting the app to a fake server.

const FP_RE = /^[0-9a-f]{64}$/;
const TOKEN_TTL = 72 * 3600; // seconds a token stays valid (the app's offline grace)
const WEBHOOK_TOLERANCE = 300; // max age in seconds of a webhook signature
const ACTIVE = new Set(["active", "trialing"]);
const STRIPE_API = "https://api.stripe.com/v1";

export async function handleLicense(request, env, url) {
  const seg = url.pathname.replace(/^\/license\/?/, "").replace(/\/$/, "");
  try {
    if (seg === "webhook") {
      if (request.method !== "POST") return text("Method not allowed", 405);
      return await webhook(request, env);
    }
    if (request.method !== "GET") return text("Method not allowed", 405);
    switch (seg) {
      case "verify":
        return await verify(env, url);
      case "checkout":
        return await checkout(env, url);
      case "success":
        return await success(env, url);
      case "portal":
        return await portal(env, url);
      case "cancelled":
        return page("Checkout cancelled", "No payment was taken. You can subscribe any time from the Hotfix tray menu.");
      default:
        return text("Not found", 404);
    }
  } catch (err) {
    // Never leak internals (Stripe errors can echo request details).
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
  const claims = {
    fp,
    status: rec && ACTIVE.has(rec.status) ? "active" : "inactive",
    iat: now,
    exp: now + TOKEN_TTL,
    period_end: (rec && rec.current_period_end) || 0,
  };
  return new Response(JSON.stringify({ token: await signToken(env, claims) }), {
    headers: { "Content-Type": "application/json", "Cache-Control": "no-store" },
  });
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

async function checkout(env, url) {
  const fp = fingerprint(url);
  if (!fp) return text("Invalid fingerprint", 400);

  // Don't sell a second subscription for a machine that already has one.
  const rec = await env.HOTFIX_LICENSE.get(`lic:${fp}`, "json");
  if (rec && ACTIVE.has(rec.status)) {
    return page("Already subscribed", "This computer already has an active Hotfix subscription. Choose “Refresh License” in the Hotfix tray menu.");
  }

  const params = {
    mode: "subscription",
    client_reference_id: fp,
    "metadata[fingerprint]": fp,
    // Copied onto the Subscription, which is how later webhook events
    // (renewals, cancellations) find their machine.
    "subscription_data[metadata][fingerprint]": fp,
    "line_items[0][quantity]": "1",
    success_url: `${url.origin}/license/success?session_id={CHECKOUT_SESSION_ID}`,
    cancel_url: `${url.origin}/license/cancelled`,
  };
  if (env.STRIPE_PRICE_ID) {
    params["line_items[0][price]"] = env.STRIPE_PRICE_ID;
  } else {
    params["line_items[0][price_data][currency]"] = "usd";
    params["line_items[0][price_data][unit_amount]"] = "100"; // $1.00
    params["line_items[0][price_data][recurring][interval]"] = "month";
    params["line_items[0][price_data][product_data][name]"] = "Hotfix for Linux (1 computer)";
  }

  const session = await stripe(env, "POST", "/checkout/sessions", params);
  return Response.redirect(session.url, 303);
}

// Landing page after payment. Activates immediately from the Checkout Session
// instead of waiting for the webhook (which remains the source of truth for
// renewals and cancellations). The session id is an unguessable Stripe token.
async function success(env, url) {
  const id = url.searchParams.get("session_id") || "";
  if (!/^cs_[A-Za-z0-9_]+$/.test(id)) return text("Invalid session", 400);

  const session = await stripe(env, "GET", `/checkout/sessions/${id}?expand[]=subscription`);
  const fp = session.client_reference_id;
  if (session.status !== "complete" || !session.subscription || !FP_RE.test(fp || "")) {
    return page("Payment not completed", "This checkout wasn't completed, so nothing was activated. You can subscribe from the Hotfix tray menu.");
  }
  await saveLicense(env, fp, session.subscription);
  return page("You're subscribed 🎉", "Thanks! Hotfix on this computer unlocks automatically within a few seconds — you can close this tab.");
}

// --- billing portal ---

async function portal(env, url) {
  const fp = fingerprint(url);
  if (!fp) return text("Invalid fingerprint", 400);

  const rec = await env.HOTFIX_LICENSE.get(`lic:${fp}`, "json");
  if (!rec || !rec.customer_id) {
    return page("No subscription found", "This computer has no Hotfix subscription to manage.");
  }
  const session = await stripe(env, "POST", "/billing_portal/sessions", {
    customer: rec.customer_id,
    return_url: `${url.origin}/`,
  });
  return Response.redirect(session.url, 303);
}

// --- webhook ---

async function webhook(request, env) {
  const body = await request.text();
  const ok = await verifyStripeSignature(
    body,
    request.headers.get("Stripe-Signature") || "",
    env.STRIPE_WEBHOOK_SECRET,
    Math.floor(Date.now() / 1000),
  );
  if (!ok) return text("Bad signature", 400);

  const event = JSON.parse(body);
  const obj = event.data && event.data.object;

  switch (event.type) {
    case "checkout.session.completed": {
      const fp = obj.client_reference_id;
      if (obj.mode === "subscription" && obj.subscription && FP_RE.test(fp || "")) {
        const sub = await stripe(env, "GET", `/subscriptions/${obj.subscription}`);
        await saveLicense(env, fp, sub);
      }
      break;
    }
    case "customer.subscription.created":
    case "customer.subscription.updated":
    case "customer.subscription.deleted": {
      const fp = obj.metadata && obj.metadata.fingerprint;
      if (FP_RE.test(fp || "")) await saveLicense(env, fp, obj);
      break;
    }
  }
  return text("ok", 200);
}

// Stripe-Signature: t=<unix>,v1=<hex hmac>[,v1=…]. The signed payload is
// "<t>.<raw body>", HMAC-SHA256 with the endpoint's signing secret.
export async function verifyStripeSignature(body, header, secret, now) {
  if (!secret) return false;
  let t = "";
  const sigs = [];
  for (const part of header.split(",")) {
    const i = part.indexOf("=");
    if (i < 0) continue;
    const k = part.slice(0, i).trim();
    const v = part.slice(i + 1).trim();
    if (k === "t") t = v;
    else if (k === "v1") sigs.push(v);
  }
  if (!/^\d+$/.test(t) || sigs.length === 0) return false;
  if (Math.abs(now - parseInt(t, 10)) > WEBHOOK_TOLERANCE) return false;

  const key = await crypto.subtle.importKey(
    "raw",
    new TextEncoder().encode(secret),
    { name: "HMAC", hash: "SHA-256" },
    false,
    ["sign"],
  );
  const mac = new Uint8Array(await crypto.subtle.sign("HMAC", key, new TextEncoder().encode(`${t}.${body}`)));
  const expected = [...mac].map((b) => b.toString(16).padStart(2, "0")).join("");
  return sigs.some((s) => timingSafeEqual(s, expected));
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
// (e.g. the user cancelled, then re-subscribed).
async function saveLicense(env, fp, sub) {
  const key = `lic:${fp}`;
  const prev = await env.HOTFIX_LICENSE.get(key, "json");
  if (prev && prev.subscription_id !== sub.id && ACTIVE.has(prev.status) && !ACTIVE.has(sub.status)) {
    return;
  }
  const item = sub.items && sub.items.data && sub.items.data[0];
  await env.HOTFIX_LICENSE.put(
    key,
    JSON.stringify({
      subscription_id: sub.id,
      customer_id: typeof sub.customer === "string" ? sub.customer : sub.customer && sub.customer.id,
      status: sub.status,
      // Newer Stripe API versions report the period on the subscription item.
      current_period_end: sub.current_period_end || (item && item.current_period_end) || 0,
      updated: Math.floor(Date.now() / 1000),
    }),
  );
}

// --- helpers ---

function fingerprint(url) {
  const fp = url.searchParams.get("fp") || "";
  return FP_RE.test(fp) ? fp : null;
}

// Minimal Stripe REST client: form-encoded params, JSON back.
async function stripe(env, method, path, params) {
  const init = {
    method,
    headers: { Authorization: `Bearer ${env.STRIPE_SECRET_KEY}` },
  };
  if (params) {
    init.headers["Content-Type"] = "application/x-www-form-urlencoded";
    init.body = new URLSearchParams(params).toString();
  }
  const res = await fetch(STRIPE_API + path, init);
  const data = await res.json();
  if (!res.ok) {
    throw new Error(`stripe ${method} ${path.split("?")[0]}: ${res.status} ${(data.error && data.error.message) || ""}`);
  }
  return data;
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

function page(title, message) {
  const esc = (s) => s.replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
  const html = `<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex">
<title>Hotfix — ${esc(title)}</title>
<style>
  body { margin: 0; min-height: 100vh; display: grid; place-items: center;
         font: 16px/1.5 system-ui, sans-serif; background: #14110f; color: #f3ece6; }
  main { max-width: 28rem; padding: 2rem; text-align: center; }
  h1 { font-size: 1.5rem; margin: 0 0 .75rem; }
  p { margin: 0; color: #c9bdb3; }
</style></head>
<body><main><h1>${esc(title)}</h1><p>${esc(message)}</p></main></body></html>`;
  return new Response(html, {
    headers: { "Content-Type": "text/html; charset=utf-8", "Cache-Control": "no-store" },
  });
}
