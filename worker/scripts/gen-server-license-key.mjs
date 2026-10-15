// Generates the Ed25519 key pair that signs Hotfix Server license tokens.
//
//   node scripts/gen-server-license-key.mjs
//
// PRIVATE key -> Cloudflare secret of the server API Worker (never commit it):
//   wrangler secret put SERVER_LICENSE_SIGNING_KEY -c wrangler.server.toml
// PUBLIC key  -> GitHub repo variable SERVER_LICENSE_PUBLIC_KEY (Settings →
//   Secrets and variables → Actions → Variables); CI compiles it into hotfixd.
//
// Rotating the key invalidates every installed hotfixd until it updates, so
// generate it once and back the private key up somewhere safe.
import { generateKeyPairSync } from "node:crypto";

const { publicKey, privateKey } = generateKeyPairSync("ed25519");

// The raw 32-byte public key is the tail of the SPKI encoding.
const pub = publicKey.export({ type: "spki", format: "der" }).subarray(-32);
const priv = privateKey.export({ type: "pkcs8", format: "der" });

console.log("SERVER_LICENSE_PUBLIC_KEY  (GitHub Actions variable, safe to share):");
console.log(pub.toString("base64"));
console.log();
console.log("SERVER_LICENSE_SIGNING_KEY (Cloudflare secret, keep private):");
console.log(priv.toString("base64"));
