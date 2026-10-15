// HTTP entry point for the licensing API on Railway: node src/server.js
//
// Env: PORT (set by Railway), DATABASE_URL (Railway Postgres), and the
// variables listed in app.js. Migrations run at startup. GET /healthz checks
// the database (Railway's healthcheck).
import http from "node:http";
import app, { RAZORPAY_VARS } from "./app.js";
import { postgres } from "./db.js";

// The Razorpay variables are all or nothing: none = billing off (trials only).
const rzpSet = RAZORPAY_VARS.filter((k) => process.env[k]);
const missing = ["DATABASE_URL", "SERVER_LICENSE_SIGNING_KEY", ...(rzpSet.length ? RAZORPAY_VARS : [])].filter((k) => !process.env[k]);
if (missing.length) {
  console.error(`license-api: missing environment variables: ${missing.join(", ")}`);
  process.exit(1);
}
if (!rzpSet.length) console.log("license-api: Razorpay not configured: billing off (trials and the machine API only)");

const store = await postgres(process.env.DATABASE_URL);
const env = { ...process.env, DB: store.db, KV: store.kv };
const port = Number(process.env.PORT || 8080);
const MAX_BODY = 256 << 10;

// The client's address. Railway's edge overwrites X-Real-IP with the
// connecting IP (clients can't forge it); X-Forwarded-For's last entry is the
// fallback. (license.buildcraft.town must be DNS-only, not proxied through
// Cloudflare, or every client would appear as a Cloudflare address.)
function clientIP(req) {
  const real = String(req.headers["x-real-ip"] || "").trim();
  if (real) return real;
  const xff = String(req.headers["x-forwarded-for"] || "").split(",").map((s) => s.trim()).filter(Boolean);
  return xff.at(-1) || req.socket.remoteAddress || "unknown";
}

const server = http.createServer(async (req, res) => {
  try {
    if (req.url === "/healthz") {
      await store.ping();
      res.writeHead(200, { "Content-Type": "text/plain" });
      return res.end("ok");
    }
    const chunks = [];
    let size = 0;
    for await (const c of req) {
      size += c.length;
      if (size > MAX_BODY) {
        res.writeHead(413);
        return res.end("Request too large");
      }
      chunks.push(c);
    }
    const headers = new Headers();
    for (const [k, v] of Object.entries(req.headers)) {
      if (k !== "x-client-ip") headers.set(k, Array.isArray(v) ? v.join(", ") : v);
    }
    headers.set("X-Client-IP", clientIP(req));
    const proto = req.headers["x-forwarded-proto"] || "http";
    const request = new Request(`${proto}://${req.headers.host || "localhost"}${req.url}`, {
      method: req.method,
      headers,
      body: ["GET", "HEAD"].includes(req.method) ? undefined : Buffer.concat(chunks),
    });
    const response = await app.fetch(request, env);
    res.writeHead(response.status, Object.fromEntries(response.headers));
    res.end(Buffer.from(await response.arrayBuffer()));
  } catch (err) {
    console.error("license-api:", err);
    if (!res.headersSent) res.writeHead(500);
    res.end("License service error");
  }
});

server.listen(port, () => console.log(`license-api listening on :${port}`));

const shutdown = () => server.close(() => store.close().then(() => process.exit(0)));
process.on("SIGTERM", shutdown);
process.on("SIGINT", shutdown);
