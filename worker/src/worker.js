// Counting download redirect for hotfix.buildcraft.town/dl/*
//
// Fresh installs come through the website's Download buttons, which point at
// /dl/mac and /dl/win. This Worker bumps a per-platform counter in Workers KV,
// then 302s to the *latest* GitHub release asset (resolved live via the API,
// so the site never needs per-release version bumps).
//
// The app's silent auto-updater fetches browser_download_url directly and never
// touches /dl, so these counters approximate INSTALLS, not updates. It's a
// fuzzy proxy by design: it can't tell a brand-new user from an existing user
// re-downloading from the site, and KV's read-modify-write can drop the odd
// concurrent increment. Good enough to separate installs from update traffic.

const REPO = "buildcraftlabs/hotfix";
const LATEST_API = `https://api.github.com/repos/${REPO}/releases/latest`;
const ASSET_TTL = 300; // seconds to cache the resolved asset URL

// platform -> predicate over the release asset name
const PICKERS = {
  mac: (name) => name.endsWith(".dmg"),
  win: (name) => name.startsWith("Hotfix-Setup-") && name.endsWith(".exe"),
};

export default {
  async fetch(request, env, ctx) {
    const url = new URL(request.url);
    const seg = url.pathname.replace(/^\/dl\/?/, "").replace(/\/$/, "");

    if (seg === "stats") return stats(request, env, url);

    const pick = PICKERS[seg];
    if (!pick) return new Response("Not found", { status: 404 });

    const target = await latestAsset(env, seg, pick);
    if (!target) return new Response("No release asset found", { status: 502 });

    // Count asynchronously so the redirect is instant.
    ctx.waitUntil(bump(env, seg));
    return Response.redirect(target, 302);
  },
};

// Resolve (and briefly cache) the latest release asset download URL.
async function latestAsset(env, platform, pick) {
  const cache = caches.default;
  const cacheKey = new Request(`https://dl-cache/${platform}`);

  const hit = await cache.match(cacheKey);
  if (hit) return (await hit.text()) || null;

  const headers = {
    "User-Agent": "hotfix-dl-worker",
    Accept: "application/vnd.github+json",
  };
  if (env.GITHUB_TOKEN) headers.Authorization = `Bearer ${env.GITHUB_TOKEN}`;

  const res = await fetch(LATEST_API, { headers });
  if (!res.ok) return null;

  const data = await res.json();
  const asset = (data.assets || []).find((a) => pick(a.name));
  const dlUrl = asset ? asset.browser_download_url : "";

  await cache.put(
    cacheKey,
    new Response(dlUrl, { headers: { "Cache-Control": `max-age=${ASSET_TTL}` } }),
  );
  return dlUrl || null;
}

// Increment a lifetime counter plus a per-day bucket. KV is eventually
// consistent, so concurrent bumps can race and lose a count — acceptable here.
async function bump(env, platform) {
  const day = new Date().toISOString().slice(0, 10); // YYYY-MM-DD (UTC)
  for (const key of [`count:${platform}`, `count:${platform}:${day}`]) {
    const cur = parseInt((await env.HOTFIX_DL.get(key)) || "0", 10);
    await env.HOTFIX_DL.put(key, String(cur + 1));
  }
}

// GET /dl/stats?key=<STATS_TOKEN> -> JSON of all counters.
async function stats(request, env, url) {
  if (!env.STATS_TOKEN || url.searchParams.get("key") !== env.STATS_TOKEN) {
    return new Response("Unauthorized", { status: 401 });
  }
  const out = {};
  let cursor;
  do {
    const list = await env.HOTFIX_DL.list({ prefix: "count:", cursor });
    for (const k of list.keys) out[k.name] = parseInt((await env.HOTFIX_DL.get(k.name)) || "0", 10);
    cursor = list.list_complete ? undefined : list.cursor;
  } while (cursor);
  return new Response(JSON.stringify(out, null, 2), {
    headers: { "Content-Type": "application/json" },
  });
}
