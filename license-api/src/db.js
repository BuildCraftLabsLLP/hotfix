// Database access for the licensing API, over PostgreSQL: node-postgres in
// production (Railway) and CI, PGlite (Postgres in WebAssembly) in local tests.
//
// app.js uses a small interface:
//   db.prepare(sql).bind(...args).first() | .all() | .run()   ("?" placeholders)
//   db.transaction(async (tx) => …)    tx has the same prepare(); one connection
//   kv.get(key, "json"?) / kv.putIfAbsent(key, value) / kv.incr(key, ttlSeconds)

import { readdirSync, readFileSync } from "node:fs";

const MIGRATIONS = new URL("../migrations/", import.meta.url);

// "?" → "$1, $2, …" (our SQL has no "?" inside string literals).
function numbered(sql) {
  let n = 0;
  return sql.replace(/\?/g, () => `$${++n}`);
}

// statements wraps a query function (text, params) → {rows, rowCount}.
function statements(query) {
  return {
    prepare(sql) {
      const text = numbered(sql);
      const stmt = (args = []) => ({
        bind: (...a) => stmt(a),
        first: async () => (await query(text, args)).rows[0] ?? null,
        all: async () => ({ results: (await query(text, args)).rows }),
        run: async () => ({ meta: { changes: (await query(text, args)).rowCount } }),
      });
      return stmt();
    },
  };
}

function kvOver(query) {
  const now = () => Math.floor(Date.now() / 1000);
  return {
    async get(key, type) {
      const r = await query("SELECT value FROM kv WHERE key = $1 AND (expires IS NULL OR expires > $2)", [key, now()]);
      if (!r.rows.length) return null;
      return type === "json" ? JSON.parse(r.rows[0].value) : r.rows[0].value;
    },
    // Stores value unless the key exists; returns the stored value (JSON).
    async putIfAbsent(key, value) {
      await query("INSERT INTO kv (key, value) VALUES ($1, $2) ON CONFLICT (key) DO NOTHING", [key, JSON.stringify(value)]);
      return this.get(key, "json");
    },
    // Atomically increments a counter that expires ttl seconds after it was
    // created, and returns the new count.
    async incr(key, ttl) {
      const t = now();
      await query("DELETE FROM kv WHERE key = $1 AND expires <= $2", [key, t]);
      const r = await query(
        `INSERT INTO kv (key, value, expires) VALUES ($1, '1', $2)
         ON CONFLICT (key) DO UPDATE SET value = (kv.value::bigint + 1)::text
         RETURNING value`,
        [key, t + ttl],
      );
      return Number(r.rows[0].value);
    },
  };
}

async function migrate(query) {
  await query("CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied BIGINT NOT NULL)");
  const done = new Set((await query("SELECT name FROM schema_migrations")).rows.map((r) => r.name));
  for (const name of readdirSync(MIGRATIONS).filter((f) => f.endsWith(".sql")).sort()) {
    if (done.has(name)) continue;
    await query("BEGIN");
    try {
      await query(readFileSync(new URL(name, MIGRATIONS), "utf8"));
      await query("INSERT INTO schema_migrations (name, applied) VALUES ($1, $2)", [name, Math.floor(Date.now() / 1000)]);
      await query("COMMIT");
    } catch (err) {
      await query("ROLLBACK");
      throw new Error(`migration ${name}: ${err.message}`);
    }
  }
}

// postgres connects with node-postgres (DATABASE_URL), runs migrations and
// returns {db, kv, close}.
export async function postgres(connectionString) {
  const { default: pg } = await import("pg");
  pg.types.setTypeParser(20, (v) => Number(v)); // BIGINT → number (unix times, counts)
  const ssl = /sslmode=(require|verify)/.test(connectionString) ? { rejectUnauthorized: false } : undefined;
  const pool = new pg.Pool({ connectionString, max: 10, ssl });
  const query = (text, params) => pool.query(text, params);
  // Migrations run on one connection (multi-statement SQL, BEGIN/COMMIT).
  const client = await pool.connect();
  try {
    await migrate((t, p) => client.query(t, p));
  } finally {
    client.release();
  }
  return {
    db: {
      ...statements(query),
      async transaction(fn) {
        const c = await pool.connect();
        try {
          await c.query("BEGIN");
          const out = await fn(statements((t, p) => c.query(t, p)));
          await c.query("COMMIT");
          return out;
        } catch (err) {
          await c.query("ROLLBACK").catch(() => {});
          throw err;
        } finally {
          c.release();
        }
      },
    },
    kv: kvOver(query),
    ping: () => pool.query("SELECT 1"),
    close: () => pool.end(),
  };
}

// pglite runs an in-memory Postgres (tests, local harness).
export async function pglite() {
  const { PGlite } = await import("@electric-sql/pglite");
  const pg = new PGlite({ parsers: { 20: (v) => Number(v) } });
  const query = async (text, params) => {
    if (!params && /;\s*\S/.test(text)) {
      await pg.exec(text); // multi-statement migration
      return { rows: [], rowCount: 0 };
    }
    const r = await pg.query(text, params ?? []);
    return { rows: r.rows, rowCount: r.affectedRows ?? r.rows.length };
  };
  await migrate(query);
  // PGlite has one connection: serialize transactions so they don't interleave.
  let chain = Promise.resolve();
  return {
    db: {
      ...statements(query),
      transaction(fn) {
        const run = chain.then(() =>
          pg.transaction((tx) =>
            fn(
              statements(async (t, p) => {
                const r = await tx.query(t, p ?? []);
                return { rows: r.rows, rowCount: r.affectedRows ?? r.rows.length };
              }),
            ),
          ),
        );
        chain = run.catch(() => {});
        return run;
      },
    },
    kv: kvOver(query),
    ping: () => pg.query("SELECT 1"),
    close: () => pg.close(),
    raw: pg,
  };
}
