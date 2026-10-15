// Counting download redirect for hotfix.buildcraft.town/dl/*
//
// Fresh installs come through the website's Download buttons, which point at
// /dl/mac and /dl/win. This Worker bumps a per-platform counter in Workers KV,
// then 302s to the *latest* GitHub release asset (resolved live via the API,
// so the site never needs per-release version bumps).
//
// GitHub rate-limits unauthenticated API calls per IP, and Cloudflare's IPs are
// shared, so the API often refuses (set the GITHUB_TOKEN secret to get our own
// limit). When it does, the latest tag is read from the github.com
// /releases/latest redirect instead (not an API call) and the asset URL is
// built from the release's file naming. If even that fails, the visitor goes
// to the release page rather than getting an error.
//
// The app's silent auto-updater fetches browser_download_url directly and never
// touches /dl, so these counters approximate INSTALLS, not updates. It's a
// fuzzy proxy by design: it can't tell a brand-new user from an existing user
// re-downloading from the site, and KV's read-modify-write can drop the odd
// concurrent increment. Good enough to separate installs from update traffic.

const REPO = "BuildCraftLabsLLP/hotfix";
const LATEST_API = `https://api.github.com/repos/${REPO}/releases/latest`;
const LATEST_PAGE = `https://github.com/${REPO}/releases/latest`;
const ASSET_TTL = 300; // seconds to cache the resolved asset URL

// platform -> how to find its asset: `pick` matches a name in the API's asset
// list; `name` builds the file name for a release tag (the CI naming, used when
// the API can't be asked).
const PLATFORMS = {
  mac: {
    pick: (n) => n.endsWith(".dmg"),
    name: (tag) => `Hotfix-${tag}-macOS.dmg`,
  },
  win: {
    pick: (n) => n.startsWith("Hotfix-Setup-") && n.endsWith(".exe"),
    name: (tag) => `Hotfix-Setup-${tag}-Windows.exe`,
  },
  linux: { // double-click installer (Ubuntu/Debian)
    pick: (n) => n.endsWith("-Linux-x86_64.deb"),
    name: (tag) => `Hotfix-${tag}-Linux-x86_64.deb`,
  },
  "linux-rpm": { // Fedora/openSUSE
    pick: (n) => n.endsWith("-Linux-x86_64.rpm"),
    name: (tag) => `Hotfix-${tag}-Linux-x86_64.rpm`,
  },
  "linux-tar": { // any distro, per-user install.sh
    pick: (n) => n.endsWith("-Linux-x86_64.tar.gz"),
    name: (tag) => `Hotfix-${tag}-Linux-x86_64.tar.gz`,
  },
  "linux-arm64": {
    pick: (n) => n.endsWith("-Linux-arm64.tar.gz"),
    name: (tag) => `Hotfix-${tag}-Linux-arm64.tar.gz`,
  },
};

export default {
  async fetch(request, env, ctx) {
    const url = new URL(request.url);

    const seg = url.pathname.replace(/^\/dl\/?/, "").replace(/\/$/, "");

    if (seg === "stats") return stats(request, env, url);

    const platform = PLATFORMS[seg];
    if (!platform) return new Response("Not found", { status: 404 });

    // Last resort: the release page, where the file can be picked by hand.
    const target = (await latestAsset(env, seg, platform)) || LATEST_PAGE;

    // Count asynchronously so the redirect is instant.
    ctx.waitUntil(bump(env, seg));
    return Response.redirect(target, 302);
  },
};

// Resolve (and briefly cache) the latest release asset download URL, or null.
async function latestAsset(env, key, platform) {
  const cache = caches.default;
  const cacheKey = new Request(`https://dl-cache/${key}`);

  const hit = await cache.match(cacheKey);
  if (hit) return (await hit.text()) || null;

  const dlUrl = (await fromAPI(env, platform)) || (await fromLatestTag(platform));
  if (dlUrl) {
    await cache.put(
      cacheKey,
      new Response(dlUrl, { headers: { "Cache-Control": `max-age=${ASSET_TTL}` } }),
    );
  }
  return dlUrl;
}

// The asset's URL from the releases API, or null (rate-limited, down, or no
// matching asset).
async function fromAPI(env, platform) {
  const headers = {
    "User-Agent": "hotfix-dl-worker",
    Accept: "application/vnd.github+json",
  };
  if (env.GITHUB_TOKEN) headers.Authorization = `Bearer ${env.GITHUB_TOKEN}`;
  try {
    const res = await fetch(LATEST_API, { headers });
    if (!res.ok) return null;
    const data = await res.json();
    const asset = (data.assets || []).find((a) => platform.pick(a.name));
    return asset ? asset.browser_download_url : null;
  } catch {
    return null;
  }
}

// The asset's URL built from the latest tag, which github.com/…/releases/latest
// names in its redirect (a web page, so not subject to the API rate limit).
async function fromLatestTag(platform) {
  try {
    const res = await fetch(LATEST_PAGE, {
      redirect: "manual",
      headers: { "User-Agent": "hotfix-dl-worker" },
    });
    const m = (res.headers.get("Location") || "").match(/\/releases\/tag\/(v[0-9][\w.-]*)$/);
    if (!m) return null;
    return `https://github.com/${REPO}/releases/download/${m[1]}/${platform.name(m[1])}`;
  } catch {
    return null;
  }
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
