// Runs the real licensing API (src/app.js) locally over HTTP for end-to-end
// tests of hotfixd (the Server workflow's license-e2e job). The database is
// Postgres: TEST_DATABASE_URL if set, else in-memory PGlite. No Razorpay: a
// paid 1-server license is seeded directly.
//
//   node test/harness.mjs <port> <out-dir>
//
// Writes <out-dir>/public-key (base64) and <out-dir>/license-key (HFXS-…).
// Env: TRIAL_DAYS (default 14).
import http from "node:http";
import { writeFileSync } from "node:fs";
import { generateKeyPairSync, createHash } from "node:crypto";
import api, { makeKey, normalizeKey } from "../src/app.js";
import { pglite, postgres } from "../src/db.js";

const [port = "8787", outDir = "."] = process.argv.slice(2);
const store = process.env.TEST_DATABASE_URL ? await postgres(process.env.TEST_DATABASE_URL) : await pglite();

const { publicKey, privateKey } = generateKeyPairSync("ed25519");
const env = {
  DB: store.db,
  KV: store.kv,
  SERVER_LICENSE_SIGNING_KEY: privateKey.export({ type: "pkcs8", format: "der" }).toString("base64"),
  TRIAL_DAYS: process.env.TRIAL_DAYS,
};
// No GitHub calls from CI: the latest-version lookup just fails quietly.
globalThis.fetch = async () => new Response("[]", { status: 503 });

// Seed a paid, 1-server license.
const key = makeKey("HFXS");
const now = Math.floor(Date.now() / 1000);
const sha = (s) => createHash("sha256").update(s).digest("hex");
await store.db
  .prepare("INSERT INTO licenses (id, activation_hash, manage_hash, created) VALUES (?,?,?,?)")
  .bind("lic_test", sha(normalizeKey(key, "HFXS")), sha(normalizeKey(makeKey("HFXM"), "HFXM")), now)
  .run();
await store.db
  .prepare("INSERT INTO subscriptions (id, license_id, status, quantity, current_end, created, updated) VALUES (?,?,?,?,?,?,?)")
  .bind("sub_TEST000000000001", "lic_test", "active", 1, now + 30 * 86400, now, now)
  .run();

writeFileSync(`${outDir}/public-key`, publicKey.export({ type: "spki", format: "der" }).subarray(-32).toString("base64"));
writeFileSync(`${outDir}/license-key`, key);

http
  .createServer(async (req, res) => {
    const chunks = [];
    for await (const c of req) chunks.push(c);
    const request = new Request(`http://127.0.0.1:${port}${req.url}`, {
      method: req.method,
      headers: { ...req.headers, "X-Client-IP": "127.0.0.1" },
      body: ["GET", "HEAD"].includes(req.method) ? undefined : Buffer.concat(chunks),
    });
    const response = await api.fetch(request, env);
    res.writeHead(response.status, Object.fromEntries(response.headers));
    res.end(Buffer.from(await response.arrayBuffer()));
    console.log(`${req.method} ${req.url} -> ${response.status}`);
  })
  .listen(Number(port), "127.0.0.1", () => console.log(`license API on http://127.0.0.1:${port}/api/server`));
