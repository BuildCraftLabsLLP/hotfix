// Runs the real licensing API (src/server-api.js) locally over HTTP, with D1 =
// node:sqlite and an in-memory KV, for end-to-end tests of hotfixd against it
// (the Server workflow's license job). No Razorpay: a paid license is seeded
// directly.
//
//   node worker/test/server-api-harness.mjs <port> <out-dir>
//
// Writes <out-dir>/public-key (base64) and <out-dir>/license-key (an HFXS key
// for a 1-server license). Env: TRIAL_DAYS (default 14).
import http from "node:http";
import { writeFileSync, readFileSync } from "node:fs";
import { generateKeyPairSync, createHash } from "node:crypto";
import { DatabaseSync } from "node:sqlite";
import api, { makeKey, normalizeKey } from "../src/server-api.js";

const [port = "8787", outDir = "."] = process.argv.slice(2);
const db = new DatabaseSync(":memory:");
db.exec(readFileSync(new URL("../migrations/0001_server_licensing.sql", import.meta.url), "utf8"));
const d1 = {
  prepare: (sql) => {
    const st = (args = []) => ({
      bind: (...a) => st(a),
      first: async () => db.prepare(sql).get(...args) ?? null,
      all: async () => ({ results: db.prepare(sql).all(...args) }),
      run: async () => ({ meta: { changes: Number(db.prepare(sql).run(...args).changes) } }),
    });
    return st();
  },
};
const kvMap = new Map();
const kv = {
  get: async (k, t) => (kvMap.has(k) ? (t === "json" ? JSON.parse(kvMap.get(k)) : kvMap.get(k)) : null),
  put: async (k, v) => void kvMap.set(k, v),
};

const { publicKey, privateKey } = generateKeyPairSync("ed25519");
const env = {
  HOTFIX_SERVER_DB: d1,
  HOTFIX_SERVER_KV: kv,
  SERVER_LICENSE_SIGNING_KEY: privateKey.export({ type: "pkcs8", format: "der" }).toString("base64"),
  TRIAL_DAYS: process.env.TRIAL_DAYS,
};
// No GitHub calls from CI: the latest-version lookup just fails quietly.
globalThis.fetch = async () => new Response("[]", { status: 503 });

// Seed a paid, 1-server license.
const key = makeKey("HFXS");
const now = Math.floor(Date.now() / 1000);
const sha = (s) => createHash("sha256").update(s).digest("hex");
db.prepare("INSERT INTO licenses (id, activation_hash, manage_hash, created) VALUES (?,?,?,?)")
  .run("lic_test", sha(normalizeKey(key, "HFXS")), sha(normalizeKey(makeKey("HFXM"), "HFXM")), now);
db.prepare("INSERT INTO subscriptions (id, license_id, status, quantity, current_end, created, updated) VALUES (?,?,?,?,?,?,?)")
  .run("sub_TEST000000000001", "lic_test", "active", 1, now + 30 * 86400, now, now);

writeFileSync(`${outDir}/public-key`, publicKey.export({ type: "spki", format: "der" }).subarray(-32).toString("base64"));
writeFileSync(`${outDir}/license-key`, key);

http
  .createServer(async (req, res) => {
    const chunks = [];
    for await (const c of req) chunks.push(c);
    const request = new Request(`http://127.0.0.1:${port}${req.url}`, {
      method: req.method,
      headers: { ...req.headers, "CF-Connecting-IP": "127.0.0.1" },
      body: ["GET", "HEAD"].includes(req.method) ? undefined : Buffer.concat(chunks),
    });
    const response = await api.fetch(request, env);
    res.writeHead(response.status, Object.fromEntries(response.headers));
    res.end(Buffer.from(await response.arrayBuffer()));
    console.log(`${req.method} ${req.url} -> ${response.status}`);
  })
  .listen(Number(port), "127.0.0.1", () => console.log(`license API on http://127.0.0.1:${port}/api/server`));
