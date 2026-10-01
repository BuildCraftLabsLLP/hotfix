// Generates the Ed25519 key pair used to sign license tokens.
//
//   node scripts/gen-license-key.mjs
//
// PRIVATE key -> Cloudflare secret (never commit it, never put it in the app):
//   wrangler secret put LICENSE_SIGNING_KEY
// PUBLIC key  -> GitHub repo variable LICENSE_PUBLIC_KEY (Settings → Secrets and
//   variables → Actions → Variables); CI compiles it into the Linux binary.
//
// Rotating the key invalidates every installed build until it updates, so
// generate it once and keep the private key backed up somewhere safe.
import { generateKeyPairSync } from "node:crypto";

const { publicKey, privateKey } = generateKeyPairSync("ed25519");

// The raw 32-byte public key is the tail of the SPKI encoding.
const pub = publicKey.export({ type: "spki", format: "der" }).subarray(-32);
const priv = privateKey.export({ type: "pkcs8", format: "der" });

console.log("LICENSE_PUBLIC_KEY  (GitHub Actions variable, safe to share):");
console.log(pub.toString("base64"));
console.log();
console.log("LICENSE_SIGNING_KEY (Cloudflare secret — keep private):");
console.log(priv.toString("base64"));
