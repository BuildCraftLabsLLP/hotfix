-- Hotfix Server licensing (src/app.js). Applied at startup by src/db.js.

-- A license = one activation key (deployed to servers) + one manage key
-- (billing). Only SHA-256 hashes of the keys are stored.
CREATE TABLE licenses (
  id              TEXT PRIMARY KEY,           -- lic_<random>
  activation_hash TEXT NOT NULL UNIQUE,
  manage_hash     TEXT NOT NULL UNIQUE,
  created         BIGINT NOT NULL
);

-- Razorpay subscriptions. Seats = sum of quantity over a license's live
-- subscriptions. A license can have several (adding servers to a UPI or
-- e-mandate subscription means buying a second one: those can't be updated).
CREATE TABLE subscriptions (
  id                 TEXT PRIMARY KEY,        -- sub_… (Razorpay)
  license_id         TEXT REFERENCES licenses (id),   -- set once paid
  pending_license_id TEXT REFERENCES licenses (id),   -- "add servers to this license" checkouts
  status             TEXT NOT NULL,           -- Razorpay status
  quantity           INTEGER NOT NULL,
  current_end        BIGINT,                  -- end of the paid period (unix)
  cancel_scheduled   INTEGER NOT NULL DEFAULT 0,
  created            BIGINT NOT NULL,
  updated            BIGINT NOT NULL
);
CREATE INDEX subscriptions_license ON subscriptions (license_id);

-- A seat is a lease held by one server (instance_id), renewed by every
-- license refresh; an expired lease frees the seat by itself.
CREATE TABLE seats (
  id          TEXT PRIMARY KEY,               -- seat_<random>
  license_id  TEXT NOT NULL REFERENCES licenses (id),
  secret_hash TEXT NOT NULL,                  -- the server refreshes with seat id + secret
  fingerprint TEXT NOT NULL,
  instance_id TEXT NOT NULL,
  hostname    TEXT,
  os          TEXT,
  version     TEXT,
  boot_id     TEXT,
  created     BIGINT NOT NULL,
  last_seen   BIGINT NOT NULL,
  lease_until BIGINT NOT NULL,
  UNIQUE (license_id, instance_id)
);
CREATE INDEX seats_lease ON seats (license_id, lease_until);

-- Small key/value store: trials (trial:<fingerprint>, never expire) and
-- counters (per-network trial caps, rate limits) with an expiry.
CREATE TABLE kv (
  key     TEXT PRIMARY KEY,
  value   TEXT NOT NULL,
  expires BIGINT                              -- unix seconds; NULL = never
);
CREATE INDEX kv_expires ON kv (expires);
